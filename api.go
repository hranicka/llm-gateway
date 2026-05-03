package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type RequestPayload struct {
	Model string `json:"model"`
}

type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}

type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error"`
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
	_ = json.NewEncoder(w).Encode(OpenAIErrorResponse{
		Error: OpenAIError{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		},
	})
}

// proxyHandler intercepts the request, switches the model, and forwards the stream.
func proxyHandler(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, "Failed to read request body", "invalid_body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var payload RequestPayload
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
	backend, err := switchModel(payload.Model)
	if err != nil {
		slog.Error("switch model failed", "model", payload.Model, "error", err)
		writeOpenAIError(w, fmt.Sprintf("Failed to load model %s: %v", payload.Model, err), "model_load_failed", http.StatusInternalServerError)
		return
	}

	target, _ := url.Parse(backend)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // flush immediately; required for SSE streaming
	proxy.ErrorLog = slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("proxy error", "error", err, "backend", backend)
		writeOpenAIError(w, fmt.Sprintf("upstream error: %v", err), "upstream_error", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// modelsHandler exposes available models to UI clients.
func modelsHandler(w http.ResponseWriter, r *http.Request) {
	models := make([]OpenAIModel, 0, len(sortedModelNames))
	for _, name := range sortedModelNames {
		models = append(models, OpenAIModel{
			ID:      name,
			Object:  "model",
			Created: 1704067200,
			OwnedBy: "gateway",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ModelList{Object: "list", Data: models})
}

// healthHandler confirms the gateway is alive.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "gateway online"})
}

// loggingMiddleware logs details of each request when debug is enabled.
func loggingMiddleware(next http.Handler) http.Handler {
	if !config.Debug {
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

// notFoundHandler returns an OpenAI-shaped 404 for unknown routes.
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, fmt.Sprintf("Path %s not found", r.URL.Path), "not_found", http.StatusNotFound)
}
