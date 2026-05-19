package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/collector"
	"github.com/clawies/hospital-agent-sidecar/internal/config"
	"github.com/clawies/hospital-agent-sidecar/internal/diagnosis"
	"github.com/clawies/hospital-agent-sidecar/internal/repair"
)

const (
	pollInterval       = 5 * time.Second
	graceAfterStart    = 30 * time.Second
	healthCheckEvery   = 30 * time.Second // functional health check interval
	healthCheckTimeout = 10 * time.Second
	unhealthyThreshold = 3 // consecutive failures before reporting degraded
)

type AgentStatus string

const (
	StatusActive   AgentStatus = "active"
	StatusFailed   AgentStatus = "failed"
	StatusInactive AgentStatus = "inactive"
	StatusUnknown  AgentStatus = "unknown"
	StatusStarting AgentStatus = "starting"
	StatusDegraded AgentStatus = "degraded" // process alive but not responding
)

type AgentState struct {
	Status      AgentStatus `json:"status"`
	LastCrashAt *time.Time  `json:"lastCrashAt,omitempty"`
	CrashCount  int         `json:"crashCount"`
	LastRepairs []string    `json:"lastRepairs,omitempty"`
}

// CrashEvent is emitted when a crash is detected and handled locally.
// The heartbeat goroutine picks this up and pushes it to the hospital
// for AI diagnosis; the hospital responds with additional repair commands.
type CrashEvent struct {
	Context      collector.CrashContext    `json:"context"`
	L0Diagnosis  diagnosis.DiagnosisResult `json:"l0Diagnosis"`
	LocalRepairs []repair.Result           `json:"localRepairs"`
	Timestamp    time.Time                 `json:"timestamp"`
}

type Watcher struct {
	cfg       *config.Config
	logger    *slog.Logger
	collector *collector.Collector
	diagnoser *diagnosis.Diagnoser
	repairer  *repair.Repairer

	mu            sync.RWMutex
	state         AgentState
	lastCrashEv   *CrashEvent
	startedAt     time.Time
	lastNRestarts int // track systemd NRestarts to detect auto-recovered crashes

	// Functional health check state
	healthFailures int  // consecutive gateway HTTP failures
	degradedSent   bool // already reported this degraded episode

	// Channel for notifying heartbeat of crash events
	CrashCh chan CrashEvent
}

func New(cfg *config.Config, logger *slog.Logger, coll *collector.Collector,
	diag *diagnosis.Diagnoser, rep *repair.Repairer) *Watcher {
	return &Watcher{
		cfg:           cfg,
		logger:        logger,
		collector:     coll,
		diagnoser:     diag,
		repairer:      rep,
		state:         AgentState{Status: StatusUnknown},
		startedAt:     time.Now(),
		lastNRestarts: -1, // -1 = not yet read, avoids false trigger on first poll
		CrashCh:       make(chan CrashEvent, 10),
	}
}

// Repairer exposes the repairer for hospital-commanded repairs.
func (w *Watcher) Repairer() *repair.Repairer {
	return w.repairer
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

// Start begins the background polling loop and functional health checker.
func (w *Watcher) Start(ctx context.Context) {
	go w.loop(ctx)
	go w.healthCheckLoop(ctx)
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
	nRestarts := w.checkNRestarts(ctx)

	w.mu.Lock()
	prev := w.state.Status
	prevRestarts := w.lastNRestarts
	w.state.Status = status
	w.lastNRestarts = nRestarts
	w.mu.Unlock()

	// Detect transition to failed state (systemd gave up restarting)
	if status == StatusFailed && prev != StatusFailed {
		w.logger.Warn("agent crash detected", "unit", w.cfg.SystemdUnit, "prev", prev)
		w.handleCrash(ctx)
		return
	}

	// Detect auto-recovered crash: unit is active but NRestarts incremented.
	// This catches crashes that systemd's Restart=always fixed before our poll.
	if nRestarts > prevRestarts && prevRestarts >= 0 && status == StatusActive {
		w.logger.Warn("agent crash detected (auto-recovered by systemd)",
			"unit", w.cfg.SystemdUnit,
			"nRestarts", nRestarts,
			"prevRestarts", prevRestarts,
		)
		w.handleAutoRecoveredCrash(ctx, nRestarts-prevRestarts)
		return
	}

	// Detect recovery from failed state
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

// checkNRestarts reads the NRestarts property from systemd.
// Returns -1 if the property can't be read.
func (w *Watcher) checkNRestarts(ctx context.Context) int {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(tctx, "systemctl", "--user", "show",
		w.cfg.SystemdUnit, "-p", "NRestarts", "--value").Output()
	if err != nil {
		return -1
	}

	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

// handleAutoRecoveredCrash handles crashes where systemd already restarted the
// unit (Restart=always). The gateway is back up, but we still collect context
// and report to the hospital for tracking. No local repairs needed.
func (w *Watcher) handleAutoRecoveredCrash(ctx context.Context, delta int) {
	if time.Since(w.startedAt) < graceAfterStart {
		w.logger.Info("within grace period, skipping auto-recovered crash handling")
		return
	}

	w.logger.Info("collecting crash context for auto-recovered crash")
	cc := w.collector.CollectCrash(ctx)

	l0diag := w.diagnoser.Diagnose(cc)

	// No repairs needed -- systemd already restarted. Create a synthetic success result.
	autoResult := repair.Result{
		Action:  "restart-gateway",
		Success: true,
		Output:  "auto-recovered by systemd (Restart=always)",
	}

	now := time.Now()
	crashEv := CrashEvent{
		Context:      cc,
		L0Diagnosis:  l0diag,
		LocalRepairs: []repair.Result{autoResult},
		Timestamp:    now,
	}

	w.mu.Lock()
	w.state.CrashCount += delta
	w.state.LastCrashAt = &now
	w.state.LastRepairs = []string{"restart-gateway(systemd)"}
	w.lastCrashEv = &crashEv
	w.mu.Unlock()

	// Push to hospital for AI diagnosis and tracking
	select {
	case w.CrashCh <- crashEv:
	default:
		w.logger.Warn("crash channel full, dropping auto-recovered event")
	}

	w.logger.Info("auto-recovered crash reported",
		"l0_diagnosis", l0diag.Summary,
		"systemd_restarts", delta,
	)
}

func (w *Watcher) handleCrash(ctx context.Context) {
	// Don't handle crashes during grace period after startup
	if time.Since(w.startedAt) < graceAfterStart {
		w.logger.Info("within grace period, skipping crash handling")
		return
	}

	// Reset repair attempts for new incident
	w.repairer.ResetAll()

	// Step 1: Collect crash context
	w.logger.Info("collecting crash context")
	cc := w.collector.CollectCrash(ctx)

	// Step 2: L0 pattern match for immediate obvious fixes
	w.logger.Info("running L0 pattern match")
	l0diag := w.diagnoser.Diagnose(cc)

	// Step 3: Execute L0 repairs immediately (don't wait for hospital)
	w.logger.Info("executing L0 repairs", "count", len(l0diag.Repairs))
	var results []repair.Result
	for _, action := range l0diag.Repairs {
		result := w.repairer.Execute(action)
		results = append(results, result)

		// If restart succeeded, wait grace period before continuing
		if action == "restart-gateway" && result.Success {
			w.logger.Info("restart succeeded, waiting grace period")
			time.Sleep(graceAfterStart)
			break
		}
	}

	// Step 4: Update state
	now := time.Now()
	crashEv := CrashEvent{
		Context:      cc,
		L0Diagnosis:  l0diag,
		LocalRepairs: results,
		Timestamp:    now,
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

	// Step 5: Push to hospital for AI diagnosis
	// The heartbeat goroutine picks this up, sends crash context to hospital,
	// hospital runs AI diagnosis (diagnoseLogs + planRepair), responds with
	// commands, and heartbeat executes them.
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
	w.logger.Info("local crash handling complete",
		"l0_diagnosis", l0diag.Summary,
		"repairs_total", len(results),
		"repairs_succeeded", succeeded,
		"awaiting_hospital_diagnosis", true,
	)
}

// --- Functional health check ---
// Detects when the gateway process is alive (systemd active) but non-functional
// (e.g., LLM provider down, event loop stuck, hung process).

func (w *Watcher) healthCheckLoop(ctx context.Context) {
	// Wait for grace period before starting health checks
	select {
	case <-ctx.Done():
		return
	case <-time.After(graceAfterStart):
	}

	w.logger.Info("functional health checker started", "interval", healthCheckEvery, "gateway", w.cfg.GatewayURL)

	ticker := time.NewTicker(healthCheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.functionalHealthCheck(ctx)
		}
	}
}

func (w *Watcher) functionalHealthCheck(ctx context.Context) {
	// Only check when systemd says the unit is active
	state := w.State()
	if state.Status != StatusActive {
		return
	}

	err := w.pingGateway()

	w.mu.Lock()
	if err != nil {
		w.healthFailures++
		failures := w.healthFailures
		alreadySent := w.degradedSent
		w.mu.Unlock()

		w.logger.Warn("gateway health check failed",
			"failures", failures,
			"threshold", unhealthyThreshold,
			"err", err,
		)

		if failures >= unhealthyThreshold && !alreadySent {
			w.handleDegraded(ctx, err)
		}
	} else {
		wasDegraded := w.healthFailures >= unhealthyThreshold
		w.healthFailures = 0
		w.degradedSent = false
		w.mu.Unlock()

		if wasDegraded {
			w.logger.Info("gateway recovered from degraded state")
		}
	}
}

func (w *Watcher) pingGateway() error {
	client := &http.Client{Timeout: healthCheckTimeout}
	resp, err := client.Get(w.cfg.GatewayURL)
	if err != nil {
		return fmt.Errorf("gateway unreachable: %w", err)
	}
	defer resp.Body.Close()

	// Any HTTP response means the gateway is at least responding.
	// Even 404/500 means the process is alive and handling requests.
	return nil
}

func (w *Watcher) handleDegraded(ctx context.Context, lastErr error) {
	if time.Since(w.startedAt) < graceAfterStart {
		return
	}

	w.mu.Lock()
	w.degradedSent = true
	w.state.Status = StatusDegraded
	w.mu.Unlock()

	w.logger.Warn("gateway is degraded: process alive but not responding",
		"unit", w.cfg.SystemdUnit,
		"gatewayURL", w.cfg.GatewayURL,
		"consecutiveFailures", w.healthFailures,
		"lastErr", lastErr,
	)

	// Collect context and report to hospital
	cc := w.collector.CollectCrash(ctx)
	l0diag := diagnosis.DiagnosisResult{
		Layer:      "L0",
		Summary:    fmt.Sprintf("Gateway degraded: process active but HTTP unreachable (%s)", lastErr),
		Repairs:    []string{"restart-gateway"},
		Confidence: 0.4,
	}

	// Try restart since the gateway is unresponsive
	w.repairer.ResetAll()
	result := w.repairer.Execute("restart-gateway")

	now := time.Now()
	crashEv := CrashEvent{
		Context:      cc,
		L0Diagnosis:  l0diag,
		LocalRepairs: []repair.Result{result},
		Timestamp:    now,
	}

	w.mu.Lock()
	w.state.CrashCount++
	w.state.LastCrashAt = &now
	if result.Success {
		w.state.LastRepairs = []string{"restart-gateway"}
	} else {
		w.state.LastRepairs = []string{"restart-gateway(FAIL)"}
	}
	w.lastCrashEv = &crashEv
	w.mu.Unlock()

	// Push to hospital for AI diagnosis
	select {
	case w.CrashCh <- crashEv:
	default:
		w.logger.Warn("crash channel full, dropping degraded event")
	}

	w.logger.Info("degraded state reported to hospital",
		"restart_success", result.Success,
	)
}
