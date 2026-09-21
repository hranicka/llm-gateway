package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/manager"
)

const testToken = "test-token-123"

// loadAuthConfig writes and loads a config with auth enabled, a host
// allowlist and a tiny body limit, so all middleware paths are exercised.
func loadAuthConfig(t *testing.T) {
	t.Helper()
	content := `host: 127.0.0.1:9999
debug: false
auth_token: "` + testToken + `"
allowed_hosts:
  - localhost:9999
  - gem12.lan
max_body_size: 1KB
auto_unload: 1h
drain_timeout: 5s

models:
  web-app:
    kind: web
    command: |
      sleep 60
    host: 127.0.0.1:1
    health_endpoint: /
    ready_timeout: 5s
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() {
		manager.ShutdownCurrentModel()
	})
}

// securityHandler wraps the full routing stack in the middleware, like main.
func securityHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/", AppLaunchHandler)
	mux.HandleFunc("/", RootHandler)
	return SecurityMiddleware(mux)
}

func authCookieValue() string {
	return base64.StdEncoding.EncodeToString([]byte(testToken))
}

func TestSecurityMiddleware_HealthAlwaysOpen(t *testing.T) {
	loadAuthConfig(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	securityHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/health with no auth and wrong Host: status = %d, want 200", w.Code)
	}
}

func TestSecurityMiddleware_HostCheck(t *testing.T) {
	loadAuthConfig(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	securityHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("disallowed Host status = %d, want 403", w.Code)
	}

	// DNS-rebinding style: attacker hostname with the gateway's port.
	req.Host = "evil.example.com:9999"
	w = httptest.NewRecorder()
	securityHandler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("rebind-style Host status = %d, want 403", w.Code)
	}

	for _, host := range []string{"gem12.lan", "localhost:9999", "LOCALHOST:9999", "gem12.lan:1"} {
		req.Host = host
		w = httptest.NewRecorder()
		securityHandler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("allowed Host %q status = %d, want 200", host, w.Code)
		}
	}
}

func TestSecurityMiddleware_Auth(t *testing.T) {
	loadAuthConfig(t)

	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		securityHandler().ServeHTTP(w, r)
		return w
	}

	// No credentials → 401 with WWW-Authenticate.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gem12.lan"
	w := do(req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth status = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 response missing WWW-Authenticate header")
	}

	// Wrong bearer → 401.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gem12.lan"
	req.Header.Set("Authorization", "Bearer wrong-token")
	if w = do(req); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer status = %d, want 401", w.Code)
	}

	// Non-bearer Authorization → 401 (no cookie fallthrough).
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gem12.lan"
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: authCookieValue()})
	if w = do(req); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong scheme with valid cookie status = %d, want 401", w.Code)
	}

	// Correct bearer → 200.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gem12.lan"
	req.Header.Set("Authorization", "Bearer "+testToken)
	if w = do(req); w.Code != http.StatusOK {
		t.Errorf("bearer status = %d, want 200", w.Code)
	}

	// Correct auth cookie → 200.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gem12.lan"
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: authCookieValue()})
	if w = do(req); w.Code != http.StatusOK {
		t.Errorf("auth cookie status = %d, want 200", w.Code)
	}

	// Garbage cookie value (not base64) → 401.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "gem12.lan"
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "!!!"})
	if w = do(req); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage cookie status = %d, want 401", w.Code)
	}
}

func TestSecurityMiddleware_AppTokenExchange(t *testing.T) {
	loadAuthConfig(t)
	manager.Shutdown(context.Background())

	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		securityHandler().ServeHTTP(w, r)
		return w
	}

	// Valid ?token= passes the middleware; AppLaunchHandler sets both cookies.
	req := httptest.NewRequest(http.MethodGet, "/app/web-app?token="+testToken, nil)
	req.Host = "gem12.lan"
	w := do(req)
	if w.Code != http.StatusFound {
		t.Fatalf("token launch status = %d, want 302 (body: %s)", w.Code, w.Body.String())
	}
	var authCookie, appCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case authCookieName:
			authCookie = c
		case appCookieName:
			appCookie = c
		}
	}
	if authCookie == nil || authCookie.Value != authCookieValue() {
		t.Fatalf("auth cookie not set correctly: %+v", authCookie)
	}
	if !authCookie.HttpOnly || authCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("auth cookie flags wrong: %+v", authCookie)
	}
	if appCookie == nil || appCookie.Value != "web-app" {
		t.Errorf("app cookie not set: %+v", appCookie)
	}

	// The exchanged cookie now authenticates plain requests.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "gem12.lan"
	req.AddCookie(authCookie)
	if w = do(req); w.Code != http.StatusOK {
		t.Errorf("index with auth cookie status = %d, want 200", w.Code)
	}

	// Wrong ?token= → 401 at the middleware.
	req = httptest.NewRequest(http.MethodGet, "/app/web-app?token=wrong", nil)
	req.Host = "gem12.lan"
	if w = do(req); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token launch status = %d, want 401", w.Code)
	}
}

func TestSecurityMiddleware_DisabledAuthOpen(t *testing.T) {
	// auth_token: "" must keep the gateway open (explicit LAN choice).
	content := strings.Replace(`host: 127.0.0.1:9999
debug: false
auth_token: "x"
allowed_hosts: []
max_body_size: 64MB
auto_unload: 1h
drain_timeout: 5s

models:
  web-app:
    kind: web
    command: |
      sleep 60
    host: 127.0.0.1:1
    health_endpoint: /
    ready_timeout: 5s
`, `auth_token: "x"`, `auth_token: ""`, 1)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	securityHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("disabled-auth index status = %d, want 200", w.Code)
	}
}

func TestAppLaunch_RejectsCrossSite(t *testing.T) {
	setupWebConfig(t, "", "")

	req := httptest.NewRequest(http.MethodGet, "/app/web-app", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site launch status = %d, want 403", w.Code)
	}

	// Same-site and header-less requests are fine.
	req = httptest.NewRequest(http.MethodGet, "/app/web-app", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	w = httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("same-origin launch status = %d, want 302", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/app/web-app", nil)
	w = httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("no-header launch status = %d, want 302", w.Code)
	}
}

func TestAppLaunch_SecureCookiesBehindHTTPSProxy(t *testing.T) {
	loadAuthConfig(t)
	manager.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/app/web-app?token="+testToken, nil)
	req.Host = "gem12.lan"
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	securityHandler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("launch status = %d, want 302", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if !c.Secure {
			t.Errorf("cookie %s missing Secure flag behind X-Forwarded-Proto: https", c.Name)
		}
	}
}

func TestParseModelRequest_BodyTooLarge(t *testing.T) {
	loadAuthConfig(t) // max_body_size: 1KB

	body := `{"model": "web-app", "prompt": "` + strings.Repeat("x", 4096) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	w := httptest.NewRecorder()
	ProxyHandler(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413 (body: %s)", w.Code, w.Body.String())
	}
	assertOpenAICode(t, w, "body_too_large")
}
