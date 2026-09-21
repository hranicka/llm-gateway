package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validConfig = `host: 0.0.0.0:1234
debug: true
auth_token: "secret-token"
allowed_hosts:
  - localhost:1234
  - gem12.lan
max_body_size: 64MB
auto_unload: 2h
drain_timeout: 30s

models:
  beta-model:
    command: |
      llama-server --host 127.0.0.1 --port 1235
    host: 127.0.0.1:1235
    ready_timeout: 1m
  alpha-model:
    command: |
      llama-server --host 127.0.0.1 --port 1236
    host: 127.0.0.1:1236
    ready_timeout: 2m
`

func TestLoad_Valid(t *testing.T) {
	if err := Load(writeConfig(t, validConfig)); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if ConfigApp.Host != "0.0.0.0:1234" {
		t.Errorf("Host = %q, want 0.0.0.0:1234", ConfigApp.Host)
	}
	if !ConfigApp.Debug {
		t.Error("Debug = false, want true")
	}
	if len(ConfigApp.Models) != 2 {
		t.Errorf("len(Models) = %d, want 2", len(ConfigApp.Models))
	}
	want := []string{"alpha-model", "beta-model"}
	if len(SortedModelNames) != len(want) {
		t.Fatalf("SortedModelNames = %v, want %v", SortedModelNames, want)
	}
	for i := range want {
		if SortedModelNames[i] != want[i] {
			t.Errorf("SortedModelNames[%d] = %q, want %q", i, SortedModelNames[i], want[i])
		}
	}
}

func TestLoad_SecurityFields(t *testing.T) {
	if err := Load(writeConfig(t, validConfig)); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !AuthEnabled() || AuthToken() != "secret-token" {
		t.Errorf("auth = %q, enabled = %v; want secret-token/enabled", AuthToken(), AuthEnabled())
	}
	// Hosts are lowercased and stripped of ports for matching.
	wantHosts := []string{"localhost", "gem12.lan"}
	gotHosts := AllowedHosts()
	if len(gotHosts) != len(wantHosts) {
		t.Fatalf("AllowedHosts = %v, want %v", gotHosts, wantHosts)
	}
	for i := range wantHosts {
		if gotHosts[i] != wantHosts[i] {
			t.Errorf("AllowedHosts[%d] = %q, want %q", i, gotHosts[i], wantHosts[i])
		}
	}
	if MaxBodyBytes() != 64<<20 {
		t.Errorf("MaxBodyBytes = %d, want %d", MaxBodyBytes(), int64(64<<20))
	}

	// Empty auth_token explicitly disables auth.
	emptyAuth := strings.Replace(validConfig, `auth_token: "secret-token"`, `auth_token: ""`, 1)
	if err := Load(writeConfig(t, emptyAuth)); err != nil {
		t.Fatalf("Load(empty auth_token) returned error: %v", err)
	}
	if AuthEnabled() || AuthToken() != "" {
		t.Errorf("empty auth_token: enabled = %v, token = %q; want disabled", AuthEnabled(), AuthToken())
	}
}

func TestLoad_Errors(t *testing.T) {
	model := "models:\n  m:\n"
	tests := []struct {
		name    string
		content string
	}{
		{"missing auto_unload", "drain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"invalid auto_unload", "auto_unload: banana\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"missing drain_timeout", "auto_unload: 2h\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"invalid drain_timeout", "auto_unload: 2h\ndrain_timeout: banana\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"zero drain_timeout", "auto_unload: 2h\ndrain_timeout: 0s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"missing auth_token", "auto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"missing allowed_hosts", "auth_token: \"\"\nauto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"missing max_body_size", "auth_token: \"\"\nallowed_hosts: []\nauto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"invalid max_body_size", "auth_token: \"\"\nallowed_hosts: []\nmax_body_size: banana\nauto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"zero max_body_size", "auth_token: \"\"\nallowed_hosts: []\nmax_body_size: 0\nauto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"model missing command", "auto_unload: 2h\ndrain_timeout: 30s\n" + model + "    host: h\n    ready_timeout: 1m\n"},
		{"model missing host", "auto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    ready_timeout: 1m\n"},
		{"model missing ready_timeout", "auto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n"},
		{"model invalid ready_timeout", "auto_unload: 2h\ndrain_timeout: 30s\n" + model + "    command: c\n    host: h\n    ready_timeout: banana\n"},
		{"model invalid kind", "auto_unload: 2h\ndrain_timeout: 30s\n" + model + "    kind: banana\n    command: c\n    host: h\n    ready_timeout: 1m\n"},
		{"no models", "auto_unload: 2h\ndrain_timeout: 30s\nmodels: {}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Load(writeConfig(t, tt.content)); err == nil {
				t.Error("Load returned nil error, want error")
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"64MB", 64 << 20, true},
		{"512KB", 512 << 10, true},
		{"1GB", 1 << 30, true},
		{"65536", 65536, true},
		{"64 mb", 64 << 20, true},
		{"", 0, false},
		{"banana", 0, false},
		{"64TB", 0, false},
		{"0MB", 0, false},
		{"-5MB", 0, false},
	}
	for _, tt := range tests {
		got, err := parseByteSize(tt.in)
		if tt.ok && (err != nil || got != tt.want) {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d, nil", tt.in, got, err, tt.want)
		}
		if !tt.ok && err == nil {
			t.Errorf("parseByteSize(%q) = %d, nil error; want error", tt.in, got)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"gem12.lan:1234": "gem12.lan",
		"gem12.lan":      "gem12.lan",
		"[::1]:1234":     "::1",
		"localhost":      "localhost",
	}
	for in, want := range tests {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Error("Load returned nil error for missing file, want error")
	}
}

func TestBuildCommand(t *testing.T) {
	ConfigApp = &Config{
		Models: map[string]ModelConf{
			"clean": {
				Command: "llama-server\n-hf org/repo:quant\n--chat-template-kwargs '{\"enable_thinking\": true}'\n--port 1235\n",
				Host:    "127.0.0.1:1235",
			},
			"indented": {
				Command: "llama-server\n  -hf org/repo:quant\n  --chat-template-kwargs '{\"enable_thinking\": true}'\n",
				Host:    "127.0.0.1:1235",
			},
		},
	}

	cmdStr, backendURL, err := BuildCommand("clean")
	if err != nil {
		t.Fatalf("BuildCommand(clean) error: %v", err)
	}
	wantCmd := "llama-server -hf org/repo:quant --chat-template-kwargs '{\"enable_thinking\": true}' --port 1235"
	if cmdStr != wantCmd {
		t.Errorf("cmdStr = %q, want %q", cmdStr, wantCmd)
	}
	if backendURL != "http://127.0.0.1:1235" {
		t.Errorf("backendURL = %q, want http://127.0.0.1:1235", backendURL)
	}

	// Indented input must collapse to a single line while preserving the
	// space inside the quoted argument.
	cmdStr, _, err = BuildCommand("indented")
	if err != nil {
		t.Fatalf("BuildCommand(indented) error: %v", err)
	}
	if strings.Contains(cmdStr, "\n") {
		t.Errorf("cmdStr still contains a newline: %q", cmdStr)
	}
	if !strings.Contains(cmdStr, "'{\"enable_thinking\": true}'") {
		t.Errorf("quoted argument with spaces was not preserved: %q", cmdStr)
	}

	if _, _, err := BuildCommand("missing"); err == nil {
		t.Error("BuildCommand(missing) = nil error, want error")
	}
}

func TestDurations(t *testing.T) {
	ConfigApp = &Config{
		AutoUnload:   "2h",
		DrainTimeout: "30s",
		Models: map[string]ModelConf{
			"m": {ReadyTimeout: "5m"},
		},
	}
	if d := AutoUnloadDuration(); d != 2*time.Hour {
		t.Errorf("AutoUnloadDuration = %v, want 2h", d)
	}
	if d := DrainTimeout(); d != 30*time.Second {
		t.Errorf("DrainTimeout = %v, want 30s", d)
	}
	if d := ModelReadyTimeout("m"); d != 5*time.Minute {
		t.Errorf("ModelReadyTimeout(m) = %v, want 5m", d)
	}
	if d := ModelReadyTimeout("missing"); d != 0 {
		t.Errorf("ModelReadyTimeout(missing) = %v, want 0", d)
	}
}

func TestHealthEndpoint(t *testing.T) {
	ConfigApp = &Config{
		Models: map[string]ModelConf{
			"llama-model": {Host: "127.0.0.1:1235"}, // no health_endpoint set → default
			"vllm-model":  {Host: "127.0.0.1:1235", HealthEndpoint: "/v1/models"},
		},
	}

	if ep := HealthEndpoint("llama-model"); ep != "/health" {
		t.Errorf("HealthEndpoint(llama-model) = %q, want /health", ep)
	}
	if ep := HealthEndpoint("vllm-model"); ep != "/v1/models" {
		t.Errorf("HealthEndpoint(vllm-model) = %q, want /v1/models", ep)
	}
	if ep := HealthEndpoint("missing"); ep != "/health" {
		t.Errorf("HealthEndpoint(missing) = %q, want /health (fallback)", ep)
	}
}

const healthConfig = `host: 0.0.0.0:1234
debug: false
auth_token: ""
allowed_hosts: []
max_body_size: 64MB
auto_unload: 1h
drain_timeout: 30s

models:
  vllm-test:
    command: |
      ~/vllm-env/bin/vllm serve org/repo
      --port 1235 --max-model-len 8192
    host: 127.0.0.1:1235
    health_endpoint: /v1/models
    ready_timeout: 4h
`

func TestLoad_HealthEndpoint(t *testing.T) {
	if err := Load(writeConfig(t, healthConfig)); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if ep := HealthEndpoint("vllm-test"); ep != "/v1/models" {
		t.Errorf("HealthEndpoint(vllm-test) = %q, want /v1/models", ep)
	}
	if ConfigApp.Models["vllm-test"].ReadyTimeout != "4h" {
		t.Errorf("ReadyTimeout = %q, want 4h", ConfigApp.Models["vllm-test"].ReadyTimeout)
	}
}

const kindConfig = `host: 0.0.0.0:1234
debug: false
auth_token: ""
allowed_hosts: []
max_body_size: 64MB
auto_unload: 1h
drain_timeout: 30s

models:
  api-model:
    command: |
      llama-server --host 127.0.0.1 --port 1235
    host: 127.0.0.1:1235
    ready_timeout: 1m
  web-model:
    kind: web
    command: |
      sd-server --listen-port 1236
    host: 127.0.0.1:1236
    health_endpoint: /
    ready_timeout: 15m
  explicit-api:
    kind: api
    command: |
      llama-server --host 127.0.0.1 --port 1237
    host: 127.0.0.1:1237
    ready_timeout: 1m
`

func TestLoad_Kind(t *testing.T) {
	if err := Load(writeConfig(t, kindConfig)); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	// Missing kind defaults to api; explicit kinds pass through.
	if k := ModelKind("api-model"); k != KindAPI {
		t.Errorf("ModelKind(api-model) = %q, want %q", k, KindAPI)
	}
	if k := ModelKind("web-model"); k != KindWeb {
		t.Errorf("ModelKind(web-model) = %q, want %q", k, KindWeb)
	}
	if k := ModelKind("explicit-api"); k != KindAPI {
		t.Errorf("ModelKind(explicit-api) = %q, want %q", k, KindAPI)
	}
	if k := ModelKind("missing"); k != KindAPI {
		t.Errorf("ModelKind(missing) = %q, want %q", k, KindAPI)
	}
}
