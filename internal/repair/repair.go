package repair

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/config"
)

const (
	cooldownSec  = 60
	maxAttempts  = 3
	reserveFile  = "/tmp/hospital-agent-disk-reserve"
	reserveBytes = 50 * 1024 * 1024 // 50MB
)

type Result struct {
	Action  string `json:"action"`
	Success bool   `json:"success"`
	Output  string `json:"output"`
}

type historyEntry struct {
	Action    string    `json:"action"`
	Success   bool      `json:"success"`
	Output    string    `json:"output"`
	Timestamp time.Time `json:"timestamp"`
}

type actionState struct {
	lastExec time.Time
	attempts int
}

type Repairer struct {
	cfg     *config.Config
	logger  *slog.Logger
	mu      sync.Mutex
	states  map[string]*actionState
	history []historyEntry
}

func New(cfg *config.Config, logger *slog.Logger) *Repairer {
	r := &Repairer{
		cfg:    cfg,
		logger: logger,
		states: make(map[string]*actionState),
	}
	// Create disk reserve file on startup
	r.ensureReserveFile()
	return r
}

// Execute runs a whitelisted repair action with cooldown and attempt tracking.
func (r *Repairer) Execute(action string) Result {
	r.mu.Lock()

	// Check whitelist
	if _, ok := whitelist[action]; !ok {
		r.mu.Unlock()
		return Result{Action: action, Success: false, Output: fmt.Sprintf("unknown action: %s", action)}
	}

	// Check cooldown
	state, exists := r.states[action]
	if !exists {
		state = &actionState{}
		r.states[action] = state
	}

	if time.Since(state.lastExec) < time.Duration(cooldownSec)*time.Second {
		remaining := cooldownSec - int(time.Since(state.lastExec).Seconds())
		r.mu.Unlock()
		return Result{Action: action, Success: false, Output: fmt.Sprintf("cooldown: %ds remaining", remaining)}
	}

	if state.attempts >= maxAttempts {
		r.mu.Unlock()
		return Result{Action: action, Success: false, Output: fmt.Sprintf("max attempts (%d) exhausted", maxAttempts)}
	}

	state.lastExec = time.Now()
	state.attempts++
	r.mu.Unlock()

	// Execute
	r.logger.Info("executing repair", "action", action, "attempt", state.attempts)
	result := r.doRepair(action)

	// Record history
	r.mu.Lock()
	r.history = append(r.history, historyEntry{
		Action:    action,
		Success:   result.Success,
		Output:    result.Output,
		Timestamp: time.Now(),
	})
	r.mu.Unlock()

	return result
}

// ResetAttempts clears attempt counter for an action (used when hospital prescribes new approach).
func (r *Repairer) ResetAttempts(action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.states[action]; ok {
		state.attempts = 0
	}
}

// ResetAll clears all attempt counters (new incident).
func (r *Repairer) ResetAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = make(map[string]*actionState)
}

// RecentHistory returns the last N repair results.
func (r *Repairer) RecentHistory(n int) []historyEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.history) <= n {
		cp := make([]historyEntry, len(r.history))
		copy(cp, r.history)
		return cp
	}
	cp := make([]historyEntry, n)
	copy(cp, r.history[len(r.history)-n:])
	return cp
}

func (r *Repairer) doRepair(action string) Result {
	fn := whitelist[action]
	output, err := fn(r)
	if err != nil {
		r.logger.Error("repair failed", "action", action, "err", err)
		return Result{Action: action, Success: false, Output: err.Error()}
	}
	r.logger.Info("repair succeeded", "action", action, "output", output)
	return Result{Action: action, Success: true, Output: output}
}

// --- Repair functions ---

type repairFunc func(r *Repairer) (string, error)

var whitelist = map[string]repairFunc{
	"restart-gateway":  restartGateway,
	"kill-zombies":     killZombies,
	"clear-logs":       clearLogs,
	"clear-disk-cache": clearDiskCache,
	"emergency-disk":   emergencyDisk,
	"kill-port":        killPort,
}

func restartGateway(r *Repairer) (string, error) {
	unit := r.cfg.SystemdUnit

	// Reset failed state first
	exec.Command("systemctl", "--user", "reset-failed", unit).Run()

	out, err := exec.Command("systemctl", "--user", "restart", unit).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("restart failed: %s -- %w", strings.TrimSpace(string(out)), err)
	}

	// Wait 5s then check if active
	time.Sleep(5 * time.Second)
	checkOut, err := exec.Command("systemctl", "--user", "is-active", unit).Output()
	status := strings.TrimSpace(string(checkOut))
	if err != nil || status != "active" {
		return "", fmt.Errorf("unit not active after restart: status=%s", status)
	}

	return "restarted successfully, unit is active", nil
}

func killZombies(r *Repairer) (string, error) {
	killed := 0

	// Kill orphaned daemon processes (framework-aware)
	var patterns []string
	var countPattern string
	if r.cfg.Framework == "hermes" {
		patterns = []string{
			"hermes_cli.*gateway",
			"hermes.*daemon",
			"python.*hermes",
		}
		countPattern = "hermes_cli|hermes.*daemon"
	} else {
		patterns = []string{
			"openclaw.*daemon",
			"node.*openclaw",
		}
		countPattern = "openclaw|node.*openclaw"
	}

	for _, pattern := range patterns {
		out, _ := exec.Command("pkill", "-9", "-f", pattern).CombinedOutput()
		if strings.TrimSpace(string(out)) != "" {
			killed++
		}
	}

	countOut, _ := exec.Command("bash", "-c",
		fmt.Sprintf("pgrep -f '%s' | wc -l", countPattern)).Output()
	remaining := strings.TrimSpace(string(countOut))

	return fmt.Sprintf("killed %d zombie pattern(s), remaining processes: %s", killed, remaining), nil
}

func clearLogs(r *Repairer) (string, error) {
	logDir := "/tmp/openclaw"
	var cleaned int64

	entries, err := os.ReadDir(logDir)
	if err != nil {
		return "", fmt.Errorf("read log dir: %w", err)
	}

	cutoff := time.Now().AddDate(0, 0, -7)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		path := filepath.Join(logDir, e.Name())

		// Delete files older than 7 days
		if info.ModTime().Before(cutoff) {
			cleaned += info.Size()
			os.Remove(path)
			continue
		}

		// Truncate files larger than 100MB
		if info.Size() > 100*1024*1024 {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644)
			if err == nil {
				cleaned += info.Size()
				f.Close()
			}
		}
	}

	return fmt.Sprintf("cleaned %dMB from %s", cleaned/(1024*1024), logDir), nil
}

func clearDiskCache(r *Repairer) (string, error) {
	stateDir := r.cfg.StateDir
	var cleaned int64

	// Prune old session files (>30d)
	cutoff := time.Now().AddDate(0, 0, -30)
	dirsToClean := []string{
		filepath.Join(stateDir, "sessions"),
		filepath.Join(stateDir, "completions"),
		filepath.Join(stateDir, "cache"),
	}

	for _, dir := range dirsToClean {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				cleaned += info.Size()
				os.Remove(path)
			}
			return nil
		})
	}

	return fmt.Sprintf("pruned %dMB of old cache/session data", cleaned/(1024*1024)), nil
}

func emergencyDisk(r *Repairer) (string, error) {
	info, err := os.Stat(reserveFile)
	if err != nil {
		return "no reserve file to delete", nil
	}

	size := info.Size()
	if err := os.Remove(reserveFile); err != nil {
		return "", fmt.Errorf("failed to delete reserve: %w", err)
	}

	return fmt.Sprintf("freed %dMB emergency reserve", size/(1024*1024)), nil
}

func killPort(r *Repairer) (string, error) {
	port := strconv.Itoa(r.cfg.GatewayPort)

	// Find PIDs using the port
	out, err := exec.Command("lsof", "-ti", ":"+port).Output()
	if err != nil {
		return "no process found on port " + port, nil
	}

	pids := strings.Fields(strings.TrimSpace(string(out)))
	if len(pids) == 0 {
		return "no process found on port " + port, nil
	}

	args := append([]string{"-9"}, pids...)
	exec.Command("kill", args...).Run()

	return fmt.Sprintf("killed %d process(es) on port %s", len(pids), port), nil
}

func (r *Repairer) ensureReserveFile() {
	if _, err := os.Stat(reserveFile); err == nil {
		return // already exists
	}

	f, err := os.Create(reserveFile)
	if err != nil {
		r.logger.Warn("failed to create disk reserve", "err", err)
		return
	}
	defer f.Close()

	// Write 50MB of zeros
	buf := make([]byte, 1024*1024) // 1MB chunks
	for i := 0; i < 50; i++ {
		if _, err := f.Write(buf); err != nil {
			r.logger.Warn("failed to write disk reserve", "err", err, "written_mb", i)
			return
		}
	}

	r.logger.Info("created 50MB disk reserve", "path", reserveFile)
}
