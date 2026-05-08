package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

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
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, message, code string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openaiErrorResponse{
		Error: openaiError{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		},
	})
}

// loopDetectHeader is stamped on every proxied request. If the gateway
// receives a request that already carries this header it means the backend
// address is pointing back at the gateway itself, and we abort immediately.
const loopDetectHeader = "X-Llm-Gateway-Forwarded"

// ProxyHandler intercepts the request, switches the model, and forwards the stream.
func ProxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(loopDetectHeader) != "" {
		slog.Error("proxy loop detected — backend is pointing at the gateway itself",
			"path", r.URL.Path, "remote_addr", r.RemoteAddr)
		writeOpenAIError(w,
			"Proxy loop detected: the gateway is forwarding requests to itself. "+
				"Ensure model backend ports differ from the gateway port.",
			"proxy_loop", http.StatusBadGateway)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, "Failed to read request body", "invalid_body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var payload requestPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		slog.Warn("failed to parse request body", "error", err)
		writeOpenAIError(w, "Invalid request body", "invalid_body", http.StatusBadRequest)
		return
	}
	if payload.Model == "" {
		writeOpenAIError(w, "Model field is required", "model_required", http.StatusBadRequest)
		return
	}

	slog.Debug("request received", "model", payload.Model, "method", r.Method, "path", r.URL.Path)
	backend, err := manager.SwitchModel(payload.Model)
	if err != nil {
		slog.Error("switch model failed", "model", payload.Model, "error", err)
		writeOpenAIError(w, fmt.Sprintf("Failed to load model %s: %v", payload.Model, err), "model_load_failed", http.StatusInternalServerError)
		return
	}

	target, err := url.Parse(backend)
	if err != nil {
		slog.Error("invalid backend URL", "backend", backend, "error", err)
		writeOpenAIError(w, fmt.Sprintf("Invalid backend URL: %v", err), "internal_error", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // flush immediately; required for SSE streaming
	proxy.ErrorLog = slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Set(loopDetectHeader, "1")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("proxy error", "error", err, "backend", backend)
		writeOpenAIError(w, fmt.Sprintf("upstream error: %v", err), "upstream_error", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// ModelsHandler exposes available models to UI clients.
func ModelsHandler(w http.ResponseWriter, r *http.Request) {
	models := make([]openaiModel, 0, len(config.SortedModelNames))
	for _, name := range config.SortedModelNames {
		models = append(models, openaiModel{
			ID:      name,
			Object:  "model",
			Created: 1704067200,
			OwnedBy: "gateway",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelList{Object: "list", Data: models})
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
	writeOpenAIError(w, fmt.Sprintf("Path %s not found", r.URL.Path), "not_found", http.StatusNotFound)
}
