package heartbeat

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/collector"
	"github.com/clawies/hospital-agent-sidecar/internal/config"
	"github.com/clawies/hospital-agent-sidecar/internal/identity"
	"github.com/clawies/hospital-agent-sidecar/internal/repair"
	"github.com/clawies/hospital-agent-sidecar/internal/watcher"
)

type Heartbeat struct {
	cfg        *config.Config
	logger     *slog.Logger
	watcher    *watcher.Watcher
	collector  *collector.Collector
	alerter    *Alerter
	version    string
	client     *http.Client
	privateKey ed25519.PrivateKey // nil if using api_key auth
}

func New(cfg *config.Config, logger *slog.Logger, w *watcher.Watcher, coll *collector.Collector, version string) (*Heartbeat, error) {
	hb := &Heartbeat{
		cfg:       cfg,
		logger:    logger,
		watcher:   w,
		collector: coll,
		version:   version,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}

	// Load Ed25519 private key if using keypair auth
	if cfg.AuthMethod == "ed25519" {
		key, err := identity.LoadPrivateKey(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load agent private key: %w", err)
		}
		hb.privateKey = key
		logger.Info("Ed25519 auth initialized", "fingerprint", cfg.AgentFingerprint[:16]+"...")
	}

	return hb, nil
}

// SetAlerter attaches the direct channel alerter (optional, set after construction).
func (h *Heartbeat) SetAlerter(a *Alerter) {
	h.alerter = a
}

// Start begins the heartbeat ticker, crash report listener, LLM status listener,
// resource alert listener, integration status listener, and MCP status listener.
func (h *Heartbeat) Start(ctx context.Context) {
	go h.tickerLoop(ctx)
	go h.crashListener(ctx)
	go h.llmStatusListener(ctx)
	go h.resourceListener(ctx)
	go h.integrationStatusListener(ctx)
	go h.mcpStatusListener(ctx)
}

func (h *Heartbeat) tickerLoop(ctx context.Context) {
	interval := time.Duration(h.cfg.HeartbeatInterval) * time.Second
	h.logger.Info("heartbeat started", "interval", interval, "hospital", h.cfg.HospitalURL)

	// Send first heartbeat immediately
	h.sendHeartbeat(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			h.logger.Info("heartbeat stopped")
			return
		case <-ticker.C:
			h.sendHeartbeat(ctx)
		}
	}
}

func (h *Heartbeat) crashListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.watcher.CrashCh:
			// Send direct alert via connected channels
			if h.alerter != nil {
				h.alerter.SendCrashAlert(ev)
			}
			h.pushCrashReport(ctx, ev)
		}
	}
}

func (h *Heartbeat) resourceListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.watcher.ResourceCh:
			if h.alerter != nil {
				h.alerter.SendResourceAlert(ev.Resource, ev.UsedPercent, ev.Detail)
			}
		}
	}
}

func (h *Heartbeat) llmStatusListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.watcher.LLMStatusCh:
			h.pushLLMStatus(ctx, ev)
			// Also send direct alert via connected channels
			if h.alerter != nil {
				h.alerter.SendLLMAlert(ev)
			}
		}
	}
}

func (h *Heartbeat) integrationStatusListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evs := <-h.watcher.IntegrationStatusCh:
			h.pushIntegrationStatus(ctx, evs)
			// Send direct alerts for failed integrations
			if h.alerter != nil {
				for _, ev := range evs {
					if !ev.Connected && ev.Error != "" {
						h.alerter.SendIntegrationAlert(ev.Integration, ev.Error)
					}
				}
			}
		}
	}
}

func (h *Heartbeat) mcpStatusListener(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evs := <-h.watcher.MCPStatusCh:
			h.pushMCPStatus(ctx, evs)
			// Send direct alerts for dead MCP servers
			if h.alerter != nil {
				for _, ev := range evs {
					if !ev.Alive && ev.Error != "" {
						h.alerter.SendMCPAlert(ev.ServerName, ev.Transport, ev.Error)
					}
				}
			}
		}
	}
}

// --- Integration Status ---

type integrationStatusPayload struct {
	AgentName    string                          `json:"agentName"`
	Framework    string                          `json:"framework"`
	Integrations []watcher.IntegrationStatusEvent `json:"integrations"`
}

func (h *Heartbeat) pushIntegrationStatus(ctx context.Context, evs []watcher.IntegrationStatusEvent) {
	payload := integrationStatusPayload{
		AgentName:    h.cfg.AgentName,
		Framework:    h.cfg.Framework,
		Integrations: evs,
	}

	_, err := h.postJSON(ctx, "/api/v1/heartbeat/integration-status", payload)
	if err != nil {
		h.logger.Warn("integration status push failed", "err", err)
	}
}

// --- MCP Status ---

type mcpStatusPayload struct {
	AgentName  string                   `json:"agentName"`
	Framework  string                   `json:"framework"`
	MCPServers []watcher.MCPStatusEvent `json:"mcpServers"`
}

func (h *Heartbeat) pushMCPStatus(ctx context.Context, evs []watcher.MCPStatusEvent) {
	payload := mcpStatusPayload{
		AgentName:  h.cfg.AgentName,
		Framework:  h.cfg.Framework,
		MCPServers: evs,
	}

	_, err := h.postJSON(ctx, "/api/v1/heartbeat/mcp-status", payload)
	if err != nil {
		h.logger.Warn("MCP status push failed", "err", err)
	}
}

// --- LLM Status ---

type llmStatusPayload struct {
	AgentName string `json:"agentName"`
	Framework string `json:"framework"`
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	Category  string `json:"category"`
	HTTPCode  int    `json:"httpCode,omitempty"`
	Endpoint  string `json:"endpoint"`
	Detail    string `json:"detail"`
}

func (h *Heartbeat) pushLLMStatus(ctx context.Context, ev watcher.LLMStatusEvent) {
	payload := llmStatusPayload{
		AgentName: h.cfg.AgentName,
		Framework: h.cfg.Framework,
		Provider:  ev.Provider,
		Status:    ev.Status,
		Category:  ev.Category,
		HTTPCode:  ev.HTTPCode,
		Endpoint:  ev.Endpoint,
		Detail:    ev.Detail,
	}

	h.logger.Info("pushing LLM status to hospital",
		"provider", ev.Provider,
		"status", ev.Status,
		"category", ev.Category,
	)

	_, err := h.postJSON(ctx, "/api/v1/heartbeat/llm-status", payload)
	if err != nil {
		// Not fatal -- hospital might not have this endpoint yet
		h.logger.Warn("LLM status push failed (hospital may not support this endpoint yet)", "err", err)
	}
}

// --- Heartbeat ---

type heartbeatPayload struct {
	AgentName      string `json:"agentName"`
	Framework      string `json:"framework"`
	Status         string `json:"status"`
	SidecarVersion string `json:"sidecarVersion"`
	CallbackURL    string `json:"callbackUrl"`
	CrashCount     int    `json:"crashCount"`
	Metrics        any    `json:"metrics,omitempty"`

	// Enriched probe data from sidecar
	Integrations []watcher.IntegrationStatusEvent `json:"integrations,omitempty"`
	MCPServers   []watcher.MCPStatusEvent         `json:"mcpServers,omitempty"`
	Sessions     *watcher.SessionSnapshot         `json:"sessions,omitempty"`
	CronHealth   []watcher.CronSnapshot           `json:"cronHealth,omitempty"`
}

type heartbeatResponse struct {
	Ack      bool            `json:"ack"`
	Commands []remoteCommand `json:"commands,omitempty"`
}

type remoteCommand struct {
	Action      string `json:"action"`
	Description string `json:"description,omitempty"`
}

func (h *Heartbeat) sendHeartbeat(ctx context.Context) {
	state := h.watcher.State()
	sysInfo := h.collector.SystemInfo()

	payload := heartbeatPayload{
		AgentName:      h.cfg.AgentName,
		Framework:      h.cfg.Framework,
		Status:         string(state.Status),
		SidecarVersion: h.version,
		CallbackURL:    fmt.Sprintf("http://localhost:%s", h.cfg.Port),
		CrashCount:     state.CrashCount,
		Metrics:        sysInfo,
		Integrations:   h.watcher.LatestIntegrations(),
		MCPServers:     h.watcher.LatestMCPServers(),
		Sessions:       h.watcher.LatestSessions(),
		CronHealth:     h.watcher.LatestCronHealth(),
	}

	resp, err := h.postJSON(ctx, "/api/v1/heartbeat", payload)
	if err != nil {
		h.logger.Warn("heartbeat failed", "err", err)
		return
	}

	var hbResp heartbeatResponse
	if err := json.Unmarshal(resp, &hbResp); err != nil {
		h.logger.Warn("heartbeat response parse failed", "err", err)
		return
	}

	// Process inline commands from hospital
	if len(hbResp.Commands) > 0 {
		h.logger.Info("hospital sent inline commands", "count", len(hbResp.Commands))
		h.executeRemoteCommands(hbResp.Commands)
	}
}

// --- Crash Report ---
// Sends crash context + L0 results to hospital.
// Hospital performs AI diagnosis and responds with repair commands.

type crashPayload struct {
	AgentName      string          `json:"agentName"`
	Framework      string          `json:"framework"`
	SidecarVersion string          `json:"sidecarVersion"`
	CrashContext   any             `json:"crashContext"`
	L0Diagnosis    any             `json:"l0Diagnosis"`
	LocalRepairs   []repair.Result `json:"localRepairs"`
	Timestamp      time.Time       `json:"timestamp"`
}

type crashResponse struct {
	Ack       bool            `json:"ack"`
	Diagnosis *aiDiagnosis    `json:"diagnosis,omitempty"`
	Commands  []remoteCommand `json:"commands,omitempty"`
}

type aiDiagnosis struct {
	Category    string  `json:"category"`
	RootCause   string  `json:"rootCause"`
	Confidence  float64 `json:"confidence"`
	Severity    string  `json:"severity"`
	SuggestedFix string `json:"suggestedFix"`
}

func (h *Heartbeat) pushCrashReport(ctx context.Context, ev watcher.CrashEvent) {
	payload := crashPayload{
		AgentName:      h.cfg.AgentName,
		Framework:      h.cfg.Framework,
		SidecarVersion: h.version,
		CrashContext:   ev.Context,
		L0Diagnosis:    ev.L0Diagnosis,
		LocalRepairs:   ev.LocalRepairs,
		Timestamp:      ev.Timestamp,
	}

	h.logger.Info("pushing crash report to hospital for AI diagnosis")

	// Use a longer timeout for crash reports -- hospital runs AI diagnosis (~30-45s)
	crashClient := &http.Client{Timeout: 90 * time.Second}
	resp, err := h.postJSONWith(ctx, crashClient, "/api/v1/heartbeat/crash", payload)
	if err != nil {
		h.logger.Warn("crash report push failed", "err", err)
		return
	}

	var crashResp crashResponse
	if err := json.Unmarshal(resp, &crashResp); err != nil {
		h.logger.Warn("crash response parse failed", "err", err)
		return
	}

	// Log the hospital's AI diagnosis
	if crashResp.Diagnosis != nil {
		h.logger.Info("hospital AI diagnosis received",
			"category", crashResp.Diagnosis.Category,
			"rootCause", crashResp.Diagnosis.RootCause,
			"confidence", crashResp.Diagnosis.Confidence,
			"severity", crashResp.Diagnosis.Severity,
		)
	}

	// Execute hospital-prescribed repair commands (AI-informed)
	if len(crashResp.Commands) > 0 {
		h.logger.Info("hospital prescribed AI-diagnosed repairs", "count", len(crashResp.Commands))
		h.executeRemoteCommands(crashResp.Commands)
	} else {
		h.logger.Info("hospital returned no additional commands")
	}
}

// --- Remote command execution ---

func (h *Heartbeat) executeRemoteCommands(commands []remoteCommand) {
	repairer := h.watcher.Repairer()

	var results []repair.Result
	for _, cmd := range commands {
		h.logger.Info("executing hospital command", "action", cmd.Action)
		result := repairer.Execute(cmd.Action)
		results = append(results, result)

		// If restart succeeded, wait before executing more
		if cmd.Action == "restart-gateway" && result.Success {
			h.logger.Info("restart succeeded after hospital command, waiting grace")
			time.Sleep(10 * time.Second)
		}
	}

	// Report results back
	if len(results) > 0 {
		h.pushRepairResults(context.Background(), results)
	}
}

type repairResultPayload struct {
	AgentName string          `json:"agentName"`
	Framework string          `json:"framework"`
	Results   []repair.Result `json:"results"`
}

func (h *Heartbeat) pushRepairResults(ctx context.Context, results []repair.Result) {
	payload := repairResultPayload{
		AgentName: h.cfg.AgentName,
		Framework: h.cfg.Framework,
		Results:   results,
	}

	_, err := h.postJSON(ctx, "/api/v1/heartbeat/repair-result", payload)
	if err != nil {
		h.logger.Warn("repair result push failed", "err", err)
	}
}

// --- HTTP helper ---

func (h *Heartbeat) postJSON(ctx context.Context, path string, body any) ([]byte, error) {
	return h.postJSONWith(ctx, h.client, path, body)
}

func (h *Heartbeat) postJSONWith(ctx context.Context, client *http.Client, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	url := h.cfg.HospitalURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if h.cfg.AuthMethod == "ed25519" && h.privateKey != nil {
		action := pathToAction(path)
		token, err := identity.SignAgentJWT(h.privateKey, h.cfg.AgentFingerprint, h.cfg.HospitalURL, action)
		if err != nil {
			return nil, fmt.Errorf("sign agent JWT: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("x-api-key", h.cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 200)]))
	}

	return respBody, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// pathToAction maps API path to capability action name for JWT signing.
func pathToAction(path string) string {
	switch {
	case strings.HasSuffix(path, "/heartbeat/crash"):
		return "crash-report"
	case strings.HasSuffix(path, "/heartbeat/repair-result"):
		return "repair-execute"
	case strings.HasSuffix(path, "/heartbeat/llm-status"):
		return "llm-status"
	case strings.HasSuffix(path, "/heartbeat/integration-status"):
		return "integration-status"
	case strings.HasSuffix(path, "/heartbeat/mcp-status"):
		return "mcp-status"
	case strings.HasSuffix(path, "/heartbeat"):
		return "heartbeat"
	case strings.HasSuffix(path, "/heal"):
		return "heal"
	default:
		return "unknown"
	}
}
