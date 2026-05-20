package heartbeat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/watcher"
)

// Alerter sends alerts directly to the agent's connected messaging channels
// (Slack, Discord, Telegram) by reading credentials from openclaw.json.
// Bypasses the gateway entirely -- works even when the gateway is crashed.
type Alerter struct {
	logger    *slog.Logger
	channels  []alertChannel
	client    *http.Client
	agentName string
}

type alertChannel struct {
	Type     string // "slack", "discord", "telegram"
	Token    string // bot token
	TargetID string // channel ID (Slack/Discord) or chat ID (Telegram)
}

// NewAlerter reads openclaw.json to discover connected channels.
// Returns nil if no channels are configured (caller should nil-check).
func NewAlerter(stateDir, framework, agentName string, logger *slog.Logger) *Alerter {
	client := &http.Client{Timeout: 10 * time.Second}
	channels := discoverChannels(stateDir, framework, logger, client)

	// Also check env var override: HOSPITAL_AGENT_ALERT_CHANNEL=slack:C0AR21ZS35J
	if envCh := os.Getenv("HOSPITAL_AGENT_ALERT_CHANNEL"); envCh != "" {
		parts := strings.SplitN(envCh, ":", 2)
		if len(parts) == 2 {
			// Need to find the matching token from discovered channels
			for _, ch := range channels {
				if ch.Type == parts[0] {
					// Already have this type with a discovered channel -- replace target
					ch.TargetID = parts[1]
					break
				}
			}
			logger.Info("alert channel override from env", "type", parts[0], "target", parts[1])
		}
	}

	if len(channels) == 0 {
		logger.Info("no messaging channels found for direct alerting")
		return nil
	}

	logger.Info("direct alerter initialized", "channels", len(channels), "types", channelTypes(channels))

	return &Alerter{
		logger:    logger,
		channels:  channels,
		agentName: agentName,
		client:    client,
	}
}

func channelTypes(channels []alertChannel) string {
	types := make([]string, len(channels))
	for i, ch := range channels {
		types[i] = ch.Type
	}
	return strings.Join(types, ",")
}

// SendCrashAlert sends a crash notification to all configured channels.
func (a *Alerter) SendCrashAlert(ev watcher.CrashEvent) {
	summary := ev.L0Diagnosis.Summary
	repairStatus := "unknown"
	for _, r := range ev.LocalRepairs {
		if r.Action == "restart-gateway" || strings.HasPrefix(r.Action, "restart-") {
			if r.Success {
				repairStatus = "auto-repaired"
			} else {
				repairStatus = "repair FAILED -- manual intervention needed"
			}
			break
		}
	}

	msg := fmt.Sprintf("[Hospital Sidecar] Agent %s crashed.\nDiagnosis: %s\nStatus: %s",
		a.agentName, summary, repairStatus)

	a.broadcast(msg)
}

// SendLLMAlert sends an LLM provider status alert to all configured channels.
func (a *Alerter) SendLLMAlert(ev watcher.LLMStatusEvent) {
	var msg string
	switch ev.Category {
	case "auth_expired":
		msg = fmt.Sprintf("[Hospital Sidecar] Agent %s: LLM API key expired or invalid (%s, HTTP %d). Agent cannot process AI requests until key is updated.",
			a.agentName, ev.Provider, ev.HTTPCode)
	case "credits_exhausted":
		msg = fmt.Sprintf("[Hospital Sidecar] Agent %s: LLM credits exhausted (%s, HTTP 429). Agent is rate-limited. Top up credits or switch provider.",
			a.agentName, ev.Provider)
	case "provider_outage":
		msg = fmt.Sprintf("[Hospital Sidecar] Agent %s: LLM provider outage (%s, HTTP %d). Waiting for provider recovery.",
			a.agentName, ev.Provider, ev.HTTPCode)
	case "integration_failure":
		msg = fmt.Sprintf("[Hospital Sidecar] Agent %s: LLM proxy unreachable (%s). Check if the proxy service is running.",
			a.agentName, ev.Provider)
	default:
		msg = fmt.Sprintf("[Hospital Sidecar] Agent %s: LLM health check failed (%s). %s",
			a.agentName, ev.Provider, ev.Detail)
	}

	a.broadcast(msg)
}

// SendResourceAlert sends disk/memory warning to all configured channels.
func (a *Alerter) SendResourceAlert(resource string, usedPercent int, detail string) {
	msg := fmt.Sprintf("[Hospital Sidecar] Agent %s: %s at %d%%. %s",
		a.agentName, resource, usedPercent, detail)
	a.broadcast(msg)
}

func (a *Alerter) broadcast(msg string) {
	for _, ch := range a.channels {
		if err := a.send(ch, msg); err != nil {
			a.logger.Warn("alert send failed", "type", ch.Type, "err", err)
		} else {
			a.logger.Info("alert sent", "type", ch.Type, "target", ch.TargetID)
		}
	}
}

func (a *Alerter) send(ch alertChannel, msg string) error {
	switch ch.Type {
	case "slack":
		return a.sendSlack(ch.Token, ch.TargetID, msg)
	case "discord":
		return a.sendDiscord(ch.Token, ch.TargetID, msg)
	case "telegram":
		return a.sendTelegram(ch.Token, ch.TargetID, msg)
	default:
		return fmt.Errorf("unknown channel type: %s", ch.Type)
	}
}

func (a *Alerter) sendSlack(token, channelID, msg string) error {
	body, _ := json.Marshal(map[string]string{
		"channel": channelID,
		"text":    msg,
	})

	req, err := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("slack HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (a *Alerter) sendDiscord(token, channelID, msg string) error {
	body, _ := json.Marshal(map[string]string{
		"content": msg,
	})

	url := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages", channelID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bot "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discord HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (a *Alerter) sendTelegram(token, chatID, msg string) error {
	body, _ := json.Marshal(map[string]string{
		"chat_id": chatID,
		"text":    msg,
	})

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// --- Channel discovery from openclaw.json ---

func discoverChannels(stateDir, framework string, logger *slog.Logger, client *http.Client) []alertChannel {
	var configPath string
	if framework == "hermes" {
		configPath = filepath.Join(stateDir, "hermes.json")
	} else {
		configPath = filepath.Join(stateDir, "openclaw.json")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Warn("cannot read agent config for channel discovery", "path", configPath, "err", err)
		return nil
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		logger.Warn("cannot parse agent config", "path", configPath, "err", err)
		return nil
	}

	var channels []alertChannel

	// Extract env section for resolving ${VAR} patterns in tokens
	envMap, _ := root["env"].(map[string]any)

	channelsCfg, ok := root["channels"].(map[string]any)
	if !ok {
		return nil
	}

	// Slack: token is in botToken, but channel IDs aren't stored in openclaw.json
	// (OpenClaw uses wildcard "*" channels). We use the Slack API to discover
	// which channels the bot is a member of, and pick the first one.
	if slack, ok := channelsCfg["slack"].(map[string]any); ok {
		enabled, _ := slack["enabled"].(bool)
		token := resolveValue(slack["botToken"], envMap)
		if token != "" && enabled {
			// Check for explicit channel ID in env override first
			if alertCh := os.Getenv("HOSPITAL_AGENT_ALERT_SLACK_CHANNEL"); alertCh != "" {
				channels = append(channels, alertChannel{Type: "slack", Token: token, TargetID: alertCh})
				logger.Info("slack alert channel from env", "channel", alertCh)
			} else {
				// Discover channels via Slack API
				channelID := discoverSlackChannel(token, client, logger)
				if channelID != "" {
					channels = append(channels, alertChannel{Type: "slack", Token: token, TargetID: channelID})
				}
			}
		}
	}

	// Discord: similar structure
	if discord, ok := channelsCfg["discord"].(map[string]any); ok {
		enabled, _ := discord["enabled"].(bool)
		token := resolveValue(discord["botToken"], envMap)
		if token != "" && enabled {
			if alertCh := os.Getenv("HOSPITAL_AGENT_ALERT_DISCORD_CHANNEL"); alertCh != "" {
				channels = append(channels, alertChannel{Type: "discord", Token: token, TargetID: alertCh})
			}
			// Discord channel discovery would require listing guilds + channels, skip for now
		}
	}

	// Telegram
	if telegram, ok := channelsCfg["telegram"].(map[string]any); ok {
		enabled, _ := telegram["enabled"].(bool)
		token := resolveValue(telegram["botToken"], envMap)
		if token != "" && enabled {
			if alertCh := os.Getenv("HOSPITAL_AGENT_ALERT_TELEGRAM_CHAT"); alertCh != "" {
				channels = append(channels, alertChannel{Type: "telegram", Token: token, TargetID: alertCh})
			}
		}
	}

	return channels
}

// discoverSlackChannel uses the Slack API to find the first channel the bot is a member of.
// Prefers channels named "alerts", "ops", "monitoring" if found; otherwise picks the first.
func discoverSlackChannel(token string, client *http.Client, logger *slog.Logger) string {
	req, err := http.NewRequest("GET",
		"https://slack.com/api/conversations.list?types=public_channel,private_channel&exclude_archived=true&limit=200",
		nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("slack channel discovery failed", "err", err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))

	var result struct {
		OK       bool `json:"ok"`
		Channels []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			IsMember bool   `json:"is_member"`
		} `json:"channels"`
	}

	if err := json.Unmarshal(body, &result); err != nil || !result.OK {
		logger.Warn("slack channel discovery: invalid response")
		return ""
	}

	// Filter to channels the bot is a member of
	var memberChannels []struct {
		ID   string
		Name string
	}
	for _, ch := range result.Channels {
		if ch.IsMember {
			memberChannels = append(memberChannels, struct {
				ID   string
				Name string
			}{ch.ID, ch.Name})
		}
	}

	if len(memberChannels) == 0 {
		logger.Warn("slack: bot is not a member of any channel")
		return ""
	}

	// Prefer channels with alert-related names
	preferredNames := []string{"alerts", "ops", "monitoring", "agent-alerts", "hospital", "general"}
	for _, preferred := range preferredNames {
		for _, ch := range memberChannels {
			if ch.Name == preferred {
				logger.Info("slack alert channel discovered", "channel", ch.Name, "id", ch.ID)
				return ch.ID
			}
		}
	}

	// Fall back to first channel
	logger.Info("slack alert channel: using first member channel", "channel", memberChannels[0].Name, "id", memberChannels[0].ID)
	return memberChannels[0].ID
}

// resolveValue handles ${VAR} patterns and raw strings for config values.
func resolveValue(val any, envMap map[string]any) string {
	s, ok := val.(string)
	if !ok || s == "" {
		return ""
	}

	// Check for ${VAR_NAME} pattern
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		varName := s[2 : len(s)-1]
		// Try envMap from openclaw.json first
		if envMap != nil {
			if v, ok := envMap[varName].(string); ok && v != "" {
				return v
			}
		}
		// Try OS env
		if v := os.Getenv(varName); v != "" {
			return v
		}
		return ""
	}

	return s
}
