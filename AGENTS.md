# AGENTS

## Project overview

LLM Gateway — a Go single-port proxy that manages model-serving backends on demand. It starts, monitors, and kills backend processes, and proxies requests.

## File structure

```
main.go          — entry point, HTTP server setup, signal/shutdown handling
config.go        — YAML loading, validation, model config
manager.go       — process lifecycle: start, monitor (Wait goroutine), shutdown
api.go           — HTTP handlers: proxy, models list, health check
config.example.yaml — example configuration file
Makefile         — build, test, lint, docker targets
docker-compose.yml — local development with Docker
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
