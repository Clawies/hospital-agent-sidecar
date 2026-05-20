package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// Server
	Port          string // Listen port (default 18793)
	InboundToken  string // Bearer token for inbound requests from hospital (required)

	// Agent identity
	APIKey       string // x-api-key for authenticating TO the hospital server (legacy, required if AuthMethod=api_key)
	Framework    string // "openclaw" or "hermes" (default "openclaw")
	AgentName    string // Human-readable name (default hostname)

	// Ed25519 auth (new)
	AuthMethod       string // "api_key" or "ed25519" (default "api_key")
	PrivateKeyPath   string // Path to Ed25519 private key file (required if AuthMethod=ed25519)
	AgentFingerprint string // SHA-256 hex of public key (required if AuthMethod=ed25519)

	// Hospital server
	HospitalURL       string // Hospital server base URL (required)
	HeartbeatInterval  int    // Seconds between heartbeats (default 60)

	// Agent runtime
	StateDir     string // OpenClaw/Hermes home dir (default /home/themadme/.openclaw)
	SystemdUnit  string // Systemd unit to watch (default openclaw-gateway.service)
	GatewayURL   string // Gateway HTTP URL for health pings (empty = skip, default http://localhost:18789)
	GatewayPort  int    // Gateway port, used by kill-port repair (default 18789)
	LLMHealthURL  string // Optional: LLM provider health URL (e.g. http://localhost:3456/v1/models, https://openrouter.ai/api/v1/models)
	LLMHealthAuth string // Optional: auth header value for LLM health check (e.g. "Bearer sk-or-...", "x-api-key ak-...")

	// Extra units to monitor (comma-separated, e.g. "claude-max-api-proxy.service,other.service")
	ExtraUnits []string
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:              envOr("HOSPITAL_AGENT_PORT", "18793"),
		InboundToken:      os.Getenv("HOSPITAL_AGENT_INBOUND_TOKEN"),
		APIKey:            os.Getenv("HOSPITAL_AGENT_API_KEY"),
		Framework:         envOr("HOSPITAL_AGENT_FRAMEWORK", "openclaw"),
		AgentName:         envOr("HOSPITAL_AGENT_NAME", hostname()),
		AuthMethod:        envOr("HOSPITAL_AGENT_AUTH_METHOD", "api_key"),
		PrivateKeyPath:    os.Getenv("HOSPITAL_AGENT_PRIVATE_KEY_PATH"),
		AgentFingerprint:  os.Getenv("HOSPITAL_AGENT_FINGERPRINT"),
		HospitalURL:       os.Getenv("HOSPITAL_AGENT_HOSPITAL_URL"),
		HeartbeatInterval: envInt("HOSPITAL_AGENT_HEARTBEAT_INTERVAL", 60),
		StateDir:          envOr("HOSPITAL_AGENT_STATE_DIR", "/home/themadme/.openclaw"),
		SystemdUnit:       envOr("HOSPITAL_AGENT_SYSTEMD_UNIT", "openclaw-gateway.service"),
		GatewayURL:        os.Getenv("HOSPITAL_AGENT_GATEWAY_URL"), // empty = skip gateway health check
		GatewayPort:       envInt("HOSPITAL_AGENT_GATEWAY_PORT", 18789),
		LLMHealthURL:      os.Getenv("HOSPITAL_AGENT_LLM_HEALTH_URL"),  // optional, empty = skip LLM check
		LLMHealthAuth:     os.Getenv("HOSPITAL_AGENT_LLM_HEALTH_AUTH"), // optional auth header for LLM check
	}

	// Parse extra units
	if extra := os.Getenv("HOSPITAL_AGENT_EXTRA_UNITS"); extra != "" {
		for _, u := range strings.Split(extra, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				cfg.ExtraUnits = append(cfg.ExtraUnits, u)
			}
		}
	}

	// Auto-detect LLM config from openclaw.json if not manually overridden
	if cfg.LLMHealthURL == "" && cfg.Framework == "openclaw" {
		det, err := DetectLLM(cfg.StateDir, cfg.Framework)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: LLM auto-detect: %v\n", err)
		} else {
			cfg.LLMHealthURL = det.HealthURL
			cfg.LLMHealthAuth = det.AuthHeader
			fmt.Fprintf(os.Stderr, "INFO: Auto-detected LLM: %s -> %s\n", det.Provider, det.HealthURL)
		}
	}

	if cfg.InboundToken == "" {
		return nil, errors.New("HOSPITAL_AGENT_INBOUND_TOKEN is required")
	}
	if cfg.HospitalURL == "" {
		return nil, errors.New("HOSPITAL_AGENT_HOSPITAL_URL is required")
	}
	if cfg.Framework != "openclaw" && cfg.Framework != "hermes" {
		return nil, errors.New("HOSPITAL_AGENT_FRAMEWORK must be 'openclaw' or 'hermes'")
	}

	// Validate auth-method-specific fields
	switch cfg.AuthMethod {
	case "ed25519":
		if cfg.PrivateKeyPath == "" {
			return nil, errors.New("HOSPITAL_AGENT_PRIVATE_KEY_PATH is required when AUTH_METHOD=ed25519")
		}
		if cfg.AgentFingerprint == "" {
			return nil, errors.New("HOSPITAL_AGENT_FINGERPRINT is required when AUTH_METHOD=ed25519")
		}
	case "api_key":
		if cfg.APIKey == "" {
			return nil, errors.New("HOSPITAL_AGENT_API_KEY is required when AUTH_METHOD=api_key")
		}
	default:
		return nil, fmt.Errorf("HOSPITAL_AGENT_AUTH_METHOD must be 'api_key' or 'ed25519', got %q", cfg.AuthMethod)
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
