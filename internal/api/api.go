package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/manager"
)

type requestPayload struct {
	Model string `json:"model"`
}

type openaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []openaiModel `json:"data"`
}

type openaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

type openaiErrorResponse struct {
	Error openaiError `json:"error"`
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode json response", "error", err)
	}
}

func writeOpenAIError(w http.ResponseWriter, message, code string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(openaiErrorResponse{
		Error: openaiError{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		},
	})
	if err != nil {
		slog.Error("failed to encode openai error response", "error", err)
	}
}

// loopDetectHeader is stamped on every proxied request. If the gateway
// receives a request that already carries this header it means the backend
// address is pointing back at the gateway itself, and we abort immediately.
const loopDetectHeader = "X-Llm-Gateway-Forwarded"

// proxySlots serializes proxied generation requests (chat completions and
// images). Only one generation runs at a time: clients that fire concurrent
// sessions (e.g. parallel agent subagents) queue here instead of interleaving
// on the single loaded backend — and instead of triggering model switches
// that would kill the in-flight stream.
//
// Web-app traffic (kind: web) deliberately bypasses this slot: browsers open
// long-lived websocket connections that would block API generation forever,
// and UI assets/polls are safe to run concurrently.
var proxySlots = make(chan struct{}, 1)

// appCookieName stores which web app the browser selected via /app/<name>.
// All requests carrying a valid app cookie are proxied to that backend —
// including its own /v1/* endpoints, so UIs calling e.g. sd-server's
// /v1/images/generations keep working through the gateway.
const appCookieName = "llm_gateway_app"

// RootHandler routes every request that is not /app/<name>:
//
//  1. /health always answers for the gateway itself (monitoring).
//  2. A valid app cookie sends everything else to the selected web app,
//     including /v1/* paths the app's own UI may call.
//  3. Without a cookie, the OpenAI-style API routes and the index page
//     are served as usual.
func RootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		HealthHandler(w, r)
		return
	}

	if name, ok := appModelFromRequest(r); ok {
		proxyWebApp(w, r, name)
		return
	}

	switch r.URL.Path {
	case "/":
		IndexHandler(w, r)
	case "/v1/models":
		ModelsHandler(w, r)
	case "/v1/chat/completions", "/v1/completions":
		ChatProxyHandler(w, r)
	case "/v1/images/generations", "/v1/images/edits":
		ProxyHandler(w, r)
	default:
		NotFoundHandler(w, r)
	}
}

// appModelFromRequest returns the web model selected by the app cookie.
func appModelFromRequest(r *http.Request) (string, bool) {
	c, err := r.Cookie(appCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	if _, ok := config.ConfigApp.Models[c.Value]; !ok {
		return "", false
	}
	if config.ModelKind(c.Value) != config.KindWeb {
		return "", false
	}
	return c.Value, true
}

// AppLaunchHandler serves /app/<name>: it pins the browser to a web-app
// backend via cookie and redirects to /, where all requests are then
// reverse-proxied to the app (including websockets). /app/ clears the
// selection and returns to the index page.
func AppLaunchHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/app/")
	name = strings.Trim(name, "/")

	if name == "" {
		http.SetCookie(w, &http.Cookie{
			Name:   appCookieName,
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	m, ok := config.ConfigApp.Models[name]
	if !ok {
		slog.Warn("app launch for unknown model", "model", name)
		http.Error(w, fmt.Sprintf("Unknown app %q", name), http.StatusNotFound)
		return
	}
	if m.Kind != config.KindWeb {
		slog.Warn("app launch for non-web model", "model", name, "kind", m.Kind)
		http.Error(w, fmt.Sprintf("Model %q is an API model — use /v1/chat/completions or /v1/images/generations instead of /app/", name), http.StatusBadRequest)
		return
	}

	slog.Info("browser selected web app", "model", name, "remote_addr", r.RemoteAddr)
	http.SetCookie(w, &http.Cookie{
		Name:     appCookieName,
		Value:    name,
		Path:     "/",
		MaxAge:   31536000, // one year; /app/ clears it earlier
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// isWebSocketRequest reports whether the request upgrades to a websocket.
func isWebSocketRequest(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// proxyWebApp switches to the web-app backend and reverse-proxies the
// request in full. Websocket connections are kept alive against the
// auto-unload timer for as long as the browser holds them open, but they
// never block model switches (no active-request slot is held).
func proxyWebApp(w http.ResponseWriter, r *http.Request, name string) {
	if r.Header.Get(loopDetectHeader) != "" {
		slog.Error("proxy loop detected — backend is pointing at the gateway itself",
			"path", r.URL.Path, "remote_addr", r.RemoteAddr)
		http.Error(w, "Proxy loop detected: the gateway is forwarding requests to itself. "+
			"Ensure model backend ports differ from the gateway port.", http.StatusBadGateway)
		return
	}

	backend, release, err := manager.SwitchModel(name)
	if err != nil {
		slog.Error("switch model failed", "model", name, "error", err)
		http.Error(w, fmt.Sprintf("Failed to load app %s: %v", name, err), http.StatusBadGateway)
		return
	}

	if isWebSocketRequest(r) {
		// An open tab must neither block switches (release immediately) nor
		// let the idle timer unload the app underneath the websocket.
		release()
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					manager.TouchModel()
				case <-done:
					return
				}
			}
		}()
	} else {
		defer release()
	}

	proxy, perr := newProxy(backend)
	if perr != nil {
		slog.Error("invalid backend URL", "backend", backend, "error", perr)
		http.Error(w, fmt.Sprintf("Invalid backend URL: %v", perr), http.StatusInternalServerError)
		return
	}
	proxy.ServeHTTP(w, r)
}

// ChatProxyHandler proxies OpenAI chat/completion requests, rejecting
// web-app models that have no chat endpoint.
func ChatProxyHandler(w http.ResponseWriter, r *http.Request) {
	name, ok := parseModelRequest(w, r)
	if !ok {
		return
	}
	if config.ModelKind(name) == config.KindWeb {
		slog.Warn("chat request for web model", "model", name, "path", r.URL.Path)
		writeOpenAIError(w,
			fmt.Sprintf("Model %s is a web app (kind: web) with no chat endpoint — open /app/%s in a browser, or use /v1/images/generations for image models.", name, name),
			"model_not_api", http.StatusBadRequest)
		return
	}
	switchAndProxy(w, r, name)
}

// ProxyHandler proxies generation requests that accept any configured
// backend kind (e.g. /v1/images/generations for sd-server).
func ProxyHandler(w http.ResponseWriter, r *http.Request) {
	name, ok := parseModelRequest(w, r)
	if !ok {
		return
	}
	switchAndProxy(w, r, name)
}

// parseModelRequest reads and validates the request body, returning the
// requested model name. It also rejects proxy loops (a backend host pointing
// back at the gateway).
func parseModelRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Header.Get(loopDetectHeader) != "" {
		slog.Error("proxy loop detected — backend is pointing at the gateway itself",
			"path", r.URL.Path, "remote_addr", r.RemoteAddr)
		writeOpenAIError(w,
			"Proxy loop detected: the gateway is forwarding requests to itself. "+
				"Ensure model backend ports differ from the gateway port.",
			"proxy_loop", http.StatusBadGateway)
		return "", false
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, "Failed to read request body", "invalid_body", http.StatusBadRequest)
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	r.ContentLength = int64(len(bodyBytes))

	var payload requestPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		slog.Warn("failed to parse request body", "error", err)
		writeOpenAIError(w, "Invalid request body", "invalid_body", http.StatusBadRequest)
		return "", false
	}
	if payload.Model == "" {
		writeOpenAIError(w, "Model field is required", "model_required", http.StatusBadRequest)
		return "", false
	}

	if _, ok := config.ConfigApp.Models[payload.Model]; !ok {
		slog.Error("model not found", "model", payload.Model)
		writeOpenAIError(w, fmt.Sprintf("Model %s not found in configuration", payload.Model), "model_not_found", http.StatusBadRequest)
		return "", false
	}

	slog.Debug("request received", "model", payload.Model, "method", r.Method, "path", r.URL.Path)
	return payload.Model, true
}

// switchAndProxy waits for the generation slot, switches to the model and
// forwards the request (streaming included).
func switchAndProxy(w http.ResponseWriter, r *http.Request, name string) {
	// Wait for the previous request to finish before touching backend state.
	select {
	case proxySlots <- struct{}{}:
		defer func() { <-proxySlots }()
	case <-r.Context().Done():
		slog.Debug("client disconnected while waiting for its turn",
			"model", name, "remote_addr", r.RemoteAddr)
		return
	}

	backend, release, err := manager.SwitchModel(name)
	if err != nil {
		slog.Error("switch model failed", "model", name, "error", err)
		writeOpenAIError(w, fmt.Sprintf("Failed to load model %s: %v", name, err), "model_load_failed", http.StatusInternalServerError)
		return
	}
	defer release()

	proxy, perr := newProxy(backend)
	if perr != nil {
		slog.Error("invalid backend URL", "backend", backend, "error", perr)
		writeOpenAIError(w, fmt.Sprintf("Invalid backend URL: %v", perr), "internal_error", http.StatusInternalServerError)
		return
	}
	proxy.ServeHTTP(w, r)
}

// newProxy builds the reverse proxy for a backend URL. Websocket upgrades
// are handled transparently by httputil.ReverseProxy.
func newProxy(backend string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		FlushInterval: -1, // flush immediately; required for SSE streaming
		ErrorLog:      slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			req.Out.Header.Set(loopDetectHeader, "1")
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 400 {
				slog.Warn("backend returned error",
					"status", resp.StatusCode,
					"backend", backend,
					"path", resp.Request.URL.Path,
				)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("proxy error", "error", err, "backend", backend)
			writeOpenAIError(w, fmt.Sprintf("upstream error: %v", err), "upstream_error", http.StatusBadGateway)
		},
	}, nil
}

// indexPageCSS is the inline stylesheet for the index page.
const indexPageCSS = `
  :root { color-scheme: dark; }
  body { font-family: system-ui, sans-serif; background: #0f1115; color: #e2e6ee; margin: 0; padding: 2rem 1rem; }
  main { max-width: 640px; margin: 0 auto; }
  h1 { font-size: 1.4rem; margin-bottom: .25rem; }
  h2 { font-size: .8rem; text-transform: uppercase; letter-spacing: .08em; color: #8b93a5; margin: 1.75rem 0 .5rem; }
  .status { color: #8b93a5; font-size: .9rem; margin: 0 0 1rem; }
  .status b { color: #7cc4a0; }
  ul { list-style: none; padding: 0; margin: 0; }
  li { background: #171b23; border: 1px solid #262c38; border-radius: 8px; padding: .65rem .9rem; margin-bottom: .5rem; display: flex; justify-content: space-between; gap: 1rem; align-items: center; }
  a { color: #7aa2f7; text-decoration: none; font-weight: 600; }
  a:hover { text-decoration: underline; }
  code { background: #10141b; border: 1px solid #262c38; border-radius: 4px; padding: .15rem .4rem; font-size: .8rem; color: #9fb6d8; }
  .hint { color: #8b93a5; font-size: .8rem; }
`

// IndexHandler serves a small landing page listing web apps (clickable) and
// API models. It is only shown when no app cookie is present.
func IndexHandler(w http.ResponseWriter, r *http.Request) {
	var apps, apiModels strings.Builder
	for _, name := range config.SortedModelNames {
		esc := html.EscapeString(name)
		if config.ModelKind(name) == config.KindWeb {
			fmt.Fprintf(&apps, "<li><a href=\"/app/%s\">%s</a><span class=\"hint\">open UI</span></li>\n", esc, esc)
		} else {
			fmt.Fprintf(&apiModels, "<li><span>%s</span><code>POST /v1/chat/completions</code></li>\n", esc)
		}
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<title>LLM Gateway</title>\n<style>" + indexPageCSS + "</style>\n</head>\n<body>\n<main>\n")
	b.WriteString("<h1>LLM Gateway</h1>\n")

	if active := manager.CurrentModel(); active != "" {
		fmt.Fprintf(&b, "<p class=\"status\">Active model: <b>%s</b></p>\n", html.EscapeString(active))
	} else {
		b.WriteString("<p class=\"status\">No model loaded — it starts on the first request.</p>\n")
	}

	if apps.Len() > 0 {
		b.WriteString("<h2>Web apps</h2>\n<ul>\n" + apps.String() + "</ul>\n")
	}
	if apiModels.Len() > 0 {
		b.WriteString("<h2>API models</h2>\n<ul>\n" + apiModels.String() + "</ul>\n")
	}

	b.WriteString("<p class=\"hint\">API endpoints: <code>/v1/chat/completions</code>, <code>/v1/completions</code>, <code>/v1/images/generations</code>, <code>/v1/models</code>, <code>/health</code></p>\n")
	b.WriteString("</main>\n</body>\n</html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := io.WriteString(w, b.String()); err != nil {
		slog.Debug("failed to write index page", "error", err)
	}
}

// ModelsHandler exposes available API models to OpenAI clients. Web apps
// (kind: web) are omitted: they are not addressable through chat or image
// model fields (their UIs live under /app/<name>), and listing them only
// invites clients to select something unusable.
func ModelsHandler(w http.ResponseWriter, r *http.Request) {
	models := make([]openaiModel, 0, len(config.SortedModelNames))
	for _, name := range config.SortedModelNames {
		if config.ModelKind(name) != config.KindAPI {
			continue
		}
		models = append(models, openaiModel{
			ID:      name,
			Object:  "model",
			Created: 1704067200,
			OwnedBy: "gateway",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(modelList{Object: "list", Data: models}); err != nil {
		slog.Error("failed to encode models list", "error", err)
	}
}

// HealthHandler confirms the gateway is alive.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "gateway online"})
}

// LoggingMiddleware logs details of each request when debug is enabled.
func LoggingMiddleware(next http.Handler) http.Handler {
	if !config.ConfigApp.Debug {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("incoming request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
		)
		next.ServeHTTP(w, r)
	})
}

// NotFoundHandler returns an OpenAI-shaped 404 for unknown routes.
func NotFoundHandler(w http.ResponseWriter, r *http.Request) {
	slog.Warn("unknown route", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
	writeOpenAIError(w, fmt.Sprintf("Path %s not found", r.URL.Path), "not_found", http.StatusNotFound)
}
