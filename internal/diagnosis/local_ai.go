package diagnosis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawies/hospital-agent/internal/collector"
	"github.com/clawies/hospital-agent/internal/config"
)

// localAIDiagnose sends crash context to the local LLM proxy for diagnosis.
// Returns nil if the proxy is unavailable (caller should fall through to L0).
func localAIDiagnose(ctx context.Context, cfg *config.Config, logger *slog.Logger, cc collector.CrashContext) *DiagnosisResult {
	// Check if local AI is reachable (3s timeout)
	if !probeLocalAI(cfg.LocalAIURL) {
		logger.Info("local AI unavailable, falling through to pattern match")
		return nil
	}

	logger.Info("local AI available, sending crash context for diagnosis")

	prompt := buildCrashPrompt(cfg, cc)

	result, err := callLocalLLM(ctx, cfg, prompt)
	if err != nil {
		logger.Warn("local AI call failed", "err", err)
		return nil
	}

	return result
}

func probeLocalAI(baseURL string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(baseURL + "/models")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func buildCrashPrompt(cfg *config.Config, cc collector.CrashContext) chatRequest {
	system := fmt.Sprintf(`You are a diagnostic AI running on an agent VM (%s framework).
The agent gateway process has crashed. Analyze the crash data and recommend repairs.

AVAILABLE REPAIR ACTIONS (whitelist -- only recommend these):
- restart-gateway: reset-failed + restart the systemd unit
- kill-zombies: kill orphaned openclaw/node processes, then restart
- clear-logs: delete old logs (>7d), truncate large logs (>100MB)
- clear-disk-cache: prune old sessions/completions (>30d)
- emergency-disk: delete the 50MB reserve file to free emergency space
- kill-port: kill whatever is using the gateway port

RULES:
- Only recommend actions from the whitelist above
- Order by priority (most likely fix first)
- Be concise -- this runs on a resource-constrained VM
- If you're unsure, include restart-gateway as a safe default

Respond with JSON only:
{
  "summary": "brief diagnosis (1-2 sentences)",
  "repairs": ["action1", "action2"],
  "confidence": 0.0-1.0
}`, cfg.Framework)

	user := fmt.Sprintf(`CRASH DATA:
Unit: %s
Exit code: %d
Exit signal: %d (0=normal, 9=SIGKILL/OOM, 15=SIGTERM)

JOURNAL (last 50 lines):
%s

DMESG OOM:
%s

DISK: %d%% used (%dMB free of %dMB)
MEMORY: %d%% used (%dMB free of %dMB)

OPENCLAW LOG TAIL:
%s

Diagnose and recommend repairs. JSON only.`,
		cc.Unit, cc.ExitCode, cc.ExitSignal,
		truncate(cc.Journal, 3000),
		truncate(cc.DmesgOOM, 500),
		cc.DiskUsage.UsedPercent, cc.DiskUsage.AvailMB, cc.DiskUsage.TotalMB,
		cc.MemInfo.UsedPercent, cc.MemInfo.AvailMB, cc.MemInfo.TotalMB,
		truncate(cc.LogTail, 2000),
	)

	return chatRequest{
		Model:       cfg.LocalAIModel,
		Messages:    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: 0.1,
		MaxTokens:   800,
	}
}

func callLocalLLM(ctx context.Context, cfg *config.Config, req chatRequest) (*DiagnosisResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(tctx, http.MethodPost,
		cfg.LocalAIURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("empty response from LLM")
	}

	return parseLLMResponse(chatResp.Choices[0].Message.Content)
}

func parseLLMResponse(content string) (*DiagnosisResult, error) {
	// Strip code fences
	cleaned := strings.TrimSpace(content)
	if idx := strings.Index(cleaned, "```"); idx != -1 {
		// Find the JSON inside fences
		start := strings.Index(cleaned, "{")
		end := strings.LastIndex(cleaned, "}")
		if start != -1 && end > start {
			cleaned = cleaned[start : end+1]
		}
	} else {
		start := strings.Index(cleaned, "{")
		end := strings.LastIndex(cleaned, "}")
		if start != -1 && end > start {
			cleaned = cleaned[start : end+1]
		}
	}

	var parsed struct {
		Summary    string   `json:"summary"`
		Repairs    []string `json:"repairs"`
		Confidence float64  `json:"confidence"`
	}

	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("parse LLM JSON: %w (raw: %s)", err, truncate(content, 200))
	}

	// Filter to whitelist only
	var filtered []string
	whitelist := map[string]bool{
		"restart-gateway":  true,
		"kill-zombies":     true,
		"clear-logs":       true,
		"clear-disk-cache": true,
		"emergency-disk":   true,
		"kill-port":        true,
	}
	for _, r := range parsed.Repairs {
		if whitelist[r] {
			filtered = append(filtered, r)
		}
	}

	if len(filtered) == 0 {
		filtered = []string{"restart-gateway"}
	}

	return &DiagnosisResult{
		Layer:      "L1",
		Summary:    parsed.Summary,
		Repairs:    filtered,
		Confidence: parsed.Confidence,
	}, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
