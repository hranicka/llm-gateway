package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
		Host:         "127.0.0.1:9999",
		Debug:        true,
		AutoUnload:   "1h",
		DrainTimeout: "5s",
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

func assertOpenAICode(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var resp openaiErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if resp.Error.Code != wantCode {
		t.Errorf("error code = %v, want %q", resp.Error.Code, wantCode)
	}
}

func TestProxyHandler_ModelRequired(t *testing.T) {
	body, _ := json.Marshal(map[string]string{"messages": "hello"})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	ProxyHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	assertOpenAICode(t, w, "model_required")
}

func TestProxyHandler_InvalidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString("{not json"))
	w := httptest.NewRecorder()
	ProxyHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	assertOpenAICode(t, w, "invalid_body")
}

func TestProxyHandler_LoopDetection(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString("{}"))
	req.Header.Set(loopDetectHeader, "1")
	w := httptest.NewRecorder()
	ProxyHandler(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	assertOpenAICode(t, w, "proxy_loop")
}

func TestModelsHandler(t *testing.T) {
	config.SortedModelNames = []string{"a-model", "b-model"}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	ModelsHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var list modelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode models list: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) != 2 || list.Data[0].ID != "a-model" || list.Data[1].ID != "b-model" {
		t.Errorf("data = %+v, want [a-model b-model]", list.Data)
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	HealthHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestNotFoundHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	w := httptest.NewRecorder()
	NotFoundHandler(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	assertOpenAICode(t, w, "not_found")
}

func TestProxyHandler_SerializesConcurrentRequests(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		fmt.Fprint(w, "{}")
	}))
	defer backend.Close()

	cfg := &config.Config{
		Host:         "127.0.0.1:9999",
		Debug:        false,
		AutoUnload:   "1h",
		DrainTimeout: "5s",
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
	defer manager.ShutdownCurrentModel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", ProxyHandler)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	payload, _ := json.Marshal(map[string]string{"model": "test-model"})
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json", bytes.NewBuffer(payload))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			statuses[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	mu.Lock()
	gotMax := maxInFlight
	mu.Unlock()
	if gotMax != 1 {
		t.Errorf("max concurrent backend requests = %d, want 1", gotMax)
	}
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, status)
		}
	}
}
