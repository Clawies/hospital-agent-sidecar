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

type HostRegisterRequest struct {
	Name string `json:"name"`
}

type HostRegisterResponse struct {
	HostID          string `json:"hostId"`
	EnrollmentToken string `json:"enrollmentToken"`
}

// RegisterHost auto-creates a host on the hospital and returns an enrollment token.
// This mirrors the zero-config flow from @agent-hospital/client.
func RegisterHost(hospitalURL string, name string) (*HostRegisterResponse, error) {
	body, err := json.Marshal(HostRegisterRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("marshal host register request: %w", err)
	}

	url := hospitalURL + "/api/v1/hosts/register"
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("host register request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("host registration failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result HostRegisterResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse host register response: %w", err)
	}

	if result.EnrollmentToken == "" {
		return nil, fmt.Errorf("host registration response missing enrollmentToken")
	}

	return &result, nil
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
