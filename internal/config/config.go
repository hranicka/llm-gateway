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
	Host       string               `yaml:"host"`
	Debug      bool                 `yaml:"debug"`
	AutoUnload string               `yaml:"auto_unload"`
	Models     map[string]ModelConf `yaml:"models"`
}

type ModelConf struct {
	Command      string `yaml:"command"`
	Host         string `yaml:"host"`
	ReadyTimeout string `yaml:"ready_timeout"`
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

// BuildCommand returns the raw command string and the backend URL from the host field.
func BuildCommand(modelName string) (string, string, error) {
	m, ok := ConfigApp.Models[modelName]
	if !ok {
		return "", "", fmt.Errorf("model %q not found in config", modelName)
	}

	// Normalize multi-line YAML block scalar: newlines act as command
	// separators in sh -c, so collapse all whitespace to single spaces.
	cmdStr := strings.Join(strings.Fields(m.Command), " ")
	backendURL := fmt.Sprintf("http://%s", m.Host)
	return cmdStr, backendURL, nil
}
