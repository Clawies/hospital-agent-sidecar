package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/auth"
	"github.com/clawies/hospital-agent-sidecar/internal/collector"
	"github.com/clawies/hospital-agent-sidecar/internal/config"
	"github.com/clawies/hospital-agent-sidecar/internal/diagnosis"
	"github.com/clawies/hospital-agent-sidecar/internal/handlers"
	"github.com/clawies/hospital-agent-sidecar/internal/heartbeat"
	"github.com/clawies/hospital-agent-sidecar/internal/repair"
	"github.com/clawies/hospital-agent-sidecar/internal/watcher"
)

func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) error {
	// Create dependencies
	coll := collector.New(cfg, logger)
	diagnoser := diagnosis.New(cfg, logger)
	repairer := repair.New(cfg, logger)
	watch := watcher.New(cfg, logger, coll, diagnoser, repairer)
	hb, err := heartbeat.New(cfg, logger, watch, coll, version)
	if err != nil {
		return fmt.Errorf("init heartbeat: %w", err)
	}

	// Initialize direct channel alerter (reads openclaw.json for Slack/Discord/Telegram tokens)
	alerter := heartbeat.NewAlerter(cfg.StateDir, cfg.Framework, cfg.AgentName, logger)
	if alerter != nil {
		hb.SetAlerter(alerter)
	}

	h := &handlers.Handlers{
		Config:    cfg,
		Version:   version,
		Logger:    logger,
		Watcher:   watch,
		Repairer:  repairer,
		Collector: coll,
	}

	// Register routes
	mux := http.NewServeMux()
	h.Register(mux)

	// Middleware: auth -> request logger
	handler := auth.Middleware(cfg.InboundToken)(mux)
	handler = requestLogger(logger)(handler)

	srv := &http.Server{
		Addr:              net.JoinHostPort("0.0.0.0", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("hospital-agent starting",
		"addr", srv.Addr,
		"version", version,
		"framework", cfg.Framework,
		"unit", cfg.SystemdUnit,
		"hospital", cfg.HospitalURL,
	)

	// Start background goroutines
	watch.Start(ctx)
	hb.Start(ctx)

	// Run HTTP server
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		logger.Info("hospital-agent shut down cleanly")
		return nil
	}
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(lw, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", lw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggingResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}
