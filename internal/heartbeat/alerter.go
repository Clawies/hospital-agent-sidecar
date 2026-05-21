package heartbeat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/config"
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

// SendIntegrationAlert sends an integration failure alert to all configured channels.
func (a *Alerter) SendIntegrationAlert(integration string, err string) {
	msg := fmt.Sprintf("[Hospital Sidecar] Agent %s: %s integration is DOWN. %s",
		a.agentName, integration, err)
	a.broadcast(msg)
}

// SendMCPAlert sends an MCP server failure alert to all configured channels.
func (a *Alerter) SendMCPAlert(serverName, transport, errMsg string) {
	msg := fmt.Sprintf("[Hospital Sidecar] Agent %s: MCP server '%s' (%s) is UNREACHABLE. %s",
		a.agentName, serverName, transport, errMsg)
	a.broadcast(msg)
}

// --- Channel discovery from agent config ---
// Uses the shared config.DiscoverChannels reader.

func discoverChannels(stateDir, framework string, logger *slog.Logger, client *http.Client) []alertChannel {
	channelCfgs, _, err := config.DiscoverChannels(stateDir, framework)
	if err != nil {
		logger.Warn("cannot read agent config for channel discovery", "err", err)
		return nil
	}

	var channels []alertChannel

	for _, ch := range channelCfgs {
		if ch.Token == "" {
			continue
		}

		switch ch.Type {
		case "slack":
			if alertCh := os.Getenv("HOSPITAL_AGENT_ALERT_SLACK_CHANNEL"); alertCh != "" {
				channels = append(channels, alertChannel{Type: "slack", Token: ch.Token, TargetID: alertCh})
				logger.Info("slack alert channel from env", "channel", alertCh)
			} else {
				channelID := discoverSlackChannel(ch.Token, client, logger)
				if channelID != "" {
					channels = append(channels, alertChannel{Type: "slack", Token: ch.Token, TargetID: channelID})
				}
			}
		case "discord":
			if alertCh := os.Getenv("HOSPITAL_AGENT_ALERT_DISCORD_CHANNEL"); alertCh != "" {
				channels = append(channels, alertChannel{Type: "discord", Token: ch.Token, TargetID: alertCh})
			}
		case "telegram":
			if alertCh := os.Getenv("HOSPITAL_AGENT_ALERT_TELEGRAM_CHAT"); alertCh != "" {
				channels = append(channels, alertChannel{Type: "telegram", Token: ch.Token, TargetID: alertCh})
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

