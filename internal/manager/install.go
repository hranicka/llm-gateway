package manager

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	llmgateway "llm-gateway"
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

// detectInstallUser returns the username and home directory of the user who
// invoked sudo. Falls back to the current user when SUDO_USER is not set.
func detectInstallUser() (username, homeDir string) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			return u.Username, u.HomeDir
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username, u.HomeDir
	}
	return "root", "/root"
}

// buildServiceFile replaces placeholders in the given service template with
// values appropriate for the target user so the daemon runs with the correct
// identity, home directory, and PATH (including ~/.local/bin).
func buildServiceFile(template []byte, username, homeDir string) []byte {
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:" + homeDir + "/.local/bin"
	content := string(template)
	content = strings.ReplaceAll(content, "%LLM_USER%", username)
	content = strings.ReplaceAll(content, "%LLM_HOME%", homeDir)
	content = strings.ReplaceAll(content, "%LLM_PATH%", path)
	return []byte(content)
}

// promptServiceType asks whether to install the generic (Vulkan/iGPU) or the
// CUDA/eGPU service template and returns the appropriate embedded bytes.
func promptServiceType() []byte {
	fmt.Println("\nSelect service type:")
	fmt.Println("  [1] Generic / Vulkan  — iGPU only, no NVIDIA GPU (default)")
	fmt.Println("  [2] CUDA / eGPU       — NVIDIA GPU (RTX 5060 Ti eGPU, etc.)")
	fmt.Print("\nSelect [1]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println()
		return llmgateway.SystemdService
	}
	input = strings.TrimSpace(input)
	if input == "2" {
		fmt.Println("Using CUDA/eGPU service (waits for /dev/nvidia0 at boot).")
		return llmgateway.SystemdServiceCUDA
	}
	fmt.Println("Using generic/Vulkan service.")
	return llmgateway.SystemdService
}

func promptConfig(configPath string) bool {
	configDirs := []string{"config"}
	if exe, err := os.Executable(); err == nil {
		configDirs = append(configDirs, filepath.Join(filepath.Dir(exe), "config"))
	}

	var names []string
	var baseDir string
	for _, dir := range configDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
				names = append(names, e.Name())
			}
		}
		if len(names) > 0 {
			baseDir = dir
			break
		}
	}

	if len(names) == 0 {
		fmt.Println("No configs found.")
		return false
	}

	sort.Strings(names)

	fmt.Println("\nAvailable configs:")
	fmt.Println("  [0] Do nothing (keep current config, if any)")
	for i, name := range names {
		fmt.Printf("  [%d] %s\n", i+1, name)
	}
	fmt.Print("\nSelect config [0]: ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println()
		return false
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	idx, err := strconv.Atoi(input)
	if err != nil || idx < 0 || idx > len(names) {
		fmt.Println("Invalid selection, skipping config installation.")
		return false
	}
	if idx == 0 {
		return false
	}

	selected := names[idx-1]
	data, err := os.ReadFile(filepath.Join(baseDir, selected))
	if err != nil {
		slog.Error("failed to read config", "name", selected, "error", err)
		os.Exit(1)
	}

	fmt.Printf("Installing config %s to %s\n", selected, configPath)
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		slog.Error("failed to write config", "error", err)
		os.Exit(1)
	}
	if fi, err := os.Stat(configPath); err == nil {
		fmt.Printf("Config installed (%d bytes)\n", fi.Size())
	}
	return true
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
	configInstalled := promptConfig(configPath)

	serviceTemplate := promptServiceType()
	username, homeDir := detectInstallUser()
	fmt.Printf("Configuring service to run as user %q (home: %s)\n", username, homeDir)
	fmt.Println("Installing systemd service to", serviceFile)
	if err := os.WriteFile(serviceFile, buildServiceFile(serviceTemplate, username, homeDir), 0644); err != nil {
		slog.Error("failed to write service file", "error", err)
		os.Exit(1)
	}

	fmt.Println("Restarting service")
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		slog.Error("failed to reload systemd", "error", err)
		os.Exit(1)
	}
	if err := runCommand("systemctl", "enable", "llm-gateway.service"); err != nil {
		slog.Error("failed to enable service", "error", err)
		os.Exit(1)
	}
	if err := runCommand("systemctl", "restart", "llm-gateway.service"); err != nil {
		slog.Error("failed to restart service", "error", err)
		os.Exit(1)
	}

	fmt.Println("\nInstallation complete.")
	fmt.Println()
	fmt.Println("Files installed:")
	fmt.Println("  Binary:  ", binPath)
	fmt.Println("  Config:  ", configPath)
	fmt.Println("  Service: ", serviceFile)
	fmt.Println()
	if !configInstalled {
		fmt.Println("Note: config was not installed. Review your existing config to ensure it contains the required models and their configuration.")
	} else {
		fmt.Println("The service has been restarted with the selected config.")
	}
	fmt.Println()
	fmt.Println("  Status:  systemctl status llm-gateway.service")
	fmt.Println("  Logs:    journalctl -u llm-gateway.service -f")
	fmt.Println("  Restart: sudo systemctl restart llm-gateway.service")
	fmt.Println("  Remove:  llm-gateway --uninstall")
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
