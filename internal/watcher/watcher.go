package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	healthFailures    int  // consecutive gateway HTTP failures
	degradedSent      bool // already reported this degraded episode
	diskAlertSent     bool // already sent disk warning this episode
	memoryAlertSent   bool // already sent memory warning this episode

	// LLM health check state
	llmFailures int  // consecutive LLM probe failures
	llmAlertSent bool // already reported this LLM-down episode

	// Session/cron snapshot tick counter (run every 20th healthCheck tick = ~10min)
	snapshotTicks int

	// Extra unit monitoring state
	extraUnitFailed map[string]bool // tracks failed state per extra unit

	// Channel for notifying heartbeat of crash events
	CrashCh chan CrashEvent

	// Channel for LLM status events (picked up by heartbeat for hospital + alerts)
	LLMStatusCh chan LLMStatusEvent

	// Channel for resource warnings (disk/memory) -- picked up by heartbeat for alerts
	ResourceCh chan ResourceEvent

	// Channel for integration status events
	IntegrationStatusCh chan []IntegrationStatusEvent

	// Channel for MCP status events
	MCPStatusCh chan []MCPStatusEvent

	// Latest snapshots (read by heartbeat for enriched payload)
	integrationsMu      sync.RWMutex
	latestIntegrations  []IntegrationStatusEvent
	latestMCPServers    []MCPStatusEvent
	latestSessions      *SessionSnapshot
	latestCronHealth    []CronSnapshot

	// Integration/MCP alert dedup
	integrationAlertSent map[string]int // consecutive failures per integration
	mcpAlertSent         map[string]int // consecutive failures per MCP server
}

// ResourceEvent represents a disk or memory warning.
type ResourceEvent struct {
	Resource    string `json:"resource"`    // "disk" or "memory"
	UsedPercent int    `json:"usedPercent"`
	Detail      string `json:"detail"`
}

// LLMStatusEvent represents an LLM provider health status change.
type LLMStatusEvent struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`   // "down", "auth_expired", "credits_exhausted", "provider_outage", "recovered"
	Category string `json:"category"` // maps to hospital incident category
	HTTPCode int    `json:"httpCode,omitempty"`
	Detail   string `json:"detail"`
	Endpoint string `json:"endpoint"`
}

// IntegrationStatusEvent represents a messaging integration probe result.
type IntegrationStatusEvent struct {
	Integration string `json:"integration"` // "slack", "discord", "telegram", "whatsapp"
	Connected   bool   `json:"connected"`
	LatencyMs   int    `json:"latencyMs,omitempty"`
	Error       string `json:"error,omitempty"`
	HTTPCode    int    `json:"httpCode,omitempty"`
}

// MCPStatusEvent represents an MCP server probe result.
type MCPStatusEvent struct {
	ServerName string `json:"serverName"`
	Transport  string `json:"transport"` // "stdio", "sse", "streamable-http"
	Alive      bool   `json:"alive"`
	Error      string `json:"error,omitempty"`
}

// SessionSnapshot captures session health data.
type SessionSnapshot struct {
	TotalCount       int    `json:"totalCount"`
	TotalSizeMb      int    `json:"totalSizeMb"`
	OldestTimestamp  string `json:"oldestTimestamp,omitempty"`
}

// CronSnapshot captures cron health data.
type CronSnapshot struct {
	ID         string `json:"id"`
	Schedule   string `json:"schedule"`
	LastRun    string `json:"lastRun,omitempty"`
	LastStatus string `json:"lastStatus"`
	LastError  string `json:"lastError,omitempty"`
	Overdue    bool   `json:"overdue"`
}

func New(cfg *config.Config, logger *slog.Logger, coll *collector.Collector,
	diag *diagnosis.Diagnoser, rep *repair.Repairer) *Watcher {
	return &Watcher{
		cfg:                  cfg,
		logger:               logger,
		collector:            coll,
		diagnoser:            diag,
		repairer:             rep,
		state:                AgentState{Status: StatusUnknown},
		startedAt:            time.Now(),
		lastNRestarts:        -1, // -1 = not yet read, avoids false trigger on first poll
		extraUnitFailed:      make(map[string]bool),
		CrashCh:              make(chan CrashEvent, 10),
		LLMStatusCh:          make(chan LLMStatusEvent, 10),
		ResourceCh:           make(chan ResourceEvent, 10),
		IntegrationStatusCh:  make(chan []IntegrationStatusEvent, 8),
		MCPStatusCh:          make(chan []MCPStatusEvent, 8),
		integrationAlertSent: make(map[string]int),
		mcpAlertSent:         make(map[string]int),
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
	if len(w.cfg.ExtraUnits) > 0 {
		go w.extraUnitsLoop(ctx)
	}
	go w.integrationHealthLoop(ctx)
	go w.mcpHealthLoop(ctx)
}

// LatestIntegrations returns the most recent integration probe results.
func (w *Watcher) LatestIntegrations() []IntegrationStatusEvent {
	w.integrationsMu.RLock()
	defer w.integrationsMu.RUnlock()
	return w.latestIntegrations
}

// LatestMCPServers returns the most recent MCP server probe results.
func (w *Watcher) LatestMCPServers() []MCPStatusEvent {
	w.integrationsMu.RLock()
	defer w.integrationsMu.RUnlock()
	return w.latestMCPServers
}

// LatestSessions returns the most recent session snapshot.
func (w *Watcher) LatestSessions() *SessionSnapshot {
	w.integrationsMu.RLock()
	defer w.integrationsMu.RUnlock()
	return w.latestSessions
}

// LatestCronHealth returns the most recent cron health snapshot.
func (w *Watcher) LatestCronHealth() []CronSnapshot {
	w.integrationsMu.RLock()
	defer w.integrationsMu.RUnlock()
	return w.latestCronHealth
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

	// Skip clean exits -- systemd NRestarts can increment on clean stop+start cycles
	if cc.ExitCode == 0 {
		w.logger.Info("clean exit detected in auto-recovered crash (exit code 0), skipping")
		return
	}

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

	// Skip clean exits (exit code 0) -- not a crash, just a normal shutdown.
	// systemd Restart=always will bring it back; no L0 repair or hospital report needed.
	if cc.ExitCode == 0 {
		w.logger.Info("clean exit detected (exit code 0), skipping crash handling",
			"unit", w.cfg.SystemdUnit,
		)
		return
	}

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

	checks := []string{}
	if w.cfg.GatewayURL != "" {
		checks = append(checks, "gateway="+w.cfg.GatewayURL)
	}
	if w.cfg.LLMHealthURL != "" {
		checks = append(checks, "llm="+w.cfg.LLMHealthURL)
	}
	if len(checks) == 0 {
		w.logger.Info("no health check URLs configured, health checker disabled")
		return
	}
	w.logger.Info("functional health checker started", "interval", healthCheckEvery, "checks", checks)

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
	if state.Status != StatusActive && state.Status != StatusDegraded {
		return
	}

	// Check 1: Gateway HTTP reachability (skip if not configured)
	if w.cfg.GatewayURL != "" {
		w.checkGatewayHealth(ctx)
	}

	// Check 2: LLM provider reachability (skip if not configured)
	if w.cfg.LLMHealthURL != "" {
		w.checkLLMHealth(ctx)
	}

	// Check 3: Resource warnings (disk/memory)
	w.checkResources()

	// Check 4: Session & cron snapshots (every ~10 minutes, not every 30s)
	w.snapshotTicks++
	if w.snapshotTicks >= 20 {
		w.snapshotTicks = 0
		w.collectSessionSnapshot()
		w.collectCronSnapshot()
	}
}

func (w *Watcher) checkResources() {
	sysInfo := w.collector.SystemInfo()

	// Disk warning at 90%
	if sysInfo.Disk.UsedPercent >= 90 {
		w.mu.Lock()
		alreadySent := w.diskAlertSent
		w.diskAlertSent = true
		w.mu.Unlock()

		if !alreadySent {
			w.logger.Warn("disk usage critical", "percent", sysInfo.Disk.UsedPercent, "availMB", sysInfo.Disk.AvailMB)
			select {
			case w.ResourceCh <- ResourceEvent{
				Resource:    "Disk",
				UsedPercent: sysInfo.Disk.UsedPercent,
				Detail:      fmt.Sprintf("%dMB free of %dMB total. Risk of crash if disk fills up.", sysInfo.Disk.AvailMB, sysInfo.Disk.TotalMB),
			}:
			default:
			}
		}
	} else if sysInfo.Disk.UsedPercent < 85 {
		w.mu.Lock()
		w.diskAlertSent = false
		w.mu.Unlock()
	}

	// Memory warning at 90%
	if sysInfo.Memory.UsedPercent >= 90 {
		w.mu.Lock()
		alreadySent := w.memoryAlertSent
		w.memoryAlertSent = true
		w.mu.Unlock()

		if !alreadySent {
			w.logger.Warn("memory usage critical", "percent", sysInfo.Memory.UsedPercent, "availMB", sysInfo.Memory.AvailMB)
			select {
			case w.ResourceCh <- ResourceEvent{
				Resource:    "Memory",
				UsedPercent: sysInfo.Memory.UsedPercent,
				Detail:      fmt.Sprintf("%dMB available of %dMB total. OOM kill risk.", sysInfo.Memory.AvailMB, sysInfo.Memory.TotalMB),
			}:
			default:
			}
		}
	} else if sysInfo.Memory.UsedPercent < 85 {
		w.mu.Lock()
		w.memoryAlertSent = false
		w.mu.Unlock()
	}
}

func (w *Watcher) checkGatewayHealth(ctx context.Context) {
	gwErr := w.pingGateway()

	w.mu.Lock()
	if gwErr != nil {
		w.healthFailures++
		failures := w.healthFailures
		alreadySent := w.degradedSent
		w.mu.Unlock()

		w.logger.Warn("gateway health check failed",
			"failures", failures,
			"threshold", unhealthyThreshold,
			"err", gwErr,
		)

		if failures >= unhealthyThreshold && !alreadySent {
			w.handleDegraded(ctx, gwErr)
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

// --- LLM health check ---
// Detects when the LLM provider (e.g., claude-max-api proxy) is down.
// Gateway is alive and responding, but can't process any AI work.

func (w *Watcher) checkLLMHealth(ctx context.Context) {
	err := w.pingLLM()

	w.mu.Lock()
	if err != nil {
		w.llmFailures++
		failures := w.llmFailures
		alreadySent := w.llmAlertSent
		w.mu.Unlock()

		w.logger.Warn("LLM health check failed",
			"failures", failures,
			"threshold", unhealthyThreshold,
			"url", w.cfg.LLMHealthURL,
			"err", err,
		)

		if failures >= unhealthyThreshold && !alreadySent {
			w.handleLLMDown(ctx, err)
		}
	} else {
		wasDown := w.llmFailures >= unhealthyThreshold
		w.llmFailures = 0
		w.llmAlertSent = false
		w.mu.Unlock()

		if wasDown {
			w.logger.Info("LLM provider recovered")
		}
	}
}

func (w *Watcher) pingLLM() error {
	client := &http.Client{Timeout: healthCheckTimeout}

	req, err := http.NewRequest(http.MethodGet, w.cfg.LLMHealthURL, nil)
	if err != nil {
		return fmt.Errorf("LLM request build: %w", err)
	}

	// Add auth header if configured.
	// Supports multiple formats:
	//   "Bearer sk-or-..."          -> Authorization: Bearer sk-or-...  (OpenRouter, OpenAI)
	//   "x-api-key sk-ant-..."      -> x-api-key: sk-ant-...           (Anthropic)
	//   "sk-or-..."                 -> Authorization: Bearer sk-or-...  (bare key, assumes Bearer)
	if auth := w.cfg.LLMHealthAuth; auth != "" {
		if strings.HasPrefix(auth, "x-api-key ") {
			req.Header.Set("x-api-key", strings.TrimPrefix(auth, "x-api-key "))
		} else if strings.HasPrefix(auth, "Bearer ") {
			req.Header.Set("Authorization", auth)
		} else {
			// Bare key -- assume Bearer
			req.Header.Set("Authorization", "Bearer "+auth)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("LLM unreachable: %w", err)
	}
	defer resp.Body.Close()

	// 401/403 = auth issue (key expired), 429 = rate limited / credits exhausted
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("LLM auth failed (HTTP %d): API key may be expired or invalid", resp.StatusCode)
	}
	if resp.StatusCode == 429 {
		return fmt.Errorf("LLM rate limited (HTTP 429): credits may be exhausted")
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("LLM server error (HTTP %d)", resp.StatusCode)
	}

	return nil
}

func (w *Watcher) handleLLMDown(ctx context.Context, lastErr error) {
	w.mu.Lock()
	w.llmAlertSent = true
	w.mu.Unlock()

	w.logger.Warn("LLM provider is down: agent cannot process AI requests",
		"llmURL", w.cfg.LLMHealthURL,
		"consecutiveFailures", w.llmFailures,
		"lastErr", lastErr,
	)

	// Don't restart -- the gateway is fine, it's the LLM that's broken.
	// Just report to hospital for alerting + tracking.
	cc := w.collector.CollectCrash(ctx)
	l0diag := diagnosis.DiagnosisResult{
		Layer:      "L0",
		Summary:    fmt.Sprintf("LLM provider down: %s", lastErr),
		Repairs:    []string{}, // no local fix for API key / provider issues
		Confidence: 0.5,
	}

	now := time.Now()
	crashEv := CrashEvent{
		Context:     cc,
		L0Diagnosis: l0diag,
		LocalRepairs: []repair.Result{{
			Action:  "none",
			Success: false,
			Output:  "LLM provider down -- no local repair available, needs human intervention",
		}},
		Timestamp: now,
	}

	w.mu.Lock()
	w.state.LastCrashAt = &now
	w.state.LastRepairs = []string{"llm-down(no-fix)"}
	w.lastCrashEv = &crashEv
	w.mu.Unlock()

	// Push to hospital -- hospital will alert via Slack
	select {
	case w.CrashCh <- crashEv:
	default:
		w.logger.Warn("crash channel full, dropping LLM-down event")
	}

	w.logger.Info("LLM-down event reported to hospital for alerting")

	// Emit structured LLM status event
	llmEv := classifyLLMError(lastErr, w.cfg.LLMHealthURL)
	select {
	case w.LLMStatusCh <- llmEv:
	default:
		w.logger.Warn("LLM status channel full, dropping event")
	}
}

// classifyLLMError maps an LLM health check error to a structured status event.
func classifyLLMError(err error, endpoint string) LLMStatusEvent {
	msg := err.Error()
	ev := LLMStatusEvent{
		Endpoint: endpoint,
		Detail:   msg,
		Status:   "down",
	}

	switch {
	case strings.Contains(msg, "auth failed") || strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403"):
		ev.Category = "auth_expired"
		ev.Status = "auth_expired"
		if strings.Contains(msg, "401") {
			ev.HTTPCode = 401
		} else {
			ev.HTTPCode = 403
		}
	case strings.Contains(msg, "rate limited") || strings.Contains(msg, "HTTP 429"):
		ev.Category = "credits_exhausted"
		ev.Status = "credits_exhausted"
		ev.HTTPCode = 429
	case strings.Contains(msg, "server error") || strings.Contains(msg, "HTTP 5"):
		ev.Category = "provider_outage"
		ev.Status = "provider_outage"
		ev.HTTPCode = 500
	case strings.Contains(msg, "unreachable") || strings.Contains(msg, "connection refused"):
		ev.Category = "integration_failure"
		ev.Status = "unreachable"
	default:
		ev.Category = "unknown"
	}

	// Extract provider from endpoint URL
	switch {
	case strings.Contains(endpoint, "openrouter.ai"):
		ev.Provider = "openrouter"
	case strings.Contains(endpoint, "openai.com"):
		ev.Provider = "openai"
	case strings.Contains(endpoint, "anthropic.com"):
		ev.Provider = "anthropic"
	case strings.Contains(endpoint, "localhost") || strings.Contains(endpoint, "127.0.0.1"):
		ev.Provider = "local-proxy"
	default:
		ev.Provider = "unknown"
	}

	return ev
}

// --- Extra unit monitoring ---
// Watches additional systemd units (e.g. claude-max-api-proxy.service) and
// restarts them if they go down.

func (w *Watcher) extraUnitsLoop(ctx context.Context) {
	w.logger.Info("extra unit monitor started", "units", w.cfg.ExtraUnits)

	// Wait for grace period
	select {
	case <-ctx.Done():
		return
	case <-time.After(graceAfterStart):
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, unit := range w.cfg.ExtraUnits {
				w.checkExtraUnit(ctx, unit)
			}
		}
	}
}

func (w *Watcher) checkExtraUnit(ctx context.Context, unit string) {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(tctx, "systemctl", "--user", "is-active", unit).Output()
	status := strings.TrimSpace(string(out))
	if err != nil {
		status = strings.TrimSpace(string(out))
	}

	w.mu.Lock()
	wasFailed := w.extraUnitFailed[unit]
	w.mu.Unlock()

	if status == "active" {
		if wasFailed {
			w.logger.Info("extra unit recovered", "unit", unit)
			w.mu.Lock()
			w.extraUnitFailed[unit] = false
			w.mu.Unlock()
		}
		return
	}

	if status == "failed" || status == "inactive" {
		if !wasFailed {
			w.logger.Warn("extra unit down, attempting restart", "unit", unit, "status", status)
			w.mu.Lock()
			w.extraUnitFailed[unit] = true
			w.mu.Unlock()

			// Attempt restart
			exec.Command("systemctl", "--user", "reset-failed", unit).Run()
			restartOut, restartErr := exec.Command("systemctl", "--user", "restart", unit).CombinedOutput()

			if restartErr != nil {
				w.logger.Error("extra unit restart failed", "unit", unit, "err", restartErr, "output", string(restartOut))
			} else {
				// Wait and verify
				time.Sleep(3 * time.Second)
				verifyOut, _ := exec.Command("systemctl", "--user", "is-active", unit).Output()
				verifyStatus := strings.TrimSpace(string(verifyOut))
				if verifyStatus == "active" {
					w.logger.Info("extra unit restarted successfully", "unit", unit)
					w.mu.Lock()
					w.extraUnitFailed[unit] = false
					w.mu.Unlock()
				} else {
					w.logger.Error("extra unit still down after restart", "unit", unit, "status", verifyStatus)
				}
			}

			// Report to hospital as a crash event
			cc := w.collector.CollectCrash(ctx)
			l0diag := diagnosis.DiagnosisResult{
				Layer:      "L0",
				Summary:    fmt.Sprintf("Extra unit %s is %s, attempted restart", unit, status),
				Repairs:    []string{"restart-extra-unit"},
				Confidence: 0.5,
			}

			now := time.Now()
			crashEv := CrashEvent{
				Context:     cc,
				L0Diagnosis: l0diag,
				LocalRepairs: []repair.Result{{
					Action:  "restart-extra-unit:" + unit,
					Success: restartErr == nil,
					Output:  strings.TrimSpace(string(restartOut)),
				}},
				Timestamp: now,
			}

			select {
			case w.CrashCh <- crashEv:
			default:
				w.logger.Warn("crash channel full, dropping extra unit event")
			}
		}
	}
}

// --- Integration health check ---
// Probes messaging integrations (Slack, Discord, Telegram, WhatsApp) to verify
// that tokens are valid and services are reachable.

const integrationCheckEvery = 5 * time.Minute

func (w *Watcher) integrationHealthLoop(ctx context.Context) {
	// Wait for grace period
	select {
	case <-ctx.Done():
		return
	case <-time.After(graceAfterStart):
	}

	channels, _, err := config.DiscoverChannels(w.cfg.StateDir, w.cfg.Framework)
	if err != nil || len(channels) == 0 {
		w.logger.Info("no integrations to monitor", "err", err)
		return
	}

	w.logger.Info("integration health checker started", "count", len(channels), "interval", integrationCheckEvery)

	// Run first check immediately
	w.probeIntegrations(channels)

	ticker := time.NewTicker(integrationCheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-discover channels in case config changed
			if updated, _, err := config.DiscoverChannels(w.cfg.StateDir, w.cfg.Framework); err == nil && len(updated) > 0 {
				channels = updated
			}
			w.probeIntegrations(channels)
		}
	}
}

func (w *Watcher) probeIntegrations(channels []config.ChannelConfig) {
	client := &http.Client{Timeout: 10 * time.Second}
	var results []IntegrationStatusEvent

	for _, ch := range channels {
		ev := IntegrationStatusEvent{Integration: ch.Type}

		switch ch.Type {
		case "slack":
			ev = w.probeSlack(client, ch.Token)
		case "discord":
			ev = w.probeDiscord(client, ch.Token)
		case "telegram":
			ev = w.probeTelegram(client, ch.Token)
		case "whatsapp":
			ev = w.probeWhatsApp()
		default:
			ev.Connected = false
			ev.Error = "unknown integration type"
		}

		results = append(results, ev)
	}

	// Store latest results
	w.integrationsMu.Lock()
	w.latestIntegrations = results
	w.integrationsMu.Unlock()

	// Push to channel for heartbeat
	select {
	case w.IntegrationStatusCh <- results:
	default:
	}

	// Check for failures and alert
	for _, ev := range results {
		if !ev.Connected {
			w.mu.Lock()
			w.integrationAlertSent[ev.Integration]++
			failures := w.integrationAlertSent[ev.Integration]
			w.mu.Unlock()

			if failures == 2 { // alert on 2nd consecutive failure
				w.logger.Warn("integration probe failed", "integration", ev.Integration, "error", ev.Error)
			}
		} else {
			w.mu.Lock()
			w.integrationAlertSent[ev.Integration] = 0
			w.mu.Unlock()
		}
	}
}

func (w *Watcher) probeSlack(client *http.Client, token string) IntegrationStatusEvent {
	ev := IntegrationStatusEvent{Integration: "slack"}
	if token == "" {
		ev.Connected = false
		ev.Error = "no bot token configured"
		return ev
	}

	start := time.Now()
	req, _ := http.NewRequest("POST", "https://slack.com/api/auth.test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	ev.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		ev.Connected = false
		ev.Error = fmt.Sprintf("request failed: %v", err)
		return ev
	}
	defer resp.Body.Close()
	ev.HTTPCode = resp.StatusCode

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.Unmarshal(body, &result)

	ev.Connected = result.OK
	if !result.OK {
		ev.Error = result.Error
	}
	return ev
}

func (w *Watcher) probeDiscord(client *http.Client, token string) IntegrationStatusEvent {
	ev := IntegrationStatusEvent{Integration: "discord"}
	if token == "" {
		ev.Connected = false
		ev.Error = "no bot token configured"
		return ev
	}

	start := time.Now()
	req, _ := http.NewRequest("GET", "https://discord.com/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bot "+token)

	resp, err := client.Do(req)
	ev.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		ev.Connected = false
		ev.Error = fmt.Sprintf("request failed: %v", err)
		return ev
	}
	defer resp.Body.Close()
	ev.HTTPCode = resp.StatusCode

	if resp.StatusCode == 200 {
		ev.Connected = true
	} else {
		ev.Connected = false
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		ev.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return ev
}

func (w *Watcher) probeTelegram(client *http.Client, token string) IntegrationStatusEvent {
	ev := IntegrationStatusEvent{Integration: "telegram"}
	if token == "" {
		ev.Connected = false
		ev.Error = "no bot token configured"
		return ev
	}

	start := time.Now()
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
	resp, err := client.Get(url)
	ev.LatencyMs = int(time.Since(start).Milliseconds())
	if err != nil {
		ev.Connected = false
		ev.Error = fmt.Sprintf("request failed: %v", err)
		return ev
	}
	defer resp.Body.Close()
	ev.HTTPCode = resp.StatusCode

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var result struct {
		OK bool `json:"ok"`
	}
	json.Unmarshal(body, &result)

	ev.Connected = result.OK
	if !result.OK {
		ev.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return ev
}

func (w *Watcher) probeWhatsApp() IntegrationStatusEvent {
	ev := IntegrationStatusEvent{Integration: "whatsapp"}

	// WhatsApp (Baileys) sessions are local files -- check if credentials exist
	credDir := fmt.Sprintf("%s/credentials", w.cfg.StateDir)
	entries, err := os.ReadDir(credDir)
	if err != nil {
		ev.Connected = false
		ev.Error = "credentials directory not found"
		return ev
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "whatsapp-") && e.IsDir() {
			credsFile := fmt.Sprintf("%s/%s/creds.json", credDir, e.Name())
			if _, err := os.Stat(credsFile); err == nil {
				ev.Connected = true
				return ev
			}
		}
	}

	ev.Connected = false
	ev.Error = "no WhatsApp session found (QR scan required)"
	return ev
}

// --- MCP server health check ---
// Probes configured MCP servers to verify they are available.

func (w *Watcher) mcpHealthLoop(ctx context.Context) {
	// Wait for grace period
	select {
	case <-ctx.Done():
		return
	case <-time.After(graceAfterStart):
	}

	servers, err := config.DiscoverMCPServers(w.cfg.StateDir, w.cfg.Framework)
	if err != nil || len(servers) == 0 {
		w.logger.Info("no MCP servers to monitor", "err", err)
		return
	}

	w.logger.Info("MCP health checker started", "count", len(servers), "interval", integrationCheckEvery)

	// Run first check immediately
	w.probeMCPServers(servers)

	ticker := time.NewTicker(integrationCheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if updated, err := config.DiscoverMCPServers(w.cfg.StateDir, w.cfg.Framework); err == nil && len(updated) > 0 {
				servers = updated
			}
			w.probeMCPServers(servers)
		}
	}
}

func (w *Watcher) probeMCPServers(servers []config.MCPServerConfig) {
	client := &http.Client{Timeout: 10 * time.Second}
	var results []MCPStatusEvent

	for _, srv := range servers {
		ev := MCPStatusEvent{
			ServerName: srv.Name,
			Transport:  srv.Transport,
		}

		switch srv.Transport {
		case "stdio":
			// Check if the command binary exists on PATH
			_, err := exec.LookPath(srv.Command)
			if err != nil {
				ev.Alive = false
				ev.Error = fmt.Sprintf("binary not found: %s", srv.Command)
			} else {
				ev.Alive = true
			}
		case "sse", "streamable-http":
			// Check if the HTTP endpoint is reachable
			if srv.URL == "" {
				ev.Alive = false
				ev.Error = "no URL configured"
			} else {
				resp, err := client.Get(srv.URL)
				if err != nil {
					ev.Alive = false
					ev.Error = fmt.Sprintf("unreachable: %v", err)
				} else {
					resp.Body.Close()
					ev.Alive = true
				}
			}
		default:
			ev.Alive = false
			ev.Error = fmt.Sprintf("unknown transport: %s", srv.Transport)
		}

		results = append(results, ev)
	}

	// Store latest results
	w.integrationsMu.Lock()
	w.latestMCPServers = results
	w.integrationsMu.Unlock()

	// Push to channel for heartbeat
	select {
	case w.MCPStatusCh <- results:
	default:
	}

	// Check for failures and alert
	for _, ev := range results {
		if !ev.Alive {
			w.mu.Lock()
			w.mcpAlertSent[ev.ServerName]++
			failures := w.mcpAlertSent[ev.ServerName]
			w.mu.Unlock()

			if failures == 2 {
				w.logger.Warn("MCP server probe failed", "server", ev.ServerName, "transport", ev.Transport, "error", ev.Error)
			}
		} else {
			w.mu.Lock()
			w.mcpAlertSent[ev.ServerName] = 0
			w.mu.Unlock()
		}
	}
}

// --- Session snapshot ---

func (w *Watcher) collectSessionSnapshot() {
	var sessionsDir string
	if w.cfg.Framework == "openclaw" {
		// OpenClaw stores sessions under <stateDir>/agents/*/sessions/
		sessionsDir = filepath.Join(w.cfg.StateDir, "agents")
	} else {
		// Hermes stores sessions under <stateDir>/sessions/
		sessionsDir = filepath.Join(w.cfg.StateDir, "sessions")
	}

	if _, err := os.Stat(sessionsDir); err != nil {
		return // no sessions dir
	}

	var totalCount int
	var totalSize int64
	var oldest time.Time

	err := filepath.WalkDir(sessionsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		totalCount++
		totalSize += info.Size()
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return
	}

	snap := &SessionSnapshot{
		TotalCount:  totalCount,
		TotalSizeMb: int(totalSize / (1024 * 1024)),
	}
	if !oldest.IsZero() {
		ts := oldest.Format(time.RFC3339)
		snap.OldestTimestamp = ts
	}

	w.integrationsMu.Lock()
	w.latestSessions = snap
	w.integrationsMu.Unlock()

	if totalCount > 100 || totalSize > 500*1024*1024 {
		w.logger.Warn("session bloat detected", "count", totalCount, "sizeMb", snap.TotalSizeMb)
	}
}

// --- Cron snapshot ---

func (w *Watcher) collectCronSnapshot() {
	if w.cfg.Framework != "openclaw" {
		return // cron snapshot only for OpenClaw (uses CLI)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "openclaw", "cron", "list", "--json").Output()
	if err != nil {
		// CLI not available or no crons -- not fatal
		return
	}

	var cronList []struct {
		ID         string `json:"id"`
		Cron       string `json:"cron"`
		Schedule   string `json:"schedule"`
		LastRun    string `json:"lastRun"`
		LastStatus string `json:"lastStatus"`
		LastError  string `json:"lastError"`
		NextRun    string `json:"nextRun"`
	}

	if err := json.Unmarshal(out, &cronList); err != nil {
		return
	}

	var snapshots []CronSnapshot
	now := time.Now()

	for _, c := range cronList {
		sched := c.Cron
		if sched == "" {
			sched = c.Schedule
		}

		snap := CronSnapshot{
			ID:         c.ID,
			Schedule:   sched,
			LastRun:    c.LastRun,
			LastStatus: c.LastStatus,
			LastError:  c.LastError,
		}

		// Check if overdue: last run > 2x the expected interval
		if c.LastRun != "" && c.NextRun != "" {
			lastRun, err1 := time.Parse(time.RFC3339, c.LastRun)
			nextRun, err2 := time.Parse(time.RFC3339, c.NextRun)
			if err1 == nil && err2 == nil {
				interval := nextRun.Sub(lastRun)
				if interval > 0 && now.Sub(lastRun) > 2*interval {
					snap.Overdue = true
				}
			}
		}

		snapshots = append(snapshots, snap)
	}

	w.integrationsMu.Lock()
	w.latestCronHealth = snapshots
	w.integrationsMu.Unlock()

	// Log any failing crons
	for _, s := range snapshots {
		if s.LastStatus == "error" || s.Overdue {
			w.logger.Warn("cron health issue", "id", s.ID, "status", s.LastStatus, "overdue", s.Overdue)
		}
	}
}
