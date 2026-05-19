package diagnosis

import (
	"strings"

	"github.com/clawies/hospital-agent-sidecar/internal/collector"
)

// DiagnosisResult holds the output of any diagnosis layer.
type DiagnosisResult struct {
	Layer     string   `json:"layer"`     // "L0" (pattern), "L1" (local AI), "L2" (hospital)
	Summary   string   `json:"summary"`
	Repairs   []string `json:"repairs"`   // whitelisted action names to execute
	Confidence float64 `json:"confidence"`
}

// patternMatch applies deterministic rules based on exit codes and log patterns.
// This is L0 -- works when everything else is dead.
func patternMatch(cc collector.CrashContext) DiagnosisResult {
	var repairs []string
	var reasons []string

	// Rule 1: OOM (exit 137 = SIGKILL, signal 9)
	if cc.ExitSignal == 9 || cc.ExitCode == 137 {
		reasons = append(reasons, "OOM kill detected (exit 137/SIGKILL)")
		repairs = append(repairs, "kill-zombies", "restart-gateway")

		if cc.DmesgOOM != "" {
			reasons = append(reasons, "dmesg confirms OOM killer activity")
		}
	}

	// Rule 2: Disk full (ENOSPC in logs or journal)
	if strings.Contains(cc.Journal, "ENOSPC") || strings.Contains(cc.LogTail, "ENOSPC") ||
		strings.Contains(cc.Journal, "No space left on device") ||
		cc.DiskUsage.UsedPercent >= 98 {
		reasons = append(reasons, "disk full detected")
		repairs = append(repairs, "clear-logs", "clear-disk-cache")
		if cc.DiskUsage.UsedPercent >= 100 || cc.DiskUsage.AvailMB < 10 {
			repairs = append(repairs, "emergency-disk")
		}
		repairs = append(repairs, "restart-gateway")
	}

	// Rule 3: Port conflict (EADDRINUSE)
	if strings.Contains(cc.Journal, "EADDRINUSE") || strings.Contains(cc.LogTail, "EADDRINUSE") ||
		strings.Contains(cc.Journal, "address already in use") {
		reasons = append(reasons, "port conflict detected (EADDRINUSE)")
		repairs = append(repairs, "kill-port", "restart-gateway")
	}

	// Rule 4: Memory pressure (not OOM-killed but memory is very low)
	if cc.MemInfo.UsedPercent > 90 && cc.ExitSignal != 9 {
		reasons = append(reasons, "high memory pressure")
		repairs = append(repairs, "kill-zombies")
	}

	// Rule 5: Generic crash -- no specific pattern matched
	if len(repairs) == 0 {
		reasons = append(reasons, "no specific crash pattern matched, attempting restart")
		repairs = append(repairs, "restart-gateway")
	}

	// Deduplicate repairs
	repairs = dedup(repairs)

	return DiagnosisResult{
		Layer:      "L0",
		Summary:    "Pattern match: " + strings.Join(reasons, "; "),
		Repairs:    repairs,
		Confidence: 0.6,
	}
}

func dedup(items []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}
