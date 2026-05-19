package diagnosis

import (
	"log/slog"

	"github.com/clawies/hospital-agent-sidecar/internal/collector"
	"github.com/clawies/hospital-agent-sidecar/internal/config"
)

// Diagnoser runs L0 pattern matching for immediate crash diagnosis.
// AI diagnosis is handled server-side by the hospital.
type Diagnoser struct {
	cfg    *config.Config
	logger *slog.Logger
}

func New(cfg *config.Config, logger *slog.Logger) *Diagnoser {
	return &Diagnoser{cfg: cfg, logger: logger}
}

// Diagnose analyzes a crash context using deterministic pattern matching (L0).
// This provides immediate best-effort repairs while the hospital does AI diagnosis.
func (d *Diagnoser) Diagnose(cc collector.CrashContext) DiagnosisResult {
	result := patternMatch(cc)
	d.logger.Info("L0 diagnosis complete",
		"summary", result.Summary,
		"repairs", result.Repairs,
		"confidence", result.Confidence,
	)
	return result
}
