package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

	manager.StartAutoUnload(config.AutoUnloadDuration())

	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.HealthHandler)
	mux.HandleFunc("/v1/models", api.ModelsHandler)
	mux.HandleFunc("/v1/chat/completions", api.ProxyHandler)
	mux.HandleFunc("/v1/completions", api.ProxyHandler)
	mux.HandleFunc("/", api.NotFoundHandler)

	server := &http.Server{
		Addr:    config.ConfigApp.Host,
		Handler: api.LoggingMiddleware(mux),
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
