package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
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
