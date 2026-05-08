package manager

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"llm-gateway"
)

var Version = "dev"

func runAsRoot() bool {
	return os.Geteuid() == 0
}

func RequireRoot() {
	if !runAsRoot() {
		fmt.Fprintln(os.Stderr, "Error: this command requires root privileges. Use sudo.")
		os.Exit(1)
	}
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func DoInstall() {
	RequireRoot()

	binPath := "/usr/local/bin/llm-gateway"
	configDir := "/etc/llm-gateway"
	configPath := filepath.Join(configDir, "config.yaml")
	serviceFile := "/etc/systemd/system/llm-gateway.service"

	self, err := os.Executable()
	if err != nil {
		slog.Error("failed to get binary path", "error", err)
		os.Exit(1)
	}

	absSelf, err := filepath.Abs(self)
	if err != nil {
		slog.Error("failed to resolve binary path", "error", err)
		os.Exit(1)
	}

	absBin, err := filepath.Abs(binPath)
	if err != nil {
		slog.Error("failed to resolve install path", "error", err)
		os.Exit(1)
	}

	if absSelf != absBin {
		fmt.Println("Installing binary to", binPath)
		if err := runCommand("cp", "-f", absSelf, absBin); err != nil {
			slog.Error("failed to copy binary", "error", err)
			os.Exit(1)
		}
	} else {
		fmt.Println("Binary already at", binPath)
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		slog.Error("failed to create config directory", "error", err)
		os.Exit(1)
	}
	configSkipped := false
	if _, err := os.Stat(configPath); err == nil {
		fmt.Println("Config already exists at", configPath, "— skipping copy")
		configSkipped = true
	} else {
		fmt.Println("Installing config to", configPath)
		if err := os.WriteFile(configPath, llmgateway.ExampleYAML, 0644); err != nil {
			slog.Error("failed to write config", "error", err)
			os.Exit(1)
		}
	}

	fmt.Println("Installing systemd service to", serviceFile)
	if err := os.WriteFile(serviceFile, llmgateway.SystemdService, 0644); err != nil {
		slog.Error("failed to write service file", "error", err)
		os.Exit(1)
	}

	fmt.Println("Starting service")
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		slog.Error("failed to reload systemd", "error", err)
		os.Exit(1)
	}
	if err := runCommand("systemctl", "enable", "--now", "llm-gateway.service"); err != nil {
		slog.Error("failed to start service", "error", err)
		os.Exit(1)
	}

	fmt.Println("\nInstallation complete.")
	fmt.Println()
	fmt.Println("Files installed:")
	fmt.Println("  Binary:  ", binPath)
	fmt.Println("  Config:  ", configPath)
	fmt.Println("  Service: ", serviceFile)
	fmt.Println()
	if configSkipped {
		fmt.Println("Note: existing config was kept. Review it to ensure it contains the required models and their configuration.")
	} else {
		fmt.Println("Next step: edit the config file to add your models and their configuration:")
		fmt.Println("  $EDITOR", configPath)
	}
	fmt.Println()
	fmt.Println("  Logs:   journalctl -u llm-gateway -f")
	fmt.Println("  Remove: llm-gateway --uninstall")
}

func DoUninstall() {
	RequireRoot()

	binPath := "/usr/local/bin/llm-gateway"
	configDir := "/etc/llm-gateway"
	serviceFile := "/etc/systemd/system/llm-gateway.service"

	fmt.Println("Stopping service")
	_ = runCommand("systemctl", "stop", "llm-gateway.service")

	fmt.Println("Disabling service")
	_ = runCommand("systemctl", "disable", "llm-gateway.service")

	fmt.Println("Removing service file:", serviceFile)
	if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove service file", "error", err)
		os.Exit(1)
	}

	fmt.Println("Reloading systemd")
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		slog.Error("failed to reload systemd", "error", err)
		os.Exit(1)
	}

	fmt.Println("Removing binary:", binPath)
	if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove binary", "error", err)
		os.Exit(1)
	}

	fmt.Println("Removing config directory:", configDir)
	if err := os.RemoveAll(configDir); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove config directory", "error", err)
		os.Exit(1)
	}

	fmt.Println("\nUninstall complete.")
}
