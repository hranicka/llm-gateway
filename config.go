package main

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
	Host   string               `yaml:"host"`
	Debug  bool                 `yaml:"debug"`
	Models map[string]ModelConf `yaml:"models"`
}

type ModelConf struct {
	Command      string `yaml:"command"`
	Host         string `yaml:"host"`
	ReadyTimeout string `yaml:"ready_timeout"`
}

var config *Config

// sortedModelNames is the config.Models keys, sorted once after loadConfig.
var sortedModelNames []string

// loadConfig reads the YAML file and validates models.
func loadConfig(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	config = &Config{}
	if err := yaml.Unmarshal(data, config); err != nil {
		return fmt.Errorf("failed to parse yaml: %w", err)
	}

	for name, m := range config.Models {
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

	sortedModelNames = slices.Sorted(maps.Keys(config.Models))
	return nil
}

// modelReadyTimeout returns the ready timeout for a model.
func modelReadyTimeout(modelName string) time.Duration {
	m, ok := config.Models[modelName]
	if !ok {
		return 0
	}
	timeout, _ := time.ParseDuration(m.ReadyTimeout)
	return timeout
}

// buildCommand returns the raw command string and the backend URL from the host field.
func buildCommand(modelName string) (string, string, error) {
	m, ok := config.Models[modelName]
	if !ok {
		return "", "", fmt.Errorf("model %q not found in config", modelName)
	}

	// Normalize multi-line YAML block scalar: newlines act as command
	// separators in sh -c, so collapse all whitespace to single spaces.
	cmdStr := strings.Join(strings.Fields(m.Command), " ")
	backendURL := fmt.Sprintf("http://%s", m.Host)
	return cmdStr, backendURL, nil
}
