package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/manager"
)

func TestProxyHandler_Streaming(t *testing.T) {
	// 1. Setup Mock Backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Verify headers (only for proxy requests)
		if r.Header.Get(loopDetectHeader) == "" {
			t.Errorf("Missing %s header on %s", loopDetectHeader, r.URL.Path)
		}

		// Verify body
		body, _ := io.ReadAll(r.Body)
		var actualPayload map[string]any
		if err := json.Unmarshal(body, &actualPayload); err != nil {
			t.Errorf("Failed to unmarshal body: %v", err)
		}
		if actualPayload["model"] != "test-model" || actualPayload["messages"] != "hello" {
			t.Errorf("Unexpected body content: %v", actualPayload)
		}

		// Stream response
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("Expected http.Flusher")
			return
		}

		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: chunk %d\n\n", i)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer backend.Close()

	// 2. Setup Gateway Config
	cfg := &config.Config{
		Host:       "127.0.0.1:9999",
		Debug:      true,
		AutoUnload: "1h",
		Models: map[string]config.ModelConf{
			"test-model": {
				Command:      "sleep 60",
				Host:         backend.Listener.Addr().String(),
				ReadyTimeout: "5s",
			},
		},
	}
	config.ConfigApp = cfg
	config.SortedModelNames = []string{"test-model"}

	manager.Shutdown(context.Background())

	// 3. Start Gateway Handler
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", ProxyHandler)

	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	// 4. Send Request
	payload := map[string]string{"model": "test-model", "messages": "hello"}
	jsonPayload, _ := json.Marshal(payload)

	resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// 5. Read Stream
	var fullResponse bytes.Buffer
	_, err = io.Copy(&fullResponse, resp.Body)
	if err != nil {
		t.Errorf("Failed to read response body: %v", err)
	}

	expectedResponse := "data: chunk 1\n\ndata: chunk 2\n\ndata: chunk 3\n\n"
	if fullResponse.String() != expectedResponse {
		t.Errorf("Expected response %q, got %q", expectedResponse, fullResponse.String())
	}

	// Cleanup manager
	manager.ShutdownCurrentModel()
}
