package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type RegisterRequest struct {
	HostToken   string `json:"hostToken"`
	PublicKey   string `json:"publicKey"`   // base64-encoded Ed25519 public key
	Name        string `json:"name"`
	Framework   string `json:"framework"`
	CallbackURL string `json:"callbackUrl"`
}

type RegisterResponse struct {
	AgentID      string   `json:"agentId"`
	Capabilities []string `json:"capabilities"`
	InboundToken string   `json:"inboundToken"`
}

// RegisterAgent sends the agent's public key to the hospital server
// and receives an agent ID, capabilities, and inbound token.
func RegisterAgent(hospitalURL string, req RegisterRequest) (*RegisterResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal registration request: %w", err)
	}

	url := hospitalURL + "/api/v1/agents/register"
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("registration request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("registration failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result RegisterResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse registration response: %w", err)
	}

	if result.AgentID == "" {
		return nil, fmt.Errorf("registration response missing agentId")
	}

	return &result, nil
}
