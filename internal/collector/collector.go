package collector

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/config"
)

type Collector struct {
	cfg    *config.Config
	logger *slog.Logger
}

func New(cfg *config.Config, logger *slog.Logger) *Collector {
	return &Collector{cfg: cfg, logger: logger}
}

// CrashContext holds everything we know about a crash.
type CrashContext struct {
	Unit       string `json:"unit"`
	ExitCode   int    `json:"exitCode"`
	ExitSignal int    `json:"exitSignal"`
	Journal    string `json:"journal"`
	DmesgOOM   string `json:"dmesgOom"`
	DiskUsage  DiskInfo   `json:"disk"`
	MemInfo    MemoryInfo `json:"memory"`
	LogTail    string `json:"logTail"`
	Timestamp  string `json:"timestamp"`
}

type DiskInfo struct {
	TotalMB     int `json:"totalMb"`
	UsedMB      int `json:"usedMb"`
	AvailMB     int `json:"availMb"`
	UsedPercent int `json:"usedPercent"`
}

type MemoryInfo struct {
	TotalMB     int `json:"totalMb"`
	AvailMB     int `json:"availMb"`
	UsedPercent int `json:"usedPercent"`
}

type SystemInfo struct {
	Disk   DiskInfo   `json:"disk"`
	Memory MemoryInfo `json:"memory"`
	LoadAvg string   `json:"loadAvg"`
}

// CollectCrash gathers crash context for diagnosis.
func (c *Collector) CollectCrash(ctx context.Context) CrashContext {
	cc := CrashContext{
		Unit:      c.cfg.SystemdUnit,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	cc.ExitCode, cc.ExitSignal = c.unitExitInfo(ctx)
	cc.Journal = c.journalTail(ctx, 50)
	cc.DmesgOOM = c.dmesgOOM(ctx)
	cc.DiskUsage = c.diskInfo()
	cc.MemInfo = c.memInfo()
	cc.LogTail = c.gatewayLogTail()

	return cc
}

// SystemInfo returns current system metrics (for heartbeats).
func (c *Collector) SystemInfo() SystemInfo {
	return SystemInfo{
		Disk:    c.diskInfo(),
		Memory:  c.memInfo(),
		LoadAvg: c.loadAvg(),
	}
}

// unitExitInfo extracts the exit code and signal from the systemd unit.
func (c *Collector) unitExitInfo(ctx context.Context) (int, int) {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Get ExecMainStatus (exit code)
	out, err := exec.CommandContext(tctx, "systemctl", "--user", "show",
		c.cfg.SystemdUnit, "--property=ExecMainStatus").Output()
	if err != nil {
		return -1, -1
	}

	exitCode := -1
	line := strings.TrimSpace(string(out))
	if strings.HasPrefix(line, "ExecMainStatus=") {
		if n, err := strconv.Atoi(strings.TrimPrefix(line, "ExecMainStatus=")); err == nil {
			exitCode = n
		}
	}

	// Check if killed by signal
	exitSignal := 0
	if exitCode > 128 {
		exitSignal = exitCode - 128 // e.g., 137 -> signal 9 (SIGKILL)
	}

	return exitCode, exitSignal
}

// journalTail gets the last N lines from journalctl for the unit.
func (c *Collector) journalTail(ctx context.Context, lines int) string {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(tctx, "journalctl", "--user-unit", c.cfg.SystemdUnit,
		"-n", strconv.Itoa(lines), "--no-pager", "-o", "short-iso").Output()
	if err != nil {
		c.logger.Warn("journalctl failed", "err", err)
		return ""
	}
	return string(out)
}

// dmesgOOM checks for recent OOM killer activity.
func (c *Collector) dmesgOOM(ctx context.Context) string {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// dmesg may require root, but worth trying
	out, err := exec.CommandContext(tctx, "dmesg", "--time-format", "iso").Output()
	if err != nil {
		return ""
	}

	// Filter for OOM-related lines
	var oomLines []string
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "oom") || strings.Contains(lower, "killed process") ||
			strings.Contains(lower, "out of memory") {
			oomLines = append(oomLines, line)
		}
	}

	// Return last 10 OOM lines
	if len(oomLines) > 10 {
		oomLines = oomLines[len(oomLines)-10:]
	}
	return strings.Join(oomLines, "\n")
}

// diskInfo gets disk usage for the root partition.
func (c *Collector) diskInfo() DiskInfo {
	out, err := exec.Command("df", "-m", "/").Output()
	if err != nil {
		return DiskInfo{}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return DiskInfo{}
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return DiskInfo{}
	}

	total, _ := strconv.Atoi(fields[1])
	used, _ := strconv.Atoi(fields[2])
	avail, _ := strconv.Atoi(fields[3])
	pctStr := strings.TrimSuffix(fields[4], "%")
	pct, _ := strconv.Atoi(pctStr)

	return DiskInfo{TotalMB: total, UsedMB: used, AvailMB: avail, UsedPercent: pct}
}

// memInfo reads /proc/meminfo for memory stats.
func (c *Collector) memInfo() MemoryInfo {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemoryInfo{}
	}

	var totalKB, availKB int
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			totalKB = parseMemLine(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			availKB = parseMemLine(line)
		}
	}

	totalMB := totalKB / 1024
	availMB := availKB / 1024
	usedPct := 0
	if totalMB > 0 {
		usedPct = ((totalMB - availMB) * 100) / totalMB
	}

	return MemoryInfo{TotalMB: totalMB, AvailMB: availMB, UsedPercent: usedPct}
}

func parseMemLine(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(fields[1])
	return n
}

// gatewayLogTail reads the tail of the gateway log file (framework-aware).
func (c *Collector) gatewayLogTail() string {
	var candidates []string

	if c.cfg.Framework == "hermes" {
		// Hermes logs to /tmp/hermes/gateway.log (single file, not date-rotated)
		candidates = []string{
			filepath.Join("/tmp", "hermes", "gateway.log"),
			filepath.Join("/tmp", "hermes", "errors.log"),
		}
	} else {
		// OpenClaw logs to /tmp/openclaw/openclaw-YYYY-MM-DD.log
		today := time.Now().Format("2006-01-02")
		yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		candidates = []string{
			filepath.Join("/tmp", "openclaw", fmt.Sprintf("openclaw-%s.log", today)),
			filepath.Join("/tmp", "openclaw", fmt.Sprintf("openclaw-%s.log", yesterday)),
		}
	}

	for _, logPath := range candidates {
		data, err := os.ReadFile(logPath)
		if err != nil {
			continue
		}
		// Return last 4KB
		if len(data) > 4096 {
			data = data[len(data)-4096:]
		}
		return string(data)
	}
	return ""
}

// loadAvg reads /proc/loadavg.
func (c *Collector) loadAvg() string {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
