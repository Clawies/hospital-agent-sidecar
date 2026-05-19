package config

import (
	"errors"
	"os"
	"strconv"
)

type Config struct {
	// Server
	Port          string // Listen port (default 18791)
	InboundToken  string // Bearer token for inbound requests from hospital (required)

	// Agent identity
	APIKey       string // x-api-key for authenticating TO the hospital server (required)
	Framework    string // "openclaw" or "hermes" (default "openclaw")
	AgentName    string // Human-readable name (default hostname)

	// Hospital server
	HospitalURL       string // Hospital server base URL (required)
	HeartbeatInterval  int    // Seconds between heartbeats (default 60)

	// Agent runtime
	StateDir     string // OpenClaw/Hermes home dir (default /home/themadme/.openclaw)
	SystemdUnit  string // Systemd unit to watch (default openclaw-gateway.service)
	GatewayURL   string // Gateway HTTP URL for health pings (default http://localhost:18789)
	GatewayPort  int    // Gateway port, used by kill-port repair (default 18789)
	LLMHealthURL string // Optional: LLM proxy health URL (e.g. http://localhost:3456/v1/models)
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:              envOr("HOSPITAL_AGENT_PORT", "18792"),
		InboundToken:      os.Getenv("HOSPITAL_AGENT_INBOUND_TOKEN"),
		APIKey:            os.Getenv("HOSPITAL_AGENT_API_KEY"),
		Framework:         envOr("HOSPITAL_AGENT_FRAMEWORK", "openclaw"),
		AgentName:         envOr("HOSPITAL_AGENT_NAME", hostname()),
		HospitalURL:       os.Getenv("HOSPITAL_AGENT_HOSPITAL_URL"),
		HeartbeatInterval: envInt("HOSPITAL_AGENT_HEARTBEAT_INTERVAL", 60),
		StateDir:          envOr("HOSPITAL_AGENT_STATE_DIR", "/home/themadme/.openclaw"),
		SystemdUnit:       envOr("HOSPITAL_AGENT_SYSTEMD_UNIT", "openclaw-gateway.service"),
		GatewayURL:        envOr("HOSPITAL_AGENT_GATEWAY_URL", "http://localhost:18789"),
		GatewayPort:       envInt("HOSPITAL_AGENT_GATEWAY_PORT", 18789),
		LLMHealthURL:      os.Getenv("HOSPITAL_AGENT_LLM_HEALTH_URL"), // optional, empty = skip LLM check
	}

	if cfg.InboundToken == "" {
		return nil, errors.New("HOSPITAL_AGENT_INBOUND_TOKEN is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("HOSPITAL_AGENT_API_KEY is required")
	}
	if cfg.HospitalURL == "" {
		return nil, errors.New("HOSPITAL_AGENT_HOSPITAL_URL is required")
	}
	if cfg.Framework != "openclaw" && cfg.Framework != "hermes" {
		return nil, errors.New("HOSPITAL_AGENT_FRAMEWORK must be 'openclaw' or 'hermes'")
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
