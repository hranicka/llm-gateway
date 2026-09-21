# AGENTS

## Project overview

LLM Gateway — a Go single-port proxy that manages model-serving backends on demand. It starts, monitors, and kills backend processes, and proxies requests.

## File structure

```
cmd/gateway/main.go    — entry point, HTTP server setup, signal/shutdown handling
tools.go               — external tool dependencies (golangci-lint)
internal/config/config.go — YAML loading, validation, model config (kind: api|web)
internal/manager/manager.go — process lifecycle: start, monitor (Wait goroutine), shutdown
internal/manager/install.go — install/uninstall commands
internal/api/api.go   — RootHandler dispatcher: gateway API routes, index page, web-app cookie proxy; /app/<name> launcher
config/example.yaml   — example configuration file
config/systemd.service — systemd service unit file
compose.yml           — local development with Docker
Makefile              — build, test, lint targets
scripts/install-qwen-image-sdcpp.sh — sd-server + Qwen-Image-2.1 models installer
scripts/install-open-webui.sh — Open WebUI front-end (own systemd service, gateway API client)
.github/workflows/ci.yml — CI: lint, vet, test, build
.github/workflows/release.yml — release: lint, test, build, create GitHub release
```

## How to build & run

```bash
make build
./llm-gateway
```

### Testing & Linting

```bash
make test     # Run vet, lint, and tests
make all      # Run vet, lint, tests, and build
make tools    # Install dev tools (golangci-lint)
```

### Docker development

```bash
make docker   # Run in docker-compose
make docker-down   # Stop container
```

## Rules

- Check `README.md` before making changes — it tracks the public contract.
- Update `README.md` after any change to the config format, endpoints, or behavior.
- Keep all fields required in config — no defaults.
- All logging uses `slog` with `AddSource: true`.
- Shared mutable state (`currentModel`, `activeCmd`, `currentBackend`) is protected by `mu` (`sync.RWMutex`). Never access these without holding the lock.
- The app is configurable via `config.yaml` (copy from `config.example.yaml`).
- Map iteration is non-deterministic: always sort keys before iterating (`buildCommand`, `modelsHandler`).
- Prefer simplicity over complexity. Remove validation that doesn't add value.
