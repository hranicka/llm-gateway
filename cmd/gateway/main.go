package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llm-gateway/internal/api"
	"llm-gateway/internal/config"
	"llm-gateway/internal/manager"
)

func setupLogger(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	})))
}

func main() {
	setupLogger(slog.LevelInfo)
	manager.Shutdown(context.Background())

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--install":
			manager.DoInstall()
			return
		case "--uninstall":
			manager.DoUninstall()
			return
		}
	}

	if err := config.Load(""); err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	if config.ConfigApp.Debug {
		setupLogger(slog.LevelDebug)
	}

	slog.Info("config loaded", "version", manager.Version, "models", len(config.ConfigApp.Models), "debug", config.ConfigApp.Debug)

	if !bindsLoopback(config.ConfigApp.Host) {
		if !config.AuthEnabled() {
			slog.Warn("auth is disabled while listening on a non-loopback address — " +
				"anyone on the network can run models and open web apps")
		}
		if !config.HostCheckEnabled() {
			slog.Warn("allowed_hosts is empty — the Host header is not validated, " +
				"which leaves the gateway open to DNS-rebinding from web pages")
		}
	}

	manager.StartAutoUnload(config.AutoUnloadDuration())

	mux := http.NewServeMux()
	// /app/<name> pins the browser to a web-app backend; everything else is
	// dispatched by RootHandler (gateway API routes, index page, or the
	// selected app's UI when an app cookie is present).
	mux.HandleFunc("/app/", api.AppLaunchHandler)
	mux.HandleFunc("/", api.RootHandler)

	server := &http.Server{
		Addr:    config.ConfigApp.Host,
		Handler: api.LoggingMiddleware(api.SecurityMiddleware(mux)),
		// No WriteTimeout: SSE streams and slow model loads legitimately run
		// for hours. The header timeout alone defeats slowloris.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	done := make(chan struct{})
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		slog.Info("signal received, shutting down")

		// Cancel first so waitForServerOrExit unblocks immediately,
		// releasing the model lock before we try to acquire it below.
		manager.ShutdownCancel()

		// Stop the auto-unload timer so it doesn't fire during shutdown.
		manager.StopAutoUnload()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Kill the model process first so active proxy connections fail immediately,
		// allowing the HTTP server to drain without waiting the full timeout.
		manager.ShutdownCurrentModel()

		// Then drain remaining HTTP connections.
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}

		close(done)
	}()

	slog.Info("gateway running", "addr", config.ConfigApp.Host)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen error", "error", err)
		os.Exit(1)
	}

	<-done
	slog.Info("shutdown complete")
}

// bindsLoopback reports whether the listen address is loopback-only.
// 0.0.0.0, :: and empty hosts mean all interfaces.
func bindsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}
