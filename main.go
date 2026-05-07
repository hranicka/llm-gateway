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
)

var (
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
)

func setupLogger(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	})))
}

func main() {
	setupLogger(slog.LevelInfo)
	shutdownCtx, shutdownCancel = context.WithCancel(context.Background())

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--install":
			doInstall()
			return
		case "--uninstall":
			doUninstall()
			return
		}
	}

	if err := loadConfig("config.yaml"); err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	if config.Debug {
		setupLogger(slog.LevelDebug)
	}

	slog.Info("config loaded", "version", version, "models", len(config.Models), "debug", config.Debug)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/v1/chat/completions", proxyHandler)
	mux.HandleFunc("/v1/completions", proxyHandler)
	mux.HandleFunc("/", notFoundHandler)

	server := &http.Server{
		Addr:    config.Host,
		Handler: loggingMiddleware(mux),
	}

	done := make(chan struct{})
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		slog.Info("signal received, shutting down")

		// Cancel first so waitForServerOrExit unblocks immediately,
		// releasing the model lock before we try to acquire it below.
		shutdownCancel()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Kill the model process first so active proxy connections fail immediately,
		// allowing the HTTP server to drain without waiting the full timeout.
		shutdownCurrentModel()

		// Then drain remaining HTTP connections.
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}

		close(done)
	}()

	slog.Info("gateway running", "addr", config.Host)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listen error", "error", err)
		os.Exit(1)
	}

	<-done
	slog.Info("shutdown complete")
}
