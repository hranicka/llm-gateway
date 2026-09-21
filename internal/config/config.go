package config

import (
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Host  string `yaml:"host"`
	Debug bool   `yaml:"debug"`
	// AuthToken is required; an empty string disables auth explicitly.
	// Pointer so a missing field can be distinguished from an empty one.
	AuthToken    *string              `yaml:"auth_token"`
	AllowedHosts []string             `yaml:"allowed_hosts"`
	MaxBodySize  string               `yaml:"max_body_size"`
	AutoUnload   string               `yaml:"auto_unload"`
	DrainTimeout string               `yaml:"drain_timeout"`
	Models       map[string]ModelConf `yaml:"models"`
}

// Model kinds. KindAPI models serve OpenAI-style endpoints and are proxied
// via /v1/chat/completions, /v1/completions and /v1/images/generations.
// KindWeb models serve a browser UI (e.g. sd-server, ComfyUI) that the
// gateway reverse-proxies in full — including websockets — once selected
// via /app/<name>.
const (
	KindAPI = "api"
	KindWeb = "web"
)

type ModelConf struct {
	Kind           string `yaml:"kind"` // optional; "api" (default) or "web"
	Command        string `yaml:"command"`
	Host           string `yaml:"host"`
	ReadyTimeout   string `yaml:"ready_timeout"`
	HealthEndpoint string `yaml:"health_endpoint"` // optional; defaults to "/health" (used by llama-server); web apps typically "/"
}

// Default config search paths.
const (
	DefaultConfigPath = "config.yaml"
	SystemConfigPath  = "/etc/llm-gateway/config.yaml"
)

var ConfigApp *Config

// sortedModelNames is the config.Models keys, sorted once after loadConfig.
var SortedModelNames []string

// normalizedAllowedHosts is allowed_hosts lowercased and stripped of ports,
// set once after Load. Host-header matching compares against these.
var normalizedAllowedHosts []string

// maxBodyBytes is the parsed max_body_size, set once after Load.
var maxBodyBytes int64

// defaultMaxBodyBytes backs MaxBodyBytes for Configs built without Load
// (tests), so direct struct construction stays functional.
const defaultMaxBodyBytes = 64 << 20

// AuthEnabled reports whether auth_token is set to a non-empty value.
func AuthEnabled() bool {
	return AuthToken() != ""
}

// AuthToken returns the configured bearer token ("" when auth is disabled).
func AuthToken() string {
	if ConfigApp == nil || ConfigApp.AuthToken == nil {
		return ""
	}
	return *ConfigApp.AuthToken
}

// AllowedHosts returns the normalized Host allowlist (lowercased, ports
// stripped). Empty means the Host check is disabled.
func AllowedHosts() []string {
	return normalizedAllowedHosts
}

// MaxBodyBytes returns the configured request-body limit.
func MaxBodyBytes() int64 {
	if maxBodyBytes > 0 {
		return maxBodyBytes
	}
	return defaultMaxBodyBytes
}

// NormalizeHost strips the port from a host or Host-header value
// ("gem12.lan:1234" → "gem12.lan", "[::1]:1234" → "::1").
func NormalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// parseByteSize parses a size like "64MB", "512KB", "1GB" or a bare byte
// count. Units are binary (KB = 1024).
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || num <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	var mult float64
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "", "B":
		mult = 1
	case "KB":
		mult = 1 << 10
	case "MB":
		mult = 1 << 20
	case "GB":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("unknown unit in %q (use B, KB, MB or GB)", s)
	}
	return int64(num * mult), nil
}

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

	if ConfigApp.AuthToken == nil {
		return fmt.Errorf("auth_token is required (a long random string; \"\" explicitly disables auth)")
	}
	if ConfigApp.AllowedHosts == nil {
		return fmt.Errorf("allowed_hosts is required (Host headers to answer as, or [] to disable the check)")
	}
	normalizedAllowedHosts = make([]string, 0, len(ConfigApp.AllowedHosts))
	for _, h := range ConfigApp.AllowedHosts {
		if host := strings.ToLower(NormalizeHost(h)); host != "" {
			normalizedAllowedHosts = append(normalizedAllowedHosts, host)
		}
	}
	if ConfigApp.MaxBodySize == "" {
		return fmt.Errorf("max_body_size is required (e.g. 64MB)")
	}
	size, err := parseByteSize(ConfigApp.MaxBodySize)
	if err != nil {
		return fmt.Errorf("max_body_size: %w", err)
	}
	maxBodyBytes = size

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
		switch m.Kind {
		case "", KindAPI, KindWeb:
		default:
			return fmt.Errorf("model %q has unknown kind %q (expected %q or %q)", name, m.Kind, KindAPI, KindWeb)
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

// ModelKind returns the resolved kind ("api" or "web") of the given model.
// Unknown and unconfigured models resolve to "api".
func ModelKind(modelName string) string {
	m, ok := ConfigApp.Models[modelName]
	if !ok || m.Kind == "" {
		return KindAPI
	}
	return m.Kind
}
