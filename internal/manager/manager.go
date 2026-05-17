package manager

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"llm-gateway/internal/config"
)

var (
	// mu guards activeCmd, currentModel, currentBackend.
	// RLock is taken on the fast path (model already loaded);
	// Lock is held for the duration of a model switch.
	mu             sync.RWMutex
	activeCmd      *exec.Cmd
	currentModel   string
	currentBackend string

	// lastAccess is the Unix nanosecond timestamp of the last successful
	// SwitchModel call; 0 means no model is loaded.
	lastAccess atomic.Int64

	// autoUnload timer state.
	autoUnloadD time.Duration
	timerMu     sync.Mutex
	unloadTimer *time.Timer

	// activeRequests tracks the number of concurrent proxy requests for the
	// currently loaded model.
	activeRequests atomic.Int32

	// shutdownCtx and shutdownCancel are set by the main package before starting the server.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
)

// Shutdown initializes the shutdown context. Must be called before any
// other manager functions that use ShutdownCtx or ShutdownCancel.
func Shutdown(ctx context.Context) {
	shutdownCtx, shutdownCancel = context.WithCancel(ctx)
}

// ShutdownCtx returns the shutdown context.
func ShutdownCtx() context.Context {
	return shutdownCtx
}

// ShutdownCancel returns the cancel function for shutdown.
func ShutdownCancel() {
	shutdownCancel()
}

// StartAutoUnload enables idle-unloading with the given duration.
// Must be called before the server starts accepting requests.
func StartAutoUnload(d time.Duration) {
	autoUnloadD = d
}

// StopAutoUnload stops the auto-unload timer and must be called during
// shutdown to avoid a lingering goroutine.
func StopAutoUnload() {
	timerMu.Lock()
	if unloadTimer != nil {
		unloadTimer.Stop()
		unloadTimer = nil
	}
	timerMu.Unlock()
}

// ReleaseModel decrements the active request counter.
func ReleaseModel() {
	activeRequests.Add(-1)
}

// resetAutoUnload records activity and reschedules the auto-unload timer so
// it fires exactly autoUnloadD after the last request.
func resetAutoUnload() {
	if autoUnloadD <= 0 {
		return
	}
	lastAccess.Store(time.Now().UnixNano())
	timerMu.Lock()
	if unloadTimer != nil {
		unloadTimer.Stop()
	}
	unloadTimer = time.AfterFunc(autoUnloadD, doAutoUnload)
	timerMu.Unlock()
}

// doAutoUnload is called by the timer. It guards against spurious fires
// (e.g. timer stopped after Stop returned false) by re-checking idle time.
func doAutoUnload() {
	mu.Lock()
	defer mu.Unlock()
	if activeCmd == nil {
		return
	}
	if time.Since(time.Unix(0, lastAccess.Load())) < autoUnloadD {
		return
	}
	slog.Info("auto-unloading idle model", "model", currentModel)
	shutdownCurrentModelLocked()
}

// processAlive returns true if cmd's process is still running.
// Signal(0) doesn't actually send a signal; it just probes existence.
func processAlive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}

// logPipe streams a child stdout/stderr pipe into slog at the given level.
func logPipe(r io.ReadCloser, level slog.Level, attrs ...any) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := make([]any, 0, len(attrs)+2)
		fields = append(fields, attrs...)
		fields = append(fields, "line", scanner.Text())
		slog.Log(context.Background(), level, "backend", fields...)
	}
}

// SwitchModel ensures modelName is loaded and returns its backend URL and a release function.
//
// Fast path: if the requested model is already loaded and alive, return its
// URL immediately under a read lock. Otherwise acquire the write lock, shut
// down any running model, and start the new one. Concurrent requests for the
// same model naturally coalesce because they all observe the loaded state on
// the fast path once the first switch completes.
//
// The returned release function must be called when the request finishes to
// allow the model to be switched or unloaded.
func SwitchModel(modelName string) (string, func(), error) {
	if shutdownCtx != nil && shutdownCtx.Err() != nil {
		return "", nil, fmt.Errorf("server shutting down")
	}

	mu.RLock()
	if currentModel == modelName && processAlive(activeCmd) {
		backend := currentBackend
		activeRequests.Add(1)
		mu.RUnlock()
		resetAutoUnload()
		return backend, sync.OnceFunc(ReleaseModel), nil
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()

	// Re-check after upgrading to the write lock.
	if currentModel == modelName && processAlive(activeCmd) {
		resetAutoUnload()
		activeRequests.Add(1)
		return currentBackend, sync.OnceFunc(ReleaseModel), nil
	}
	if err := startModelLocked(modelName); err != nil {
		return "", nil, err
	}
	resetAutoUnload()
	activeRequests.Add(1)
	return currentBackend, sync.OnceFunc(ReleaseModel), nil
}

// startModelLocked shuts down any current model and starts modelName.
// Caller must hold mu (write lock).
func startModelLocked(modelName string) error {
	cmdStr, backendURL, err := config.BuildCommand(modelName)
	if err != nil {
		return err
	}

	shutdownCurrentModelLocked()

	slog.Info("starting model", "model", modelName, "command", cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	// Run the model process in its own process group so terminal signals (Ctrl+C)
	// aren't forwarded to it, and so we can kill the whole group at shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	go logPipe(stdout, slog.LevelInfo, "model", modelName)
	go logPipe(stderr, slog.LevelWarn, "model", modelName)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	activeCmd = cmd
	currentModel = modelName
	currentBackend = backendURL

	go monitorProcess(cmd, modelName)

	if err := waitForServerOrExit(ShutdownCtx(), cmd, backendURL+"/health", config.ModelReadyTimeout(modelName)); err != nil {
		if activeCmd == cmd {
			killProcessGroup(cmd)
			clearStateLocked()
		}
		return fmt.Errorf("server failed to become ready: %w", err)
	}

	slog.Info("model ready", "model", modelName, "url", backendURL)
	return nil
}

// monitorProcess waits for cmd to exit and clears state if it was the active
// process, so the next request triggers a fresh load.
func monitorProcess(cmd *exec.Cmd, modelName string) {
	if err := cmd.Wait(); err != nil {
		slog.Debug("model process wait finished", "model", modelName, "error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if activeCmd == cmd {
		slog.Error("model process exited", "model", modelName)
		clearStateLocked()
	}
}

// clearStateLocked resets the active-model state. Caller must hold mu.
func clearStateLocked() {
	activeCmd = nil
	currentModel = ""
	currentBackend = ""
	lastAccess.Store(0)
}

// ShutdownCurrentModel kills the active model process and cleans up.
func ShutdownCurrentModel() {
	mu.Lock()
	defer mu.Unlock()
	shutdownCurrentModelLocked()
}

// shutdownCurrentModelLocked terminates the active model process.
// Caller must hold mu.
//
// The model process holds GPU resources (VRAM, CUDA/Vulkan/Metal contexts).
// SIGKILL gives it no chance to release them, causing the next instance to
// fail with ErrorDeviceLost. We send SIGTERM to the whole process group first
// and only escalate to SIGKILL if it doesn't exit in time.
func shutdownCurrentModelLocked() {
	cmd := activeCmd
	if cmd == nil || cmd.Process == nil {
		return
	}
	slog.Info("shutting down model", "model", currentModel)

	// Wait for active requests to finish before killing the model.
	// We wait up to 5 seconds, unless the global shutdown context is cancelled.
	drainDeadline := time.Now().Add(5 * time.Second)
	for activeRequests.Load() > 0 && time.Now().Before(drainDeadline) {
		if shutdownCtx != nil && shutdownCtx.Err() != nil {
			break
		}
		mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		// Re-check if the command changed while we were unlocked.
		if activeCmd != cmd {
			return
		}
	}

	if n := activeRequests.Load(); n > 0 {
		slog.Warn("drain timeout exceeded, killing model with active requests", "model", currentModel, "active_requests", n)
	}

	pgid := cmd.Process.Pid

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		slog.Warn("process group SIGTERM failed, sending SIGTERM to process", "error", err)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			slog.Debug("process SIGTERM failed", "error", err)
		}
	}

	if !waitForGroupExit(pgid, 10*time.Second) {
		slog.Warn("model did not exit on SIGTERM, sending SIGKILL", "model", currentModel)
		killProcessGroup(cmd)
	}

	// Brief grace period for the GPU driver to release the device before
	// the next model process tries to acquire it (avoids ErrorDeviceLost).
	// We use 1 second for better reliability with RADV.
	time.Sleep(1 * time.Second)

	clearStateLocked()
}

// killProcessGroup sends SIGKILL to the whole process group and waits for exit.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if err := cmd.Process.Kill(); err != nil {
			slog.Debug("process SIGKILL failed", "error", err)
		}
	}
	waitForGroupExit(pgid, 5*time.Second)
}

// waitForGroupExit polls until no process in the process group pgid exists,
// or timeout elapses. Returns true if the group exited within the timeout.
//
// Using the process group (rather than cmd.Process alone) ensures that child
// processes spawned by the shell — e.g. the model binary started via
// sh -c "export ...; llama-server ..." — are also fully gone before we
// proceed. The shell may exit on SIGTERM before the model binary finishes
// its own cleanup; checking only the shell would return too early.
func waitForGroupExit(pgid int, timeout time.Duration) bool {
	groupGone := func() bool {
		return syscall.Kill(-pgid, syscall.Signal(0)) == syscall.ESRCH
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if shutdownCtx != nil && shutdownCtx.Err() != nil {
			return false
		}
		if groupGone() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return groupGone()
}

// waitForServerOrExit polls the model process health endpoint until it returns
// 200, the process exits, ctx is cancelled, or timeout elapses.
func waitForServerOrExit(ctx context.Context, cmd *exec.Cmd, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 2 * time.Second}

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return fmt.Errorf("shutdown requested")
		}
		if !processAlive(cmd) {
			return fmt.Errorf("process exited before becoming ready")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("shutdown requested")
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for server at %s", url)
}
