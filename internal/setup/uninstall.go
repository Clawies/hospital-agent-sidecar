package setup

import (
	"fmt"
	"os"
	"path/filepath"
)

// Uninstall stops the sidecar service and removes all installed files.
func Uninstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	fmt.Println("Uninstalling hospital-sidecar...")

	// Stop and disable both old and new service names
	for _, unit := range []string{serviceName, oldService} {
		_ = runCmd("systemctl", "--user", "stop", unit)
		_ = runCmd("systemctl", "--user", "disable", unit)
	}

	// Remove files
	files := []string{
		filepath.Join(home, ".config", "systemd", "user", serviceName),
		filepath.Join(home, ".config", "systemd", "user", oldService),
		filepath.Join(home, envDir, envFile),
		filepath.Join(home, ".local", "bin", "hospital-sidecar"),
		filepath.Join(home, ".local", "bin", "hospital-agent-sidecar"), // old binary name
	}
	for _, f := range files {
		if err := os.Remove(f); err == nil {
			fmt.Printf("  Removed: %s\n", f)
		}
	}

	// Remove env dir if empty
	envDirPath := filepath.Join(home, envDir)
	_ = os.Remove(envDirPath) // only succeeds if empty

	// Reload systemd
	_ = runCmd("systemctl", "--user", "daemon-reload")

	fmt.Println("Uninstall complete.")
	return nil
}
