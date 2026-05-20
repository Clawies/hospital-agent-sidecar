package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenerateKeypair creates a new Ed25519 keypair.
func GenerateKeypair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return pub, priv, nil
}

// SavePrivateKey writes the Ed25519 private key seed (32 bytes) to a file
// in base64 encoding with mode 0600.
func SavePrivateKey(path string, key ed25519.PrivateKey) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}

	seed := key.Seed()
	encoded := base64.StdEncoding.EncodeToString(seed)

	if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	return nil
}

// LoadPrivateKey reads a base64-encoded Ed25519 seed from file and reconstructs the full key.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}

	encoded := strings.TrimSpace(string(data))
	seed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}

	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("invalid seed size: got %d, want %d", len(seed), ed25519.SeedSize)
	}

	return ed25519.NewKeyFromSeed(seed), nil
}

// PublicKeyFromPrivate extracts the public key from a private key.
func PublicKeyFromPrivate(key ed25519.PrivateKey) ed25519.PublicKey {
	return key.Public().(ed25519.PublicKey)
}

// PublicKeyFingerprint returns the hex-encoded SHA-256 hash of a public key.
// Used as a stable identifier for fast lookups.
func PublicKeyFingerprint(pub ed25519.PublicKey) string {
	hash := sha256.Sum256(pub)
	return hex.EncodeToString(hash[:])
}

// PublicKeyBase64 returns the base64-encoded public key (for sending to hospital).
func PublicKeyBase64(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}
