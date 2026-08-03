# LLM Gateway

An OpenAI-compatible proxy that manages model-serving backends on demand. It loads models based on the `model` field in requests, ensuring efficient VRAM usage by switching models sequentially.

## Why this gateway?

We created this lean proxy to seamlessly switch between multiple specialized models (e.g., Plan and Build models) within OpenCode. While tools like Ollama support model switching, they often abstract away the underlying engine and do not provide the granular fine-tuning and optimization capabilities of native model servers.

Running on consumer hardware typically means only one single quantized model can fit in VRAM at a time, yet modern development workflows require different models for different purposes. This gateway automatically switches them on demand while allowing you to leverage the maximum native configuration. This includes taking advantage of specific compilation targets (like ROCm, CUDA, or Metal) and low-level runtime flags (such as flash attention, precise layer offloading, and context tuning) that higher-level wrappers tend to hide.

## Features

- **On-demand loading**: Automatically starts the configured backend for requested models.
- **Predictable VRAM**: Kills the previous model before starting a new one.
- **Fast switching**: Requests for an already-loaded model are proxied immediately.
- **OpenAI-compatible**: Supports `/v1/chat/completions` and `/v1/completions`.

## How it Works

1. **Request**: A client sends a request to `/v1/chat/completions` (or `/v1/completions`) specifying a `model`.
2. **Model Switch**:
    - If the model is already running, the request is proxied immediately.
    - If not, the gateway shuts down the current backend process (using `SIGTERM`, falling back to `SIGKILL`), waits for it to exit, and then starts the new one.
3. **Readiness**: The gateway polls the model's health endpoint (default `/health`) before proxying the request. If the endpoint returns 404, it automatically falls back to `/v1/models` (for backends like vLLM that lack a `/health` route).
4. **Monitoring**: If a model process exits unexpectedly, the gateway resets its state and will reload it on the next request.

## Configuration

The gateway is configured via `config.yaml`. Copy `config/example.yaml` to `config.yaml` and modify it to your needs, or use one of the machine-specific configs from the `config/` directory.

### Config Details

- **`host`**: The address the gateway listens on.
- **`debug`**: Enables detailed request logging.
- **`auto_unload`**: Idle duration after which the active model is shut down to free VRAM (e.g. `2h`). The model is reloaded automatically on the next request. Should be equal to or greater than the longest `ready_timeout` to avoid unloading a model that is still starting up.
- **`drain_timeout`**: Maximum time to wait for active requests (e.g. streaming responses) to finish before forcing the current model to shut down during a model switch (e.g. `30s`). Increase this if long generations are being interrupted by model switches.
- **`models`**: Model configurations.
    - The key (e.g., `gemma-4-26b`) is the model name used in API requests.
    - **`command`**: Full command to run (as a multiline string, passed via `sh -c`). Line breaks are collapsed into spaces, so the whole block runs as a single command; quote any argument that contains spaces (e.g. `--chat-template-kwargs '{"enable_thinking": true}'`).
    - **`host`**: The `host:port` address the model will listen on.

> **Important**: The model backend port must differ from the gateway port. If they match, the gateway's health-check would hit itself (passing instantly) and the reverse-proxy would loop. The example config uses `:1234` for the gateway and `:1235` for all backends. Ensure the ports are different to avoid this.

### OpenCode language support

The bundled [`config/opencode.json`](config/opencode.json) configures OpenCode to use standalone language servers for Go, JavaScript/TypeScript, and PHP. This gives OpenCode compiler-aware diagnostics, symbol navigation, references, completions, and formatting context. The Go configuration enables `gopls`'s `staticcheck`, `nilness`, `shadow`, and `unusedparams` analyses.

Install the servers you need if they are not already available:

```bash
go install golang.org/x/tools/gopls@latest
npm install --global typescript typescript-language-server intelephense
```
For JetBrains LSP, go to IDE Settings > MCP Server > Enabler MCP Server.

## Endpoints

| Path | Method | Purpose |
|---|---|---|
| `/v1/chat/completions` | POST | Proxy request (supports model switching) |
| `/v1/completions` | POST | Legacy proxy request (supports model switching) |
| `/v1/models` | GET | List available configured models |
| `/health` | GET | Gateway health check |

## Installation

### Pre-built binaries

Download the latest release zip from the [GitHub Releases page](https://github.com/hranicka/llm-gateway/releases). It contains the `llm-gateway` binary alongside example configs.

```bash
wget https://github.com/hranicka/llm-gateway/releases/latest/download/llm-gateway.zip
unzip llm-gateway.zip
chmod +x llm-gateway
./llm-gateway
```

### Build from source

The gateway itself is self-contained. You only need a compatible backend (e.g., [`llama-server`](https://github.com/ggerganov/llama.cpp/tree/master/examples/server)) configured in `config.yaml` to proxy requests to.

> **Note**: The gateway spawns whatever process you put in the `command` field — it is **backend-agnostic**. See below for supported backends. For a basic template see [`config/example.yaml`](config/example.yaml), or the ready-to-use configs in [`config/`](config/).

### Supported Backends

| Backend | Best for | Install |
|---|---|---|
| [`llama-server`](https://github.com/ggml-org/llama.cpp/tree/master/examples/server) | GGUF models, ROCm (AMD iGPU), MTP speculative decoding | [`scripts/install-llama.sh`](scripts/install-llama.sh) |
| [`vllm serve`](https://docs.vllm.ai/en/latest/) | True NVFP4/BF16 safetensors, fp8 KV cache up to 131K ctx on NVIDIA | [`scripts/install-vllm-globally.sh`](scripts/install-vllm-globally.sh) |

**Choosing a backend:**

- **GGUF (llama-server):** Faster cold-start loads (~10 s), lower VRAM overhead during loading, supports ROCm/Vulkan/iGPU, and has built-in speculative decoding via MTP (`--spec-draft-hf`). Models are downloaded automatically from HuggingFace by `--hf`/`-hf` flag. Uses quantized formats (Q4_K_M, Q8_0, etc.) that fit on smaller GPU VRAM.
- **vLLM:** True NVFP4 and BF16 precision for safetensors models — no GGUF conversion quality loss. Ships its own CUDA runtime so it works alongside llama.cpp without conflicts. Supports fp8 KV cache with 131K context length even on 16 GB VRAM. Requires NVIDIA GPU with Open Kernel Modules and ≥ 8 GB VRAM for ~12 B parameter models. Cold-start is slower (~60–90 s loading safetensors into VRAM).

> **Running both backends concurrently:** The gateway creates one backend process per model, each listening on its own `host:port`. Different models can use different backends (e.g. llama-server for some models, vllm for others) — they simply share whatever ports you assign in their respective configs.

### Installation notes for vLLM

The vLLM installer (`scripts/install-vllm-globally.sh`) creates a uv-managed venv at `/opt/vllm/venv` with torch + vLLM (pinned to CUDA 13.0 wheels for Blackwell support) and symlinks `vllm` into `/usr/local/bin`. PyTorch ships its own CUDA runtime (cu130) — it does not conflict with the system `cuda-toolkit-12`. The `nvidia-cuda-nvcc` package is also installed: NVFP4 models on Blackwell (sm_120) require FlashInfer to JIT-compile FP4 CUTLASS kernels at first run, which needs `nvcc`. The wrapper at `/usr/local/bin/vllm` sets `CUDA_HOME` and `PATH` so FlashInfer finds nvcc automatically. vLLM auto-detects the compressed-tensors quantization format from the model's `config.json` so no explicit `--quantization` flag is required — only `--kv-cache-dtype fp8` is passed explicitly to enable 8-bit KV cache for long context (131K on 16 GB VRAM). vllm serve exposes OpenAI-compatible API endpoints (`/v1/chat/completions`, `/v1/models`) which the gateway proxies transparently.

> **Model naming:** Use `--served-model-name <gateway-model-name>` in the vLLM command so the `model` field in API requests matches the gateway's model key. This is the vLLM equivalent of llama-server's `--alias` flag. Without it, clients must send the full HuggingFace repo name (e.g. `unsloth/gemma-4-12b-it-NVFP4`) as the model field.

#### Using Makefile (recommended)

```bash
git clone <repo-url>
cd llm-gateway
cp config/example.yaml config.yaml
make build
./llm-gateway
```

#### Using Go directly

```bash
git clone <repo-url>
cd llm-gateway
cp config/example.yaml config.yaml
go build -o llm-gateway ./cmd/gateway
./llm-gateway
```

### Using Docker

The gateway runs in a Go Alpine container. Your `config.yaml` is mounted into the container so you can edit it on the host.

```bash
git clone <repo-url>
cd llm-gateway
cp config/example.yaml config.yaml
make docker
```

The gateway will be available at `http://localhost:1234`.

## Build & Run

```bash
make build
./llm-gateway
```

## Testing & Linting

```bash
make test     # Run vet, lint, and tests
make all      # Run vet, lint, tests, and build
make tools    # Install dev tools (golangci-lint)
```

## Graceful Shutdown

Sending `SIGINT` or `SIGTERM` will trigger a graceful shutdown: the active model process is terminated, and the gateway stops accepting new connections.

## Autostart with systemd

The gateway includes built-in `--install` and `--uninstall` commands that set up a systemd service, install the binary to `/usr/local/bin`, and prompt which config to place at `/etc/llm-gateway/config.yaml`.

The service is configured to run as the user who invoked `sudo` (detected via `$SUDO_USER`). This ensures the backend process (e.g., `llama-server`) can find tools installed in `~/.local/bin` and writes model cache to the correct `~/.cache/huggingface` directory.

> **Note:** If your home directory is encrypted (e.g. ecryptfs or an unlocked-at-login LUKS volume), the model cache won't be accessible at boot before you log in. In that case, set `HF_HOME` to an unencrypted path in the backend `command`, for example prepend `HF_HOME=/var/cache/huggingface llama-server …`.

### Install

Run from the directory where the zip was extracted (the binary and `config/` directory must be in the same location):

```bash
sudo ./llm-gateway --install
```

The installer will prompt for:

1. **Config** — pick one of the bundled `config/*.yaml` files (or keep an existing one).
2. **Service type** — choose the systemd unit that matches your GPU setup:
   - `[1] Generic / Vulkan` — iGPU only (Radeon 780M, Intel iGPU). No NVIDIA ordering.
   - `[2] CUDA / eGPU` — NVIDIA GPU (RTX 5060 Ti eGPU via OCuLink/Thunderbolt, or any NVIDIA dGPU). Starts after `nvidia-persistenced.service`, loads `nvidia-uvm`, then runs a full `nvidia-smi` query before `llm-gateway` starts. This matches the manual recovery sequence that warms the GPU on cold boot. If NVIDIA is not ready yet, the unit fails and systemd retries instead of starting the gateway in a CPU-fallback state.

### Remove

```bash
sudo ./llm-gateway --uninstall
```

### Manage the service

```bash
# Start / stop / restart
sudo systemctl start/stop/restart llm-gateway

# Check status
sudo systemctl status llm-gateway

# View logs
sudo journalctl -u llm-gateway -f
```

The service has `Restart=on-failure` with a 10-second delay, so it will automatically recover from crashes.
