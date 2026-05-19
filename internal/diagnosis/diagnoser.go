package diagnosis

import (
	"context"
	"log/slog"

	"github.com/clawies/hospital-agent/internal/collector"
	"github.com/clawies/hospital-agent/internal/config"
)

// Diagnoser orchestrates the L1 -> L0 fallback chain.
type Diagnoser struct {
	cfg    *config.Config
	logger *slog.Logger
}

func New(cfg *config.Config, logger *slog.Logger) *Diagnoser {
	return &Diagnoser{cfg: cfg, logger: logger}
}

// Diagnose analyzes a crash context using the fallback chain:
// L1 (local AI) -> L0 (pattern matching).
// Always returns a result -- never fails.
func (d *Diagnoser) Diagnose(ctx context.Context, cc collector.CrashContext) DiagnosisResult {
	// L1: Try local AI first
	if result := localAIDiagnose(ctx, d.cfg, d.logger, cc); result != nil {
		d.logger.Info("L1 diagnosis complete",
			"summary", result.Summary,
			"repairs", result.Repairs,
			"confidence", result.Confidence,
		)
		return *result
	}

	// L0: Fall through to pattern matching
	result := patternMatch(cc)
	d.logger.Info("L0 diagnosis complete",
		"summary", result.Summary,
		"repairs", result.Repairs,
		"confidence", result.Confidence,
	)
	return result
}
