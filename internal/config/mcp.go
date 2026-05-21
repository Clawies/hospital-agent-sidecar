package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// MCPServerConfig represents an MCP server configured in the agent's config.
type MCPServerConfig struct {
	Name      string
	Transport string // "stdio", "sse", "streamable-http"
	Command   string // for stdio: the command binary
	Args      []string
	URL       string // for SSE/HTTP: the endpoint URL
	Enabled   bool
}

// DiscoverMCPServers reads the agent config and extracts configured MCP servers.
// OpenClaw uses mcp.servers in openclaw.json; Hermes uses mcp_servers in config.yaml.
func DiscoverMCPServers(stateDir, framework string) ([]MCPServerConfig, error) {
	if framework == "hermes" {
		return discoverHermesMCP(stateDir)
	}
	return discoverOpenClawMCP(stateDir)
}

func discoverOpenClawMCP(stateDir string) ([]MCPServerConfig, error) {
	configPath := filepath.Join(stateDir, "openclaw.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	mcpSection, ok := getNestedMap(root, "mcp", "servers")
	if !ok {
		return nil, nil // no MCP servers configured
	}

	envMap, _ := root["env"].(map[string]any)
	var servers []MCPServerConfig

	for name, v := range mcpSection {
		cfg, ok := v.(map[string]any)
		if !ok {
			continue
		}

		server := MCPServerConfig{
			Name:    name,
			Enabled: true,
		}

		// Check enabled flag
		if e, ok := cfg["enabled"].(bool); ok && !e {
			continue
		}

		// Detect transport type
		if cmd, ok := cfg["command"].(string); ok && cmd != "" {
			server.Transport = "stdio"
			server.Command = ResolveValue(cfg["command"], envMap)
			if args, ok := cfg["args"].([]any); ok {
				for _, a := range args {
					if s, ok := a.(string); ok {
						server.Args = append(server.Args, s)
					}
				}
			}
		} else if url, ok := cfg["url"].(string); ok && url != "" {
			server.URL = ResolveValue(cfg["url"], envMap)
			// Determine SSE vs streamable-http
			if t, ok := cfg["transport"].(string); ok {
				server.Transport = t
			} else {
				server.Transport = "sse" // default for URL-based
			}
		} else {
			continue // no command or URL, skip
		}

		servers = append(servers, server)
	}

	return servers, nil
}

func discoverHermesMCP(stateDir string) ([]MCPServerConfig, error) {
	// Hermes uses config.yaml with mcp_servers section.
	// For now, try to parse it as JSON (some Hermes configs are JSON).
	// Full YAML support would require a YAML parser dependency.
	configPath := filepath.Join(stateDir, "config.yaml")

	// Try JSON first (hermes.json)
	jsonPath := filepath.Join(stateDir, "hermes.json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		// Try config.yaml -- but we can't parse YAML without a dependency.
		// Check if it exists at least.
		if _, statErr := os.Stat(configPath); statErr != nil {
			return nil, nil // no config found
		}
		return nil, nil // YAML parsing not supported yet
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}

	mcpServers, ok := root["mcp_servers"].(map[string]any)
	if !ok {
		return nil, nil
	}

	var servers []MCPServerConfig
	for name, v := range mcpServers {
		cfg, ok := v.(map[string]any)
		if !ok {
			continue
		}

		server := MCPServerConfig{
			Name:    name,
			Enabled: true,
		}

		if cmd, ok := cfg["command"].(string); ok && cmd != "" {
			server.Transport = "stdio"
			server.Command = cmd
			if args, ok := cfg["args"].([]any); ok {
				for _, a := range args {
					if s, ok := a.(string); ok {
						server.Args = append(server.Args, s)
					}
				}
			}
		} else if url, ok := cfg["url"].(string); ok && url != "" {
			server.Transport = "sse"
			server.URL = url
		} else {
			continue
		}

		servers = append(servers, server)
	}

	return servers, nil
}
