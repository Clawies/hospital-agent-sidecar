package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ChannelConfig represents a discovered messaging channel from the agent config.
type ChannelConfig struct {
	Type    string // "slack", "discord", "telegram", "whatsapp"
	Enabled bool
	Token   string // resolved from ${VAR} patterns
}

// DiscoverChannels reads the agent config file and extracts enabled channels with their tokens.
func DiscoverChannels(stateDir, framework string) ([]ChannelConfig, map[string]any, error) {
	var configPath string
	if framework == "hermes" {
		configPath = filepath.Join(stateDir, "hermes.json")
	} else {
		configPath = filepath.Join(stateDir, "openclaw.json")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, err
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, err
	}

	envMap, _ := root["env"].(map[string]any)
	channelsCfg, ok := root["channels"].(map[string]any)
	if !ok {
		return nil, envMap, nil
	}

	var channels []ChannelConfig

	for _, chType := range []string{"slack", "discord", "telegram", "whatsapp"} {
		chCfg, ok := channelsCfg[chType].(map[string]any)
		if !ok {
			continue
		}

		enabled := true
		if e, ok := chCfg["enabled"].(bool); ok {
			enabled = e
		}
		if !enabled {
			continue
		}

		token := ResolveValue(chCfg["botToken"], envMap)
		channels = append(channels, ChannelConfig{
			Type:    chType,
			Enabled: enabled,
			Token:   token,
		})
	}

	return channels, envMap, nil
}

// ResolveValue handles ${VAR} patterns and raw strings for config values.
// Exported so alerter.go and other packages can reuse it.
func ResolveValue(val any, envMap map[string]any) string {
	s, ok := val.(string)
	if !ok || s == "" {
		return ""
	}

	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		varName := s[2 : len(s)-1]
		if envMap != nil {
			if v, ok := envMap[varName].(string); ok && v != "" {
				return v
			}
		}
		if v := os.Getenv(varName); v != "" {
			return v
		}
		return ""
	}

	return s
}
