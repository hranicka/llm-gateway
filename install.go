package main

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

var version = "dev"

//go:embed config.example.yaml
var exampleConfig []byte

//go:embed systemd.service
var systemdService []byte

func runAsRoot() bool {
	return os.Geteuid() == 0
}

func requireRoot() {
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

func doInstall() {
	requireRoot()

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

	fmt.Println("Installing config to", configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		slog.Error("failed to create config directory", "error", err)
		os.Exit(1)
	}
	if err := os.WriteFile(configPath, exampleConfig, 0644); err != nil {
		slog.Error("failed to write config", "error", err)
		os.Exit(1)
	}

	fmt.Println("Installing systemd service")
	if err := os.WriteFile(serviceFile, systemdService, 0644); err != nil {
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

	fmt.Println("\nInstallation complete. Service 'llm-gateway' is running.")
	fmt.Println("  Logs:  journalctl -u llm-gateway -f")
	fmt.Println("  Remove: llm-gateway --uninstall")
}

func doUninstall() {
	requireRoot()

	serviceFile := "/etc/systemd/system/llm-gateway.service"

	fmt.Println("Stopping service")
	_ = runCommand("systemctl", "stop", "llm-gateway.service")

	fmt.Println("Disabling service")
	_ = runCommand("systemctl", "disable", "llm-gateway.service")

	fmt.Println("Removing service file")
	if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove service file", "error", err)
		os.Exit(1)
	}

	fmt.Println("Reloading systemd")
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		slog.Error("failed to reload systemd", "error", err)
		os.Exit(1)
	}

	fmt.Println("\nUninstall complete.")
	fmt.Println("  Binary and config remain at their locations. Remove manually if needed.")
}
