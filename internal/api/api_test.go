package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// setupWebConfig installs a config with one api model and one web model.
// The backend address of both is overridden via arguments; pass "" to keep
// a placeholder (no proxying in that test).
func setupWebConfig(t *testing.T, apiBackend, webBackend string) {
	t.Helper()
	config.ConfigApp = &config.Config{
		Host:         "127.0.0.1:9999",
		Debug:        false,
		AutoUnload:   "1h",
		DrainTimeout: "5s",
		Models: map[string]config.ModelConf{
			"test-model": {Command: "sleep 60", Host: apiBackend, ReadyTimeout: "5s"},
			"web-app":    {Kind: config.KindWeb, Command: "sleep 60", Host: webBackend, ReadyTimeout: "5s", HealthEndpoint: "/"},
		},
	}
	config.SortedModelNames = []string{"test-model", "web-app"}
	manager.Shutdown(context.Background())
}

func TestRootHandler_Index(t *testing.T) {
	setupWebConfig(t, "", "")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	RootHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `/app/web-app`) {
		t.Errorf("index page missing web app link /app/web-app:\n%s", body)
	}
	if !strings.Contains(body, "test-model") {
		t.Errorf("index page missing api model test-model:\n%s", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestRootHandler_DispatchWithoutCookie(t *testing.T) {
	setupWebConfig(t, "", "")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	RootHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/v1/models status = %d, want 200", w.Code)
	}
	var list modelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode models list: %v", err)
	}
	// Web apps are not API-addressable — only api-kind models are listed.
	if len(list.Data) != 1 || list.Data[0].ID != "test-model" {
		t.Errorf("models listed = %+v, want only test-model (web apps excluded)", list.Data)
	}

	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	w = httptest.NewRecorder()
	RootHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/health status = %d, want 200", w.Code)
	}
}

func TestModelsHandler_ExcludesWebModels(t *testing.T) {
	setupWebConfig(t, "", "")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	ModelsHandler(w, req)
	var list modelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode models list: %v", err)
	}
	for _, m := range list.Data {
		if m.ID == "web-app" {
			t.Errorf("web model %q must not be listed via /v1/models", m.ID)
		}
	}
	if len(list.Data) != 1 {
		t.Errorf("models listed = %d, want 1", len(list.Data))
	}
}

func TestAppLaunchHandler(t *testing.T) {
	setupWebConfig(t, "", "")

	// Selecting a web app sets the cookie and redirects to /.
	req := httptest.NewRequest(http.MethodGet, "/app/web-app", nil)
	w := httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("location = %q, want /", loc)
	}
	cookies := w.Result().Cookies()
	var appCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == appCookieName {
			appCookie = c
		}
	}
	if appCookie == nil || appCookie.Value != "web-app" {
		t.Fatalf("app cookie not set correctly: %+v", appCookie)
	}

	// /app/ clears the selection.
	req = httptest.NewRequest(http.MethodGet, "/app/", nil)
	w = httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("clear status = %d, want 302", w.Code)
	}
	var clearCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == appCookieName {
			clearCookie = c
		}
	}
	if clearCookie == nil || clearCookie.MaxAge > 0 {
		t.Errorf("clear cookie not set with negative MaxAge: %+v", clearCookie)
	}

	// Unknown model → 404.
	req = httptest.NewRequest(http.MethodGet, "/app/nope", nil)
	w = httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown app status = %d, want 404", w.Code)
	}

	// API model → 400.
	req = httptest.NewRequest(http.MethodGet, "/app/test-model", nil)
	w = httptest.NewRecorder()
	AppLaunchHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("api model app status = %d, want 400", w.Code)
	}
}

func TestChatProxyHandler_RejectsWebModel(t *testing.T) {
	setupWebConfig(t, "", "")

	body, _ := json.Marshal(map[string]string{"model": "web-app"})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	ChatProxyHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	assertOpenAICode(t, w, "model_not_api")
}

// TestRootHandler_AppLoadingAndErrorPages verifies the browser flow for a
// not-yet-ready web app: a loading page while the backend starts (the load
// must actually be kicked off in the background), the real UI once ready,
// and an error page with the reason once a load failed.
func TestRootHandler_AppLoadingAndErrorPages(t *testing.T) {
	var mu2 sync.Mutex
	sawPaths := map[string]bool{}
	var sawOrigin string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu2.Lock()
		sawPaths[r.URL.Path] = true
		if o := r.Header.Get("Origin"); o != "" {
			sawOrigin = o
		}
		mu2.Unlock()
		fmt.Fprintf(w, "APP-OK %s", r.URL.Path)
	}))
	defer backend.Close()

	setupWebConfig(t, "", backend.Listener.Addr().String())
	defer manager.ShutdownCurrentModel()

	htmlReq := func(path string) *http.Response {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html")
		req.AddCookie(&http.Cookie{Name: appCookieName, Value: "web-app"})
		w := httptest.NewRecorder()
		RootHandler(w, req)
		return w.Result()
	}

	// 1. Not loaded yet → loading page (no proxying), but the load is kicked.
	resp := htmlReq("/")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loading page status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Loading <b>web-app</b>") {
		t.Errorf("expected loading page, got:\n%s", body)
	}
	mu2.Lock()
	proxied := sawPaths["/"]
	mu2.Unlock()
	if proxied {
		t.Error("backend must not be hit while the loading page is shown")
	}

	// 2. The background load completes → same navigation now proxies.
	deadline := time.Now().Add(10 * time.Second)
	for manager.ModelState("web-app") != "ready" {
		if time.Now().After(deadline) {
			t.Fatalf("model never became ready, state = %q (err: %q)",
				manager.ModelState("web-app"), manager.ModelSwitchError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp = htmlReq("/")
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if got := string(body); got != "APP-OK /" {
		t.Errorf("after ready, proxied body = %q, want APP-OK /", got)
	}

	// 3. A browser-originated request (Origin = the gateway's own origin)
	// must reach the backend with Origin rewritten to the backend's origin —
	// ComfyUI's anti-rebinding middleware 403s Host/Origin mismatches.
	originReq := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader("{}"))
	originReq.Header.Set("Origin", "http://192.168.50.103:1234")
	originReq.AddCookie(&http.Cookie{Name: appCookieName, Value: "web-app"})
	w := httptest.NewRecorder()
	RootHandler(w, originReq)
	mu2.Lock()
	gotOrigin := sawOrigin
	mu2.Unlock()
	wantOrigin := "http://" + backend.Listener.Addr().String()
	if gotOrigin != wantOrigin {
		t.Errorf("backend saw Origin %q, want %q", gotOrigin, wantOrigin)
	}

	// 3. Break the backend, force a switch away and back → failure page.
	backend.Close()
	if _, release, err := manager.SwitchModel("web-app"); err == nil {
		release()
		manager.ShutdownCurrentModel()
	}
	// web-app is dead now; the next navigation kicks a load that fails fast
	// (connection refused), and afterwards the error page is shown.
	deadline = time.Now().Add(15 * time.Second)
	for {
		resp := htmlReq("/")
		resp.Body.Close()
		state := manager.ModelState("web-app")
		if state == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("model never reached failed state, state = %q", state)
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp = htmlReq("/")
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Failed to load web-app") {
		t.Errorf("expected failed page, got status %d body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), manager.ModelSwitchError()) {
		t.Errorf("failed page missing the switch error reason, body:\n%s", body)
	}
	if !strings.Contains(string(body), `href="/?retry=1"`) {
		t.Errorf("failed page missing retry link:\n%s", body)
	}
}

// TestRootHandler_AppCookieProxiesEverything verifies that a browser with a
// valid app cookie is proxied in full to the web backend — including /v1/*
// paths the app UI itself may call (e.g. sd-server's /v1/images/generations).
func TestRootHandler_AppCookieProxiesEverything(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]bool{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path] = true
		mu.Unlock()
		// "/" is the web model's health endpoint — polled by the gateway
		// itself (no proxy header), not proxied browser traffic.
		if r.URL.Path != "/" && r.Header.Get(loopDetectHeader) == "" {
			t.Errorf("Missing %s header on %s", loopDetectHeader, r.URL.Path)
		}
		fmt.Fprintf(w, "APP-OK %s", r.URL.Path)
	}))
	defer backend.Close()

	setupWebConfig(t, "127.0.0.1:1", backend.Listener.Addr().String())
	defer manager.ShutdownCurrentModel()

	for _, path := range []string{"/", "/assets/index.js", "/v1/images/generations", "/sdapi/v1/options"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: appCookieName, Value: "web-app"})
		w := httptest.NewRecorder()
		RootHandler(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, w.Code)
		}
		if got := w.Body.String(); got != "APP-OK "+path {
			t.Errorf("%s proxied body = %q, want %q", path, got, "APP-OK "+path)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// "/" is ambiguous (health poll also hits it); /v1/images/generations
	// must have come from the proxied browser request.
	if !paths["/v1/images/generations"] {
		t.Errorf("backend never received /v1/images/generations")
	}
}

// TestRootHandler_StaleAppCookieFallsBack verifies that an invalid cookie
// (unknown model or api-kind model) does not hijack requests.
func TestRootHandler_StaleAppCookieFallsBack(t *testing.T) {
	setupWebConfig(t, "", "")

	for _, value := range []string{"gone-model", "test-model", ""} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: appCookieName, Value: value})
		w := httptest.NewRecorder()
		RootHandler(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("cookie %q status = %d, want 200 (index)", value, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("cookie %q content-type = %q, want index page", value, ct)
		}
	}
}

func TestProxyHandler_ImagesProxiesAnyKind(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"created":1,"data":[]}`)
	}))
	defer backend.Close()

	config.ConfigApp = &config.Config{
		Host:         "127.0.0.1:9999",
		Debug:        false,
		AutoUnload:   "1h",
		DrainTimeout: "5s",
		Models: map[string]config.ModelConf{
			"web-app": {Kind: config.KindWeb, Command: "sleep 60", Host: backend.Listener.Addr().String(), ReadyTimeout: "5s"},
		},
	}
	config.SortedModelNames = []string{"web-app"}
	manager.Shutdown(context.Background())
	defer manager.ShutdownCurrentModel()

	payload, _ := json.Marshal(map[string]string{"model": "web-app", "prompt": "a cat"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBuffer(payload))
	w := httptest.NewRecorder()
	ProxyHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Errorf("proxied response body = %q", w.Body.String())
	}
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
			_, _ = io.Copy(io.Discard, resp.Body)
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
