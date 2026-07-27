package config

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Host         string               `yaml:"host"`
	Debug        bool                 `yaml:"debug"`
	AutoUnload   string               `yaml:"auto_unload"`
	DrainTimeout string               `yaml:"drain_timeout"`
	Models       map[string]ModelConf `yaml:"models"`
}

type ModelConf struct {
	Command        string `yaml:"command"`
	Host           string `yaml:"host"`
	ReadyTimeout   string `yaml:"ready_timeout"`
	HealthEndpoint string `yaml:"health_endpoint"` // optional; defaults to "/health" (used by llama-server)
	BackendModel   string `yaml:"backend_model"`   // optional; client "model" field rewritten to this value for vLLM
}

// Default config search paths.
const (
	DefaultConfigPath = "config.yaml"
	SystemConfigPath  = "/etc/llm-gateway/config.yaml"
)

var ConfigApp *Config

// sortedModelNames is the config.Models keys, sorted once after loadConfig.
var SortedModelNames []string

// FindConfigPath returns the first existing config path from the search order.
func FindConfigPath() string {
	if _, err := os.Stat(DefaultConfigPath); err == nil {
		return DefaultConfigPath
	}
	if _, err := os.Stat(SystemConfigPath); err == nil {
		return SystemConfigPath
	}
	return ""
}

// Load reads the YAML file and validates models.
func Load(filename string) error {
	if filename == "" {
		if filename = FindConfigPath(); filename == "" {
			return fmt.Errorf("config not found — expected %s in current directory or %s", DefaultConfigPath, SystemConfigPath)
		}
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	ConfigApp = &Config{}
	if err := yaml.Unmarshal(data, ConfigApp); err != nil {
		return fmt.Errorf("failed to parse yaml: %w", err)
	}

	if ConfigApp.AutoUnload == "" {
		return fmt.Errorf("auto_unload is required")
	}
	if _, err := time.ParseDuration(ConfigApp.AutoUnload); err != nil {
		return fmt.Errorf("auto_unload: %w", err)
	}
	if ConfigApp.DrainTimeout == "" {
		return fmt.Errorf("drain_timeout is required")
	}
	drainTimeout, err := time.ParseDuration(ConfigApp.DrainTimeout)
	if err != nil {
		return fmt.Errorf("drain_timeout: %w", err)
	}
	if drainTimeout <= 0 {
		return fmt.Errorf("drain_timeout must be greater than zero")
	}

	for name, m := range ConfigApp.Models {
		if m.Command == "" {
			return fmt.Errorf("model %q requires command", name)
		}
		if m.Host == "" {
			return fmt.Errorf("model %q requires host", name)
		}
		if m.ReadyTimeout == "" {
			return fmt.Errorf("model %q requires ready_timeout", name)
		}
		if _, err := time.ParseDuration(m.ReadyTimeout); err != nil {
			return fmt.Errorf("model %q ready_timeout: %w", name, err)
		}
	}

	if len(ConfigApp.Models) == 0 {
		return fmt.Errorf("at least one model must be configured")
	}

	SortedModelNames = slices.Sorted(maps.Keys(ConfigApp.Models))

	return nil
}

// ModelReadyTimeout returns the ready timeout for a model.
func ModelReadyTimeout(modelName string) time.Duration {
	m, ok := ConfigApp.Models[modelName]
	if !ok {
		return 0
	}
	timeout, _ := time.ParseDuration(m.ReadyTimeout)
	return timeout
}

// AutoUnloadDuration returns the configured auto-unload idle duration.
func AutoUnloadDuration() time.Duration {
	d, _ := time.ParseDuration(ConfigApp.AutoUnload)
	return d
}

// DrainTimeout returns the configured time to wait for active requests to
// finish before terminating the current model during a switch.
func DrainTimeout() time.Duration {
	d, _ := time.ParseDuration(ConfigApp.DrainTimeout)
	return d
}

// BuildCommand returns the raw command string and the backend URL from the host field.
func BuildCommand(modelName string) (string, string, error) {
	m, ok := ConfigApp.Models[modelName]
	if !ok {
		return "", "", fmt.Errorf("model %q not found in config", modelName)
	}

	// Normalize the multi-line YAML block scalar into a single shell command
	// line: collapse line breaks to spaces so the whole string runs as one
	// command under sh -c. Spaces inside quoted arguments are preserved.
	cmdStr := strings.ReplaceAll(strings.TrimSpace(m.Command), "\n", " ")
	backendURL := fmt.Sprintf("http://%s", m.Host)
	return cmdStr, backendURL, nil
}

// HealthEndpoint returns the health-check path for the given model. Returns
// the configured value if set (e.g. "/v1/models" for vllm serve), otherwise
// the default "/health" used by llama-server.
func HealthEndpoint(modelName string) string {
	m, ok := ConfigApp.Models[modelName]
	if !ok || m.HealthEndpoint == "" {
		return "/health"
	}
	return m.HealthEndpoint
}

// BackendModel returns the model name to send in the JSON body to the backend.
// Returns empty string when not configured (no rewrite needed).
func BackendModel(modelName string) string {
	m, ok := ConfigApp.Models[modelName]
	if !ok || m.BackendModel == "" {
		return ""
	}
	return m.BackendModel
}
