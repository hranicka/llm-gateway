package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// resetState clears all manager globals, kills any leftover backend process,
// and re-initializes the shutdown context so each test starts clean.
func resetState(t *testing.T) {
	t.Helper()
	StopAutoUnload()
	autoUnloadD = 0
	clearActive()
	Shutdown(context.Background())
	t.Cleanup(clearActive)
}

// clearActive kills the active process group (if any) and resets the
// active-model state.
func clearActive() {
	StopAutoUnload()
	mu.Lock()
	if activeCmd != nil {
		killProcessGroup(activeCmd)
	}
	activeCmd = nil
	currentModel = ""
	currentBackend = ""
	lastAccess.Store(0)
	activeRequests.Store(0)
	lastExit = time.Time{}
	mu.Unlock()
	statusMu.Lock()
	readyModel = ""
	readyCmd = nil
	switchTarget = ""
	switchErr = nil
	statusMu.Unlock()
}

// newHealthBackend returns the address of a stub backend whose /health (and
// every other path) returns 200, so waitForServerOrExit becomes ready at once.
func newHealthBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func setTestConfig(models map[string]config.ModelConf) {
	config.ConfigApp = &config.Config{
		Host:         "127.0.0.1:0",
		AutoUnload:   "1h",
		DrainTimeout: "1s",
		Models:       models,
	}
}

func TestSwitchModel_Lifecycle(t *testing.T) {
	resetState(t)
	backend := newHealthBackend(t)
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 30", Host: backend, ReadyTimeout: "5s"},
	})

	url, release, err := SwitchModel("m")
	if err != nil {
		t.Fatalf("SwitchModel error: %v", err)
	}
	if want := "http://" + backend; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
	if activeRequests.Load() != 1 {
		t.Errorf("activeRequests = %d, want 1", activeRequests.Load())
	}

	// Fast path: requesting the already-loaded model returns the same URL.
	url2, release2, err := SwitchModel("m")
	if err != nil {
		t.Fatalf("SwitchModel (fast path) error: %v", err)
	}
	if url2 != url {
		t.Errorf("fast path url = %q, want %q", url2, url)
	}
	if activeRequests.Load() != 2 {
		t.Errorf("activeRequests = %d, want 2", activeRequests.Load())
	}

	release()
	release2()
	if activeRequests.Load() != 0 {
		t.Errorf("activeRequests after release = %d, want 0", activeRequests.Load())
	}

	ShutdownCurrentModel()
	mu.RLock()
	model := currentModel
	cmd := activeCmd
	mu.RUnlock()
	if model != "" || cmd != nil {
		t.Errorf("after shutdown: currentModel=%q activeCmd=%v, want empty/nil", model, cmd)
	}
}

func TestSwitchModel_UnknownModel(t *testing.T) {
	resetState(t)
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 30", Host: "127.0.0.1:1", ReadyTimeout: "5s"},
	})
	if _, _, err := SwitchModel("nope"); err == nil {
		t.Error("SwitchModel(nope) = nil error, want error")
	}
}

func TestSwitchModel_ShuttingDown(t *testing.T) {
	resetState(t)
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 30", Host: "127.0.0.1:1", ReadyTimeout: "5s"},
	})
	ShutdownCancel()
	if _, _, err := SwitchModel("m"); err == nil {
		t.Error("SwitchModel during shutdown = nil error, want error")
	}
}

func TestSwitchModel_ProcessExitResetsState(t *testing.T) {
	resetState(t)
	backend := newHealthBackend(t)
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 0.2", Host: backend, ReadyTimeout: "5s"},
	})

	_, release, err := SwitchModel("m")
	if err != nil {
		t.Fatalf("SwitchModel error: %v", err)
	}
	release()

	// The short-lived process exits on its own; monitorProcess must clear state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.RLock()
		done := currentModel == "" && activeCmd == nil
		mu.RUnlock()
		if done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("state not cleared after process exit")
}

// newGatedBackend returns a stub backend that answers 503 until open() is
// called, then 200 — a model that takes a while to become ready.
func newGatedBackend(t *testing.T) (addr string, open func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() { once.Do(func() { close(gate) }) }
}

// within runs fn and fails the test if it takes longer than d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked for more than %v", what, d)
	}
}

func TestStatusQueries_DoNotBlockDuringLoad(t *testing.T) {
	resetState(t)
	addr, open := newGatedBackend(t)
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 30", Host: addr, ReadyTimeout: "10s"},
	})

	loaded := make(chan error, 1)
	go func() {
		_, release, err := SwitchModel("m")
		if err == nil {
			release()
		}
		loaded <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for ModelState("m") != "starting" {
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want starting", ModelState("m"))
		}
		time.Sleep(20 * time.Millisecond)
	}

	within(t, 500*time.Millisecond, "status queries during load", func() {
		if got := CurrentModel(); got != "" {
			t.Errorf("CurrentModel during load = %q, want empty", got)
		}
		if got := ModelState("m"); got != "starting" {
			t.Errorf("ModelState during load = %q, want starting", got)
		}
		if got := ModelSwitchError(); got != "" {
			t.Errorf("ModelSwitchError during load = %q, want empty", got)
		}
		LoadModelAsync("m") // already in flight: must return at once
		TouchModel()
	})

	open()
	if err := <-loaded; err != nil {
		t.Fatalf("SwitchModel error: %v", err)
	}
	if got := ModelState("m"); got != "ready" {
		t.Errorf("ModelState after load = %q, want ready", got)
	}
	if got := CurrentModel(); got != "m" {
		t.Errorf("CurrentModel after load = %q, want m", got)
	}
}

func TestFailedStart_RecordsExitAndError(t *testing.T) {
	resetState(t)
	addr, _ := newGatedBackend(t) // never opens
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 30", Host: addr, ReadyTimeout: "1s"},
	})

	if _, _, err := SwitchModel("m"); err == nil {
		t.Fatal("SwitchModel = nil error, want readiness timeout")
	}
	if got := ModelState("m"); got != "failed" {
		t.Errorf("ModelState = %q, want failed", got)
	}
	if ModelSwitchError() == "" {
		t.Error("ModelSwitchError empty after failed load")
	}
	mu.RLock()
	exited := lastExit
	mu.RUnlock()
	if time.Since(exited) > 5*time.Second || exited.IsZero() {
		t.Errorf("lastExit = %v, want a fresh timestamp after the failed start", exited)
	}
}

func TestAutoUnload_SparesInFlightRequest(t *testing.T) {
	resetState(t)
	addr := newHealthBackend(t)
	setTestConfig(map[string]config.ModelConf{
		"m": {Command: "sleep 30", Host: addr, ReadyTimeout: "5s"},
	})
	StartAutoUnload(300 * time.Millisecond)

	_, release, err := SwitchModel("m")
	if err != nil {
		t.Fatalf("SwitchModel error: %v", err)
	}

	// Well past the idle window while the request is still open.
	time.Sleep(900 * time.Millisecond)
	if got := CurrentModel(); got != "m" {
		t.Fatalf("model %q after the idle window with a request in flight, want m kept loaded", got)
	}

	release()
	deadline := time.Now().Add(3 * time.Second)
	for CurrentModel() != "" {
		if time.Now().After(deadline) {
			t.Fatal("model not unloaded after the request ended and the idle window elapsed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
