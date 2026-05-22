package setup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/clawies/hospital-agent-sidecar/internal/config"
	"github.com/clawies/hospital-agent-sidecar/internal/identity"
)

const (
	serviceName = "hospital-sidecar.service"
	oldService  = "hospital-agent-sidecar.service"
	envDir      = ".config/hospital-agent-sidecar"
	envFile     = "agent.env"
)

// setupConfig holds parsed CLI flags for the setup command.
type setupConfig struct {
	apiKey       string // legacy auth
	hostToken    string // Ed25519 enrollment token (new auth)
	hospitalURL  string
	inboundToken string
	framework    string
	stateDir     string
	systemdUnit  string
	gatewayPort  string
	port         string
	name         string
}

// Run executes the setup subcommand.
func Run(args []string, version string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("setup requires Linux with systemd (detected %s)", runtime.GOOS)
	}

	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	// Auto-detect framework if not specified
	if cfg.framework == "" {
		cfg.framework = detectFramework(home)
		if cfg.framework == "" {
			return fmt.Errorf("cannot auto-detect framework (no ~/.openclaw or ~/.hermes found)\nUse --framework openclaw or --framework hermes")
		}
		fmt.Printf("  Detected framework: %s\n", cfg.framework)
	}

	// Auto-detect state dir
	if cfg.stateDir == "" {
		switch cfg.framework {
		case "openclaw":
			cfg.stateDir = filepath.Join(home, ".openclaw")
		case "hermes":
			cfg.stateDir = filepath.Join(home, ".hermes")
		}
		fmt.Printf("  State dir: %s\n", cfg.stateDir)
	}

	// Auto-detect systemd unit
	if cfg.systemdUnit == "" {
		switch cfg.framework {
		case "openclaw":
			cfg.systemdUnit = "openclaw-gateway.service"
		case "hermes":
			cfg.systemdUnit = "hermes-gateway.service"
		}
		fmt.Printf("  Systemd unit: %s\n", cfg.systemdUnit)
	}

	// Generate inbound token if not provided
	if cfg.inboundToken == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generate token: %w", err)
		}
		cfg.inboundToken = hex.EncodeToString(b)
		fmt.Printf("  Generated inbound token: %s\n", cfg.inboundToken[:8]+"...")
	}

	// Agent name defaults to hostname
	if cfg.name == "" {
		cfg.name, _ = os.Hostname()
		if cfg.name == "" {
			cfg.name = "unknown"
		}
	}

	// Auto-detect LLM provider
	llmURL, llmAuth := "", ""
	if cfg.framework == "openclaw" {
		det, err := config.DetectLLM(cfg.stateDir, cfg.framework)
		if err != nil {
			fmt.Printf("  LLM auto-detect: %v (will skip LLM health check)\n", err)
		} else {
			llmURL = det.HealthURL
			llmAuth = det.AuthHeader
			fmt.Printf("  Detected LLM: %s -> %s\n", det.Provider, det.HealthURL)
		}
	}

	// --- Ed25519 registration (if --host-token provided) ---
	var authMethod string
	var privateKeyPath string
	var fingerprint string

	if cfg.hostToken != "" {
		authMethod = "ed25519"
		fmt.Println("\n  Generating Ed25519 keypair...")

		pub, priv, err := identity.GenerateKeypair()
		if err != nil {
			return fmt.Errorf("generate keypair: %w", err)
		}

		privateKeyPath = filepath.Join(home, envDir, "agent.key")
		if err := identity.SavePrivateKey(privateKeyPath, priv); err != nil {
			return fmt.Errorf("save private key: %w", err)
		}
		fmt.Printf("  Private key: %s\n", privateKeyPath)

		fingerprint = identity.PublicKeyFingerprint(pub)
		fmt.Printf("  Fingerprint: %s\n", fingerprint[:16]+"...")

		// Register with hospital
		fmt.Printf("  Registering with %s...\n", cfg.hospitalURL)
		regResp, err := RegisterAgent(cfg.hospitalURL, RegisterRequest{
			HostToken:   cfg.hostToken,
			PublicKey:   identity.PublicKeyBase64(pub),
			Name:        cfg.name,
			Framework:   cfg.framework,
			CallbackURL: fmt.Sprintf("http://localhost:%s", cfg.port),
		})
		if err != nil {
			return fmt.Errorf("agent registration failed: %w", err)
		}

		cfg.inboundToken = regResp.InboundToken
		fmt.Printf("  Registered: agentId=%s\n", regResp.AgentID)
		fmt.Printf("  Capabilities: %s\n", strings.Join(regResp.Capabilities, ", "))
	} else {
		authMethod = "api_key"
	}

	// --- Create directories ---
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".config", "systemd", "user"),
		filepath.Join(home, envDir),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", d, err)
		}
	}

	// Stop existing services before replacing the binary. Re-running setup is the
	// upgrade path for npx installs, and Linux can reject truncating a running
	// executable with ETXTBSY ("text file busy").
	_ = runCmd("systemctl", "--user", "stop", serviceName)
	_ = runCmd("systemctl", "--user", "stop", oldService)
	_ = runCmd("systemctl", "--user", "disable", oldService)

	// --- Install binary ---
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	binDst := filepath.Join(home, ".local", "bin", "hospital-sidecar")
	if err := installExecutable(exe, binDst, 0755); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}
	fmt.Printf("  Installed binary: %s\n", binDst)

	// --- Write systemd unit ---
	unitDst := filepath.Join(home, ".config", "systemd", "user", serviceName)
	if err := os.WriteFile(unitDst, systemdUnit, 0644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	fmt.Printf("  Installed systemd unit: %s\n", unitDst)

	// --- Write env file ---
	envPath := filepath.Join(home, envDir, envFile)
	envContent := buildEnvFile(cfg, llmURL, llmAuth, authMethod, privateKeyPath, fingerprint)
	if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}
	fmt.Printf("  Wrote config: %s\n", envPath)

	// --- Enable linger ---
	_ = runCmd("loginctl", "enable-linger")

	// --- Reload and start ---
	if err := runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if err := runCmd("systemctl", "--user", "enable", serviceName); err != nil {
		return fmt.Errorf("enable service: %w", err)
	}
	if err := runCmd("systemctl", "--user", "restart", serviceName); err != nil {
		return fmt.Errorf("restart service: %w", err)
	}

	// --- Verify ---
	time.Sleep(2 * time.Second)
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/healthz", cfg.port)
	resp, err := http.Get(healthURL)
	if err != nil {
		fmt.Printf("\n  WARNING: healthz check failed: %v\n", err)
		fmt.Println("  Service may still be starting. Check: systemctl --user status hospital-sidecar")
	} else {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			fmt.Printf("  Health check: OK (%s)\n", healthURL)
		} else {
			fmt.Printf("  WARNING: healthz returned %d\n", resp.StatusCode)
		}
	}

	fmt.Println()
	fmt.Println("Setup complete.")
	fmt.Printf("  Agent: %s (%s)\n", cfg.name, cfg.framework)
	fmt.Printf("  Hospital: %s\n", cfg.hospitalURL)
	fmt.Printf("  Auth: %s\n", authMethod)
	fmt.Printf("  Port: %s\n", cfg.port)
	fmt.Println()
	if authMethod == "ed25519" {
		fmt.Println("Agent registered with Ed25519 keypair authentication.")
	} else {
		fmt.Println("The sidecar will auto-register with Agent Hospital on the first heartbeat.")
	}
	fmt.Println("Check logs: journalctl --user -u hospital-sidecar -f")

	return nil
}

func parseFlags(args []string) (*setupConfig, error) {
	cfg := &setupConfig{
		gatewayPort: "18789",
		port:        "18793",
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api-key":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--api-key requires a value")
			}
			i++
			cfg.apiKey = args[i]
		case "--host-token":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--host-token requires a value")
			}
			i++
			cfg.hostToken = args[i]
		case "--hospital-url":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--hospital-url requires a value")
			}
			i++
			cfg.hospitalURL = args[i]
		case "--inbound-token":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--inbound-token requires a value")
			}
			i++
			cfg.inboundToken = args[i]
		case "--framework":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--framework requires a value")
			}
			i++
			cfg.framework = args[i]
		case "--state-dir":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--state-dir requires a value")
			}
			i++
			cfg.stateDir = args[i]
		case "--systemd-unit":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--systemd-unit requires a value")
			}
			i++
			cfg.systemdUnit = args[i]
		case "--gateway-port":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--gateway-port requires a value")
			}
			i++
			cfg.gatewayPort = args[i]
		case "--port":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--port requires a value")
			}
			i++
			cfg.port = args[i]
		case "--name":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--name requires a value")
			}
			i++
			cfg.name = args[i]
		case "--help", "-h":
			fmt.Println("Usage: hospital-sidecar setup [--host-token TOKEN | --api-key KEY] --hospital-url URL [options]")
			fmt.Println()
			fmt.Println("Authentication (pick one):")
			fmt.Println("  --host-token TOKEN   Register with Ed25519 keypair (recommended)")
			fmt.Println("  --api-key KEY        Use legacy API key auth")
			fmt.Println()
			fmt.Println("Options:")
			fmt.Println("  --hospital-url URL   Hospital server URL (required)")
			fmt.Println("  --name NAME          Agent name (default: hostname)")
			fmt.Println("  --framework FW       openclaw or hermes (auto-detected)")
			fmt.Println("  --state-dir DIR      Agent home dir (auto-detected)")
			fmt.Println("  --systemd-unit UNIT  Unit to monitor (auto-detected)")
			fmt.Println("  --gateway-port PORT  Gateway port (default: 18789)")
			fmt.Println("  --port PORT          Sidecar listen port (default: 18793)")
			fmt.Println("  --inbound-token TOK  Inbound bearer token (auto-generated)")
			os.Exit(0)
		default:
			return nil, fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	if cfg.apiKey == "" && cfg.hostToken == "" {
		return nil, fmt.Errorf("either --host-token or --api-key is required")
	}
	if cfg.apiKey != "" && cfg.hostToken != "" {
		return nil, fmt.Errorf("--host-token and --api-key are mutually exclusive")
	}
	if cfg.hospitalURL == "" {
		return nil, fmt.Errorf("--hospital-url is required")
	}
	if cfg.framework != "" && cfg.framework != "openclaw" && cfg.framework != "hermes" {
		return nil, fmt.Errorf("--framework must be 'openclaw' or 'hermes', got %q", cfg.framework)
	}

	return cfg, nil
}

func detectFramework(home string) string {
	if _, err := os.Stat(filepath.Join(home, ".openclaw")); err == nil {
		return "openclaw"
	}
	if _, err := os.Stat(filepath.Join(home, ".hermes")); err == nil {
		return "hermes"
	}
	return ""
}

func buildEnvFile(cfg *setupConfig, llmURL, llmAuth, authMethod, privateKeyPath, fingerprint string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_PORT=%s\n", cfg.port))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_INBOUND_TOKEN=%s\n", cfg.inboundToken))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_AUTH_METHOD=%s\n", authMethod))
	if authMethod == "ed25519" {
		b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_PRIVATE_KEY_PATH=%s\n", privateKeyPath))
		b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_FINGERPRINT=%s\n", fingerprint))
	} else {
		b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_API_KEY=%s\n", cfg.apiKey))
	}
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_HOSPITAL_URL=%s\n", cfg.hospitalURL))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_STATE_DIR=%s\n", cfg.stateDir))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_SYSTEMD_UNIT=%s\n", cfg.systemdUnit))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_FRAMEWORK=%s\n", cfg.framework))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_GATEWAY_PORT=%s\n", cfg.gatewayPort))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_HEARTBEAT_INTERVAL=60\n"))
	b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_NAME=%s\n", cfg.name))
	if cfg.framework == "openclaw" {
		b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_GATEWAY_URL=http://localhost:%s\n", cfg.gatewayPort))
	}
	if llmURL != "" {
		b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_LLM_HEALTH_URL=%s\n", llmURL))
	}
	if llmAuth != "" {
		b.WriteString(fmt.Sprintf("HOSPITAL_AGENT_LLM_HEALTH_AUTH=%s\n", llmAuth))
	}
	return b.String()
}

func installExecutable(src, dst string, mode os.FileMode) error {
	// If src == dst, skip
	srcAbs, _ := filepath.Abs(src)
	dstAbs, _ := filepath.Abs(dst)
	if srcAbs == dstAbs {
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".hospital-sidecar-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
