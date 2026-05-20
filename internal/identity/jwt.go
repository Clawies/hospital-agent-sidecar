package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// AgentClaims are the payload fields for an agent-signed JWT.
type AgentClaims struct {
	Sub   string `json:"sub"`   // agent fingerprint (pubkey sha256 hex)
	Iss   string `json:"iss"`   // "agent"
	Aud   string `json:"aud"`   // hospital URL
	Iat   int64  `json:"iat"`   // issued at (unix seconds)
	Exp   int64  `json:"exp"`   // expires at (iat + 60s)
	Nonce string `json:"nonce"` // random 16 bytes hex (replay protection)
	Act   string `json:"act"`   // action: "heartbeat", "crash-report", etc.
}

// Static JWT header for EdDSA agent tokens.
var jwtHeaderB64 = base64URLEncode([]byte(`{"alg":"EdDSA","typ":"agent+jwt"}`))

// SignAgentJWT creates a signed JWT token using the agent's Ed25519 private key.
// The token expires in 60 seconds and includes a random nonce.
func SignAgentJWT(key ed25519.PrivateKey, fingerprint, audience, action string) (string, error) {
	now := time.Now().Unix()

	nonce, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	claims := AgentClaims{
		Sub:   fingerprint,
		Iss:   "agent",
		Aud:   audience,
		Iat:   now,
		Exp:   now + 60,
		Nonce: nonce,
		Act:   action,
	}

	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	payloadB64 := base64URLEncode(payloadJSON)

	// Sign: header.payload
	signingInput := jwtHeaderB64 + "." + payloadB64
	signature := ed25519.Sign(key, []byte(signingInput))
	sigB64 := base64URLEncode(signature)

	return signingInput + "." + sigB64, nil
}

// base64URLEncode encodes bytes as base64url (no padding) per RFC 7515.
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// randomHex generates n random bytes and returns them as a hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
