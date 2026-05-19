package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/clawies/hospital-agent-sidecar/internal/config"
	"github.com/clawies/hospital-agent-sidecar/internal/server"
	"github.com/clawies/hospital-agent-sidecar/internal/setup"
)

var version = "dev"

func main() {
	// Subcommand routing: setup, status, uninstall, or default (run server)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			if err := setup.Run(os.Args[2:], version); err != nil {
				fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "status":
			if err := setup.Status(); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			return
		case "uninstall":
			if err := setup.Uninstall(); err != nil {
				fmt.Fprintf(os.Stderr, "uninstall failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "--version", "-v", "version":
			fmt.Println(version)
			return
		case "--help", "-h", "help":
			printUsage()
			return
		}
	}

	// Default: run the sidecar server (for systemd ExecStart)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := server.Run(ctx, cfg, logger, version); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`hospital-sidecar %s -- crash detection & auto-repair for AI agent gateways

Usage:
  hospital-sidecar              Run the sidecar server (used by systemd)
  hospital-sidecar setup        Install and start as a systemd service
  hospital-sidecar status       Check if the sidecar is running
  hospital-sidecar uninstall    Stop and remove the sidecar
  hospital-sidecar version      Print version

Setup flags:
  --api-key KEY        (required) API key for Agent Hospital server
  --hospital-url URL   (required) Agent Hospital server URL
  --inbound-token TOK  Bearer token for inbound requests (auto-generated if omitted)
  --framework NAME     openclaw or hermes (auto-detected)
  --state-dir DIR      Agent home dir (auto-detected)
  --systemd-unit NAME  Systemd unit to watch (auto-detected)
  --gateway-port PORT  Gateway port (default 18789)
  --port PORT          Sidecar listen port (default 18793)
  --name NAME          Agent name (default hostname)
`, version)
}
