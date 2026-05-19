package setup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Status prints the current sidecar status.
func Status() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	// Read env file
	envPath := filepath.Join(home, envDir, envFile)
	vars, err := readEnvFile(envPath)
	if err != nil {
		return fmt.Errorf("sidecar not installed (no config at %s)", envPath)
	}

	port := vars["HOSPITAL_AGENT_PORT"]
	if port == "" {
		port = "18793"
	}

	// Check systemd status
	unitStatus := "unknown"
	// Try new service name first, fall back to old
	for _, unit := range []string{serviceName, oldService} {
		out, err := exec.Command("systemctl", "--user", "is-active", unit).Output()
		if err == nil {
			unitStatus = strings.TrimSpace(string(out))
			break
		}
		s := strings.TrimSpace(string(out))
		if s == "inactive" || s == "failed" {
			unitStatus = s
			break
		}
	}

	fmt.Printf("hospital-sidecar\n")
	fmt.Printf("  Service:    %s\n", unitStatus)

	// If active, query healthz
	if unitStatus == "active" {
		url := fmt.Sprintf("http://127.0.0.1:%s/health", port)
		client := &http.Client{Timeout: 3 * time.Second}
		req, _ := http.NewRequest("GET", url, nil)
		if token := vars["HOSPITAL_AGENT_INBOUND_TOKEN"]; token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("  Health:     unreachable (%v)\n", err)
		} else {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var health struct {
					AgentName  string  `json:"agentName"`
					AgentState string  `json:"agentState"`
					Framework  string  `json:"framework"`
					Uptime     float64 `json:"uptime"`
					CrashCount int     `json:"crashCount"`
					Version    string  `json:"version"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&health); err == nil {
					fmt.Printf("  Agent:      %s (%s)\n", health.AgentName, health.Framework)
					fmt.Printf("  Gateway:    %s\n", health.AgentState)
					fmt.Printf("  Uptime:     %s\n", formatDuration(health.Uptime))
					fmt.Printf("  Crashes:    %d\n", health.CrashCount)
					fmt.Printf("  Version:    %s\n", health.Version)
				}
			} else {
				fmt.Printf("  Health:     HTTP %d\n", resp.StatusCode)
			}
		}
	}

	// Show config
	fmt.Printf("  Hospital:   %s\n", vars["HOSPITAL_AGENT_HOSPITAL_URL"])
	fmt.Printf("  Framework:  %s\n", vars["HOSPITAL_AGENT_FRAMEWORK"])
	fmt.Printf("  Port:       %s\n", port)
	if llm := vars["HOSPITAL_AGENT_LLM_HEALTH_URL"]; llm != "" {
		fmt.Printf("  LLM check:  %s\n", llm)
	}

	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			vars[parts[0]] = parts[1]
		}
	}
	return vars, nil
}

func formatDuration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
