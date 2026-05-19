package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LLMDetection holds auto-detected LLM health check parameters.
type LLMDetection struct {
	HealthURL  string // e.g. "https://openrouter.ai/api/v1/models"
	AuthHeader string // e.g. "Bearer sk-or-v1-..." or "" for no-auth local proxies
	Provider   string // e.g. "openrouter", "openai" -- for logging
}

var envVarPattern = regexp.MustCompile(`^\$\{(.+)\}$`)

// DetectLLM reads the agent framework config file and extracts the primary
// LLM provider's base URL and API key. Returns detection result or error.
// Caller should treat errors as non-fatal (log warning, skip LLM health check).
func DetectLLM(stateDir, framework string) (*LLMDetection, error) {
	if framework != "openclaw" {
		return nil, fmt.Errorf("auto-detect not supported for framework %q (use manual LLM_HEALTH_URL)", framework)
	}

	configPath := filepath.Join(stateDir, "openclaw.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configPath, err)
	}

	// Extract env section for variable resolution
	envMap, _ := getNestedMap(root, "env")

	// Find primary model: agents.defaults.model.primary
	primaryModel, ok := getNestedString(root, "agents", "defaults", "model", "primary")
	if !ok || primaryModel == "" {
		return nil, fmt.Errorf("agents.defaults.model.primary not found in %s", configPath)
	}

	// Split provider from model: "openrouter/z-ai/glm-4.7" -> provider="openrouter"
	providerName, _ := splitProvider(primaryModel)
	if providerName == "" {
		return nil, fmt.Errorf("cannot extract provider from primary model %q", primaryModel)
	}

	// Look up provider config: models.providers.<provider>
	providerCfg, ok := getNestedMap(root, "models", "providers", providerName)
	if !ok {
		return nil, fmt.Errorf("provider %q not found in models.providers", providerName)
	}

	// Get baseUrl
	baseURL, ok := providerCfg["baseUrl"].(string)
	if !ok || baseURL == "" {
		return nil, fmt.Errorf("provider %q has no baseUrl", providerName)
	}

	// Get apiKey -- may be absent, empty, "${VAR}", "not-needed", or a raw key
	apiKey := resolveAPIKey(providerCfg, envMap, providerName)

	// Construct health URL: baseUrl + "/models"
	healthURL := strings.TrimRight(baseURL, "/") + "/models"

	// Construct auth header based on provider
	authHeader := buildAuthHeader(apiKey, baseURL)

	return &LLMDetection{
		HealthURL:  healthURL,
		AuthHeader: authHeader,
		Provider:   providerName,
	}, nil
}

// splitProvider splits "openrouter/z-ai/glm-4.7" into ("openrouter", "z-ai/glm-4.7").
// Returns ("", "") if no slash found.
func splitProvider(model string) (provider, modelID string) {
	idx := strings.IndexByte(model, '/')
	if idx < 0 {
		return "", model
	}
	return model[:idx], model[idx+1:]
}

// resolveAPIKey extracts the API key from provider config, resolving ${VAR} patterns
// and falling back to well-known env var names.
func resolveAPIKey(providerCfg, envMap map[string]any, providerName string) string {
	// Try apiKey from provider config
	if raw, ok := providerCfg["apiKey"].(string); ok && raw != "" {
		// Check for ${VAR_NAME} pattern
		if matches := envVarPattern.FindStringSubmatch(raw); len(matches) == 2 {
			varName := matches[1]
			if val, ok := envMap[varName].(string); ok && val != "" && val != "not-needed" {
				return val
			}
			// Also try OS env (the var might be set at process level)
			if val := os.Getenv(varName); val != "" {
				return val
			}
			return ""
		}
		// Raw key value
		if raw != "not-needed" {
			return raw
		}
		return ""
	}

	// No apiKey in provider config -- try well-known env var names
	wellKnown := wellKnownAPIKeyVar(providerName)
	if wellKnown != "" {
		// Try env section in openclaw.json
		if val, ok := envMap[wellKnown].(string); ok && val != "" && val != "not-needed" {
			return val
		}
		// Try OS env
		if val := os.Getenv(wellKnown); val != "" {
			return val
		}
	}

	return ""
}

// wellKnownAPIKeyVar returns the conventional env var name for a provider.
func wellKnownAPIKeyVar(provider string) string {
	switch provider {
	case "openrouter":
		return "OPENROUTER_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}

// buildAuthHeader constructs the appropriate auth header string.
// For Anthropic API URLs, uses "x-api-key <key>".
// For everything else (OpenAI-compatible), uses "Bearer <key>".
// Returns "" if no API key (local proxy, no auth needed).
func buildAuthHeader(apiKey, baseURL string) string {
	if apiKey == "" {
		return ""
	}
	if strings.Contains(baseURL, "anthropic.com") {
		return "x-api-key " + apiKey
	}
	return "Bearer " + apiKey
}

// getNestedString navigates a map by keys and returns the leaf as a string.
func getNestedString(m map[string]any, keys ...string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	current := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			return "", false
		}
		current = next
	}
	val, ok := current[keys[len(keys)-1]].(string)
	return val, ok
}

// getNestedMap navigates a map by keys and returns the leaf as a sub-map.
func getNestedMap(m map[string]any, keys ...string) (map[string]any, bool) {
	if len(keys) == 0 {
		return m, true
	}
	current := m
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}
