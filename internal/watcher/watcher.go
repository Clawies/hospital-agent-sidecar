package watcher

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/clawies/hospital-agent/internal/collector"
	"github.com/clawies/hospital-agent/internal/config"
	"github.com/clawies/hospital-agent/internal/diagnosis"
	"github.com/clawies/hospital-agent/internal/repair"
)

const (
	pollInterval    = 5 * time.Second
	graceAfterStart = 30 * time.Second
)

type AgentStatus string

const (
	StatusActive   AgentStatus = "active"
	StatusFailed   AgentStatus = "failed"
	StatusInactive AgentStatus = "inactive"
	StatusUnknown  AgentStatus = "unknown"
	StatusStarting AgentStatus = "starting"
)

type AgentState struct {
	Status      AgentStatus `json:"status"`
	LastCrashAt *time.Time  `json:"lastCrashAt,omitempty"`
	CrashCount  int         `json:"crashCount"`
	LastRepairs []string    `json:"lastRepairs,omitempty"`
}

// CrashEvent is emitted when a crash is detected and handled.
type CrashEvent struct {
	Context   collector.CrashContext    `json:"context"`
	Diagnosis diagnosis.DiagnosisResult `json:"diagnosis"`
	Repairs   []repair.Result          `json:"repairs"`
	Timestamp time.Time                `json:"timestamp"`
}

type Watcher struct {
	cfg       *config.Config
	logger    *slog.Logger
	collector *collector.Collector
	diagnoser *diagnosis.Diagnoser
	repairer  *repair.Repairer

	mu          sync.RWMutex
	state       AgentState
	lastCrashEv *CrashEvent
	startedAt   time.Time

	// Channel for notifying heartbeat of crash events
	CrashCh chan CrashEvent
}

func New(cfg *config.Config, logger *slog.Logger, coll *collector.Collector,
	diag *diagnosis.Diagnoser, rep *repair.Repairer) *Watcher {
	return &Watcher{
		cfg:       cfg,
		logger:    logger,
		collector: coll,
		diagnoser: diag,
		repairer:  rep,
		state:     AgentState{Status: StatusUnknown},
		startedAt: time.Now(),
		CrashCh:   make(chan CrashEvent, 10),
	}
}

// State returns the current agent state (thread-safe).
func (w *Watcher) State() AgentState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

// LastCrashEvent returns the most recent crash event (or nil).
func (w *Watcher) LastCrashEvent() *CrashEvent {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastCrashEv
}

// Start begins the background polling loop.
func (w *Watcher) Start(ctx context.Context) {
	go w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) {
	w.logger.Info("watcher started", "unit", w.cfg.SystemdUnit, "poll_interval", pollInterval)

	// Initial check
	w.poll(ctx)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("watcher stopped")
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

func (w *Watcher) poll(ctx context.Context) {
	status := w.checkUnit(ctx)

	w.mu.Lock()
	prev := w.state.Status
	w.state.Status = status
	w.mu.Unlock()

	// Detect transition to failed state
	if status == StatusFailed && prev != StatusFailed {
		w.logger.Warn("agent crash detected", "unit", w.cfg.SystemdUnit, "prev", prev)
		w.handleCrash(ctx)
	}

	// Detect recovery
	if status == StatusActive && prev == StatusFailed {
		w.logger.Info("agent recovered", "unit", w.cfg.SystemdUnit)
	}
}

func (w *Watcher) checkUnit(ctx context.Context) AgentStatus {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(tctx, "systemctl", "--user", "is-active", w.cfg.SystemdUnit).Output()
	if err != nil {
		result := strings.TrimSpace(string(out))
		switch result {
		case "failed":
			return StatusFailed
		case "inactive":
			return StatusInactive
		case "activating":
			return StatusStarting
		default:
			return StatusUnknown
		}
	}

	result := strings.TrimSpace(string(out))
	if result == "active" {
		return StatusActive
	}
	return AgentStatus(result)
}

func (w *Watcher) handleCrash(ctx context.Context) {
	// Don't handle crashes during grace period after startup
	if time.Since(w.startedAt) < graceAfterStart {
		w.logger.Info("within grace period, skipping crash handling")
		return
	}

	// Reset repair attempts for new incident
	w.repairer.ResetAll()

	// Collect crash context
	w.logger.Info("collecting crash context")
	cc := w.collector.CollectCrash(ctx)

	// Diagnose (L1 -> L0 fallback)
	w.logger.Info("diagnosing crash")
	diag := w.diagnoser.Diagnose(ctx, cc)

	// Execute prescribed repairs
	w.logger.Info("executing repairs", "count", len(diag.Repairs), "layer", diag.Layer)
	var results []repair.Result
	for _, action := range diag.Repairs {
		result := w.repairer.Execute(action)
		results = append(results, result)

		// If restart succeeded, wait grace period before continuing
		if action == "restart-gateway" && result.Success {
			w.logger.Info("restart succeeded, waiting grace period")
			time.Sleep(graceAfterStart)
			break
		}
	}

	// Update state
	now := time.Now()
	crashEv := CrashEvent{
		Context:   cc,
		Diagnosis: diag,
		Repairs:   results,
		Timestamp: now,
	}

	w.mu.Lock()
	w.state.CrashCount++
	w.state.LastCrashAt = &now
	repairNames := make([]string, len(results))
	for i, r := range results {
		repairNames[i] = r.Action
		if !r.Success {
			repairNames[i] += "(FAIL)"
		}
	}
	w.state.LastRepairs = repairNames
	w.lastCrashEv = &crashEv
	w.mu.Unlock()

	// Notify heartbeat goroutine of crash event
	select {
	case w.CrashCh <- crashEv:
	default:
		w.logger.Warn("crash channel full, dropping event")
	}

	// Log summary
	succeeded := 0
	for _, r := range results {
		if r.Success {
			succeeded++
		}
	}
	w.logger.Info("crash handling complete",
		"diagnosis_layer", diag.Layer,
		"diagnosis", diag.Summary,
		"repairs_total", len(results),
		"repairs_succeeded", succeeded,
	)
}
