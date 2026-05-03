package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var (
	// mu guards activeCmd, currentModel, currentBackend.
	// RLock is taken on the fast path (model already loaded);
	// Lock is held for the duration of a model switch.
	mu             sync.RWMutex
	activeCmd      *exec.Cmd
	currentModel   string
	currentBackend string
)

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
		slog.Log(context.Background(), level, "backend", append(append([]any{}, attrs...), "line", scanner.Text())...)
	}
}

// switchModel ensures modelName is loaded and returns its backend URL.
//
// Fast path: if the requested model is already loaded and alive, return its
// URL immediately under a read lock. Otherwise acquire the write lock, shut
// down any running model, and start the new one. Concurrent requests for the
// same model naturally coalesce because they all observe the loaded state on
// the fast path once the first switch completes.
func switchModel(modelName string) (string, error) {
	mu.RLock()
	if currentModel == modelName && processAlive(activeCmd) {
		backend := currentBackend
		mu.RUnlock()
		return backend, nil
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()

	// Re-check after upgrading to the write lock.
	if currentModel == modelName && processAlive(activeCmd) {
		return currentBackend, nil
	}
	if err := startModelLocked(modelName); err != nil {
		return "", err
	}
	return currentBackend, nil
}

// startModelLocked shuts down any current model and starts modelName.
// Caller must hold mu (write lock).
func startModelLocked(modelName string) error {
	cmdStr, backendURL, err := buildCommand(modelName)
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

	if err := waitForServerOrExit(shutdownCtx, cmd, backendURL+"/health", modelReadyTimeout(modelName)); err != nil {
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
	_ = cmd.Wait()
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
}

// shutdownCurrentModel kills the active model process and cleans up.
func shutdownCurrentModel() {
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
	pgid := cmd.Process.Pid

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		slog.Warn("process group SIGTERM failed, sending SIGTERM to process", "error", err)
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	if !waitWithTimeout(cmd, 10*time.Second) {
		slog.Warn("model did not exit on SIGTERM, sending SIGKILL", "model", currentModel)
		killProcessGroup(cmd)
		if !waitWithTimeout(cmd, 10*time.Second) {
			slog.Error("model process group did not exit after SIGKILL, process may leak", "model", currentModel, "pid", pgid)
		}
	}

	// Brief grace period for the GPU driver to release the device before
	// the next model process tries to acquire it (avoids ErrorDeviceLost).
	time.Sleep(500 * time.Millisecond)

	clearStateLocked()
}

// killProcessGroup sends SIGKILL to the whole process group, falling back to
// the main process if the group kill fails.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// waitWithTimeout polls processAlive until cmd exits or timeout elapses.
// Returns true if the process exited within the timeout.
func waitWithTimeout(cmd *exec.Cmd, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(cmd) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(cmd)
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
