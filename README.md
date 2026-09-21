# LLM Gateway

An OpenAI-compatible proxy that manages model-serving backends on demand. It loads models based on the `model` field in requests, ensuring efficient VRAM usage by switching models sequentially.

## Why this gateway?

We created this lean proxy to seamlessly switch between multiple specialized models (e.g., Plan and Build models) within OpenCode. While tools like Ollama support model switching, they often abstract away the underlying engine and do not provide the granular fine-tuning and optimization capabilities of native model servers.

Running on consumer hardware typically means only one single quantized model can fit in VRAM at a time, yet modern development workflows require different models for different purposes. This gateway automatically switches them on demand while allowing you to leverage the maximum native configuration. This includes taking advantage of specific compilation targets (like ROCm, CUDA, or Metal) and low-level runtime flags (such as flash attention, precise layer offloading, and context tuning) that higher-level wrappers tend to hide.

## Features

- **On-demand loading**: Automatically starts the configured backend for requested models.
- **Predictable VRAM**: Kills the previous model before starting a new one.
- **Fast switching**: Requests for an already-loaded model are proxied immediately.
- **Sequential execution**: Proxied requests are processed strictly one at a time. Concurrent client sessions (e.g. coding agents spawning parallel subagents) queue at the gateway instead of interleaving on the single loaded backend or triggering model switches that would kill an in-flight stream.
- **OpenAI-compatible**: Supports `/v1/chat/completions` and `/v1/completions`.
- **Image generation**: Supports OpenAI-style `/v1/images/generations` for image backends (e.g. `sd-server` from stable-diffusion.cpp).
- **Web apps in the browser**: Backends with `kind: web` (sd-server, ComfyUI, …) get their full web UI proxied through the gateway — open `http://<gateway>/`, click the app, and it loads like llama-server loads for a chat request, including websockets.

## How it Works

1. **Request**: A client sends a request to `/v1/chat/completions` (or `/v1/completions`) specifying a `model`.
2. **Model Switch**:
    - If the model is already running, the request is proxied immediately.
    - If not, the gateway shuts down the current backend process (using `SIGTERM`, falling back to `SIGKILL`), waits for it to exit, and then starts the new one.
3. **Readiness**: The gateway polls the model's health endpoint (default `/health`) before proxying the request. If the endpoint returns 404, it automatically falls back to `/v1/models` (for backends like vLLM that lack a `/health` route).
4. **Monitoring**: If a model process exits unexpectedly, the gateway resets its state and will reload it on the next request.

**Web apps** (`kind: web`) work the same way, but the trigger is a browser: opening `/app/<name>` pins the app via cookie and redirects to `/`, from where every request — page, assets, API calls, websocket — is reverse-proxied to the app backend. Loading it kills whatever chat model currently holds the VRAM, exactly like a chat request would.

## Configuration

The gateway is configured via `config.yaml`. Copy `config/example.yaml` to `config.yaml` and modify it to your needs, or use one of the machine-specific configs from the `config/` directory.

### Config Details

- **`host`**: The address the gateway listens on.
- **`debug`**: Enables detailed request logging.
- **`auth_token`**: Required. Bearer token for every route except `/health`. Set it to a long random string; `""` explicitly disables auth (trusted-LAN only). API clients send `Authorization: Bearer <token>`; browsers authenticate once via `/app/<name>?token=<token>`. See [Security](#security).
- **`allowed_hosts`**: Required. Host headers the gateway answers as (case-insensitive, ports ignored when matching). Requests with any other `Host` get 403 — this defeats DNS rebinding. `[]` disables the check (warns at startup on non-loopback binds).
- **`max_body_size`**: Required. Request-body limit, e.g. `64MB` (units B/KB/MB/GB, binary). Oversized bodies get 413.
- **`auto_unload`**: Idle duration after which the active model is shut down to free VRAM (e.g. `2h`). The model is reloaded automatically on the next request. Should be equal to or greater than the longest `ready_timeout` to avoid unloading a model that is still starting up.
- **`drain_timeout`**: Maximum time to wait for active requests (e.g. streaming responses) to finish before forcing the current model to shut down during a model switch (e.g. `30s`). Increase this if long generations are being interrupted by model switches.
- **`models`**: Model configurations.
    - The key (e.g., `gemma-4-26b`) is the model name used in API requests.
    - **`kind`**: Optional. `api` (default) for OpenAI-style backends reached via `/v1/*`, or `web` for backends with a browser UI that the gateway proxies in full once selected via `/app/<name>`.
    - **`command`**: Full command to run (as a multiline string, passed via `sh -c`). Line breaks are collapsed into spaces, so the whole block runs as a single command; quote any argument that contains spaces (e.g. `--chat-template-kwargs '{"enable_thinking": true}'`).
    - **`host`**: The `host:port` address the model will listen on.
    - **`ready_timeout`**: How long the gateway waits for the backend to become ready before failing the request.
    - **`health_endpoint`**: Optional readiness probe path; defaults to `/health` (llama-server). vLLM uses `/v1/models`, web apps (sd-server, ComfyUI) use `/` (their UI page).

> **Important**: The model backend port must differ from the gateway port. If they match, the gateway's health-check would hit itself (passing instantly) and the reverse-proxy would loop. The example config uses `:1234` for the gateway and `:1235` for all backends. Ensure the ports are different to avoid this.

### OpenCode language support

The bundled [`config/opencode.json`](config/opencode.json) configures LSP servers for Go, JavaScript/TypeScript, Vue, ESLint, Bash, YAML, and PHP. This gives OpenCode compiler-aware diagnostics, symbol navigation, references, completions, and formatting context. Each server declares its executable and file extensions explicitly. The Go configuration also enables `gopls`'s `staticcheck`, `nilness`, `shadow`, and `unusedparams` analyses.

Install the servers you need if they are not already available:

```bash
go install golang.org/x/tools/gopls@latest
npm install --global typescript typescript-language-server intelephense bash-language-server yaml-language-server @vue/language-server vscode-langservers-extracted
```
For JetBrains LSP, go to IDE Settings > MCP Server > Enabler MCP Server.

### Oh My Pi on a single-slot backend

Use the same scheduling settings in every Oh My Pi profile: disable asynchronous task agents and limit task concurrency to one. This makes the main agent wait for each delegated agent. Cap only the `network-gem12` provider at one in-flight request (including streamed responses); other providers are not request-capped.

[`config/omp/profiles/gem12/agent/config.yml`](config/omp/profiles/gem12/agent/config.yml) and [`config/omp/profiles/gem12/project/config.yml`](config/omp/profiles/gem12/project/config.yml) provide the reusable configuration. All roles ride one model and vary only the reasoning effort (`default:medium`, `plan`/`slow:xhigh`, `task`/`smol`/`tiny`/`commit:low`) — switching models would reload llama-server. Tool output spills to a file above 10 KB, compaction waits until ~100K tokens, and stream timeouts are raised to 15 minutes to survive slow cold-cache prefill (following the "tuning a local coding agent" playbook).

### Oh My Pi reasoning levels

The Qwen 3.8 chat template only accepts `reasoning_effort` of `low`, `medium`, or `xhigh` (plus thinking fully off via `enable_thinking: false`). The bundled [`config/omp/profiles/gem12/agent/models.yml`](config/omp/profiles/gem12/agent/models.yml) declares exactly those levels for `qwen-3.8-27b` and routes both through `chat_template_kwargs`, which llama-server merges over its startup `--chat-template-kwargs` per request. The `medium` effort baked into the gateway's model command remains the default for non-Oh-My-Pi clients only.

## Endpoints

| Path | Method | Purpose |
|---|---|---|
| `/` | GET | Landing page (lists apps & models) — or the selected web app's UI when its cookie is present |
| `/app/<name>` | GET | Pin the browser to a `kind: web` model (sets a cookie, redirects to `/`); `/app/` unpins |
| `/v1/chat/completions` | POST | Proxy request (supports model switching) |
| `/v1/completions` | POST | Legacy proxy request (supports model switching) |
| `/v1/images/generations` | POST | Proxy image-generation request (supports model switching) |
| `/v1/models` | GET | List available **API** models (web apps are excluded — they are browser-only via `/app/<name>`) |
| `/health` | GET | Gateway health check (always answers for the gateway itself, never for an app) |

### Request handling

- **Serialization**: Proxied requests (`/v1/chat/completions`, `/v1/completions`, `/v1/images/generations`) hold a single slot: the next request is not forwarded until the previous response (including SSE streams) has finished. Clients that disconnect while queued are dropped.
- **Web apps bypass the slot**: browser traffic to a `kind: web` backend is not serialized (UI assets and polls are concurrent-safe, and an open websocket must never block API generation). Websocket connections do keep the auto-unload timer reset while open, but they do not block model switches.
- **Shared single slot**: there is still only one loaded backend at a time. A chat request switches away from a running image app (killing it mid-generation) and vice versa — keep `drain_timeout` in mind.
- **Transparency**: Request bodies are proxied untouched. The gateway does not rewrite model-specific parameters — clients are expected to send values the backend chat template supports (e.g. Qwen3 GGUF templates only accept `reasoning_effort` of `xhigh`, `medium`, or `low`; see [`config/omp/`](config/omp/) for a client setup that matches).

## Security

The gateway is built for a **trusted home LAN** behind a router firewall. There are no user accounts — one shared token gates everything, and three config fields control the posture.

- **`auth_token`** — when set, every request needs `Authorization: Bearer <token>` (curl, opencode, Open WebUI's server-side client) or the browser auth cookie. `/health` is exempt: it answers with static JSON only, which is what uptime monitors and tunnel health probes expect. Browsers authenticate once — open `http://<host>:1234/app/<name>?token=<auth_token>`; the gateway sets an `HttpOnly` cookie (plus `Secure` behind HTTPS) and redirects, and the web apps then work with no per-request headers. `/app/` clears both cookies. The `?token=` URL lands in browser history — prefer a bookmark with the token, or re-launch after clearing it. Cross-site app launches (`Sec-Fetch-Site: cross-site`) are rejected, so random web pages can't trigger model loads.
- **`allowed_hosts`** — requests whose `Host` header doesn't match the list get 403. Without this, a malicious page can use [DNS rebinding](https://en.wikipedia.org/wiki/DNS_rebinding) to rebind its own domain to the gateway's IP and gain same-origin access to everything, including the proxied web-app UIs. List the names and IPs you actually use: `localhost`, the LAN hostname (`gem12`), the LAN IP (`192.168.1.10`), and the public hostname when proxied/tunneled.
- **`max_body_size`** — request-body cap so a single huge upload can't exhaust memory. Applies to the API routes and to proxied web-app uploads alike.

With `auth_token: ""` anyone who can reach the port can run models and open the apps — a deliberate choice for the LAN, but it also means any page open in your browser can fire cross-site requests at the gateway (spend GPU, switch models). The startup log warns about both disabled checks when the bind address isn't loopback-only.

Enabling the token means every API client needs it: opencode/omp provider configs (`api_key`), and Open WebUI — its installer reads `auth_token` from `/etc/llm-gateway/config.yaml` automatically; re-run the installer after changing the token.

Other hardening in the box: the systemd units run the gateway as a regular user under `NoNewPrivileges`, `ProtectSystem=full`, `PrivateTmp`, an empty capability set, and a device allowlist; the installer writes the config and Open WebUI's secret key with `0600` permissions (secrets live in a 0600 `EnvironmentFile`, never in the world-readable unit file); the installer scripts pin and checksum the binaries they download. Deliberate non-goals: per-user identity, rate limiting, IP allowlists (they break behind proxies and introduce `X-Forwarded-For` trust — which is why that header is never used for auth decisions), and built-in TLS.

### Behind a reverse proxy or tunnel

The gateway speaks plain HTTP; TLS terminates at the proxy. For hostname or tunnel exposure: set a strong `auth_token`, add the public hostname to `allowed_hosts`, and proxy as usual — keep the `Host` header, don't buffer SSE, and raise read timeouts for long model loads:

Caddy:

```
gem12.example.com {
	reverse_proxy 127.0.0.1:1234
}
```

nginx:

```nginx
location / {
	proxy_pass http://127.0.0.1:1234;
	proxy_set_header Host $host;
	proxy_http_version 1.1;
	proxy_set_header Upgrade $http_upgrade;    # websockets
	proxy_set_header Connection "upgrade";
	proxy_set_header X-Forwarded-Proto https;  # enables Secure cookies
	proxy_buffering off;                       # SSE streaming
	proxy_read_timeout 2h;                     # long model loads
}
```

Cloudflare Tunnel and Tailscale Funnel work the same way — add the public hostname (`machine.tailnet.ts.net`, `gem12.example.com`, …) to `allowed_hosts`. Behind a tunnel the token is effectively mandatory: the tunnel is the public internet.

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
| [`llama-server`](https://github.com/ggml-org/llama.cpp/tree/master/examples/server) | GGUF chat models, ROCm (AMD iGPU), MTP speculative decoding | [`scripts/install-llama.sh`](scripts/install-llama.sh) |
| [`vllm serve`](https://docs.vllm.ai/en/latest/) | True NVFP4/BF16 safetensors, fp8 KV cache up to 131K ctx on NVIDIA | [`scripts/install-vllm-globally.sh`](scripts/install-vllm-globally.sh) |
| [`sd-server`](https://github.com/leejet/stable-diffusion.cpp) (stable-diffusion.cpp) | Image generation/editing from GGUF diffusion models, embedded web UI, OpenAI-images API | [`scripts/install-qwen-image-sdcpp.sh`](scripts/install-qwen-image-sdcpp.sh) |
| [`ComfyUI`](https://github.com/comfyanonymous/ComfyUI) | Graph-based image workflows, custom nodes | [`scripts/install-comfyui.sh`](scripts/install-comfyui.sh) |

**Choosing a backend:**

- **GGUF (llama-server):** Faster cold-start loads (~10 s), lower VRAM overhead during loading, supports ROCm/Vulkan/iGPU, and has built-in speculative decoding via MTP (`--spec-draft-hf`). Models are downloaded automatically from HuggingFace by `--hf`/`-hf` flag. Uses quantized formats (Q4_K_M, Q8_0, etc.) that fit on smaller GPU VRAM.
- **vLLM:** True NVFP4 and BF16 precision for safetensors models — no GGUF conversion quality loss. Ships its own CUDA runtime so it works alongside llama.cpp without conflicts. Supports fp8 KV cache with 131K context length even on 16 GB VRAM. Requires NVIDIA GPU with Open Kernel Modules and ≥ 8 GB VRAM for ~12 B parameter models. Cold-start is slower (~60–90 s loading safetensors into VRAM).
- **sd-server (images):** A single llama.cpp-style C++ binary — no Python, no torch. Serves GGUF diffusion models (Qwen-Image-2.1, and more), loads in seconds, ships an embedded web UI, and exposes both OpenAI-compatible (`/v1/images/generations`) and A1111-style (`/sdapi/v1/`) APIs.
- **ComfyUI:** The heavyweight option — Python + torch, graph workflows, huge node ecosystem. Use it when you need custom workflows (multi-step pipelines, ControlNet, LoRAs); use sd-server for simple prompt→image with GGUF quantization. For GGUF in ComfyUI install the [`leejet/ComfyUI-GGUF`](https://github.com/leejet/ComfyUI-GGUF) custom node (successor of the unmaintained city96 fork).

> **Running both backends concurrently:** The gateway creates one backend process per model, each listening on its own `host:port`. Different models can use different backends (e.g. llama-server for some models, vllm for others) — they simply share whatever ports you assign in their respective configs.

### Installation notes for vLLM

The vLLM installer (`scripts/install-vllm-globally.sh`) creates a uv-managed venv at `/opt/vllm/venv` with torch + vLLM (pinned to CUDA 13.0 wheels for Blackwell support) and symlinks `vllm` into `/usr/local/bin`. PyTorch ships its own CUDA runtime (cu130) — it does not conflict with the system `cuda-toolkit-12`. The `nvidia-cuda-nvcc` package is also installed: NVFP4 models on Blackwell (sm_120) require FlashInfer to JIT-compile FP4 CUTLASS kernels at first run, which needs `nvcc`. The wrapper at `/usr/local/bin/vllm` sets `CUDA_HOME` and `PATH` so FlashInfer finds nvcc automatically. vLLM auto-detects the compressed-tensors quantization format from the model's `config.json` so no explicit `--quantization` flag is required — only `--kv-cache-dtype fp8` is passed explicitly to enable 8-bit KV cache for long context (131K on 16 GB VRAM). vllm serve exposes OpenAI-compatible API endpoints (`/v1/chat/completions`, `/v1/models`) which the gateway proxies transparently.

> **Model naming:** Use `--served-model-name <gateway-model-name>` in the vLLM command so the `model` field in API requests matches the gateway's model key. This is the vLLM equivalent of llama-server's `--alias` flag. Without it, clients must send the full HuggingFace repo name (e.g. `unsloth/gemma-4-12b-it-NVFP4`) as the model field.

## Image generation & editing: Qwen-Image-2.1 on 16 GB VRAM

llama.cpp cannot run diffusion/image models — but [stable-diffusion.cpp](https://github.com/leejet/stable-diffusion.cpp) can, and its `sd-server` is the same kind of lean native binary the gateway is built around. [Qwen-Image-2.1](https://huggingface.co/Qwen/Qwen-Image-2.1) is a **7B** DiT (its 2025 predecessor was 20B), does both text-to-image **and** image editing (up to 10 reference images), and natively outputs RGBA.

Weights on the gem12 (8845HS + 32 GB RAM + RTX 5060 Ti 16 GB eGPU):

| Component | File | Size |
|---|---|---|
| Diffusion model (GGUF Q4_0) | [`leejet/Qwen-Image-2.1-GGUF`](https://huggingface.co/leejet/Qwen-Image-2.1-GGUF) | 4.2 GB |
| Text encoder Qwen3-VL-8B (Q4_K_M GGUF) | [`Qwen/Qwen3-VL-8B-Instruct-GGUF`](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct-GGUF) | ~4.9 GB |
| Vision projector mmproj (F16, editing) | [`Qwen/Qwen3-VL-8B-Instruct-GGUF`](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct-GGUF) | ~2.5 GB |
| VAE (bf16) | `Comfy-Org/Qwen-Image-2.1` | ~0.4 GB |
| **Total** | | **~13.7 GB** |

The full stack — diffusion + text encoder + mmproj + VAE ≈ 12 GB — fits entirely in the 16 GB VRAM with ~4 GB headroom, so the bundled config keeps every weight GPU-resident: at the defaults (**1024×1024, 40 steps, euler sampling** with guidance per the config comment) generation runs in well under a minute on the RTX 5060 Ti. The sd.cpp guide's quality reference is `--cfg-scale 6.0`; lowering it to `1.0` (as the bundled config does) skips the guidance pass — roughly 2× faster per step, at the cost of prompt adherence and small-text rendering. `DIFFUSION_QUANT=Q6_K` (6.0 GB) upgrades to the higher-fidelity quant if Q4_0 ever looks soft. If 2048×2048 or many reference images ever hit CUDA OOM, add `--offload-to-cpu` to the command: the weights then stream from system RAM, roughly 5× slower per step but OOM-proof.

Other diffusion quants: `DIFFUSION_QUANT=Q8_0` for maximum quality (7.7 GB — with that, `--offload-to-cpu` becomes necessary), or down to Q5_0/Q4_0/Q2_K for even lighter footprints. `DIFFUSION_REPO=abenzerps/Qwen-Image-2.1-Uncensored-GGUF` switches to a community re-quant of the same weights (adds Q4_K_M/Q5_K_M).

### Install & use

```bash
sudo ./scripts/install-qwen-image-sdcpp.sh   # binary + ~14 GB of models into /opt/sdcpp
```

Then uncomment the `qwen-image-2.1` entry in the gateway config (see [`config/gem12gpu.yaml`](config/gem12gpu.yaml)) and restart the gateway.

> The browser UI is compiled **into** the sd-server binary (`SD_SERVER_BUILD_FRONTEND=ON`, needs Node ≥ 20 + pnpm ≥ 10, installed automatically). If you ever see a plain *"Stable Diffusion Server is running"* text instead of the UI, the binary was built without the frontend — re-run the installer and it will rebuild with it.

- **Browser**: open `http://<gem12>:1234/` — the landing page lists web apps and API models; click **qwen-image-2.1**. The gateway starts sd-server (first load takes a few seconds) and proxies its embedded web UI, including websockets.
- **API**: `POST /v1/images/generations` with `{"model": "qwen-image-2.1", "prompt": "..."}` — same on-demand loading as chat models.
- **Image editing**: enabled by default — the config ships with `--llm_vision` (mmproj), so reference images work out of the box, in the UI or via the API.

> **Note:** Make sure the sd-server port differs from the gateway port (`--listen-port 1235` vs gateway `1234` in the bundled configs) — the loop protection applies to image backends too.

### ComfyUI for advanced workflows (composition, masked 1:1 edits)

[`scripts/install-comfyui.sh`](scripts/install-comfyui.sh) installs ComfyUI as another gateway-managed web app (`comfyui`, port 8188, bound to loopback — no extra firewall rules). It reuses the Qwen-Image-2.1 models already downloaded for sd-server via symlinks, and downloads the **[NVFP4 package](https://huggingface.co/BennyDaBall/Qwen-Image-2.1-NVFP4)** — attention/MLP matrices in native Blackwell FP4 (sensitive layers BF16), loaded by core ComfyUI nodes and noticeably faster than GGUF on the RTX 5060 Ti. Four ready-made workflows (Text-to-Image, Image Editing, Transparent RGBA, 2K Typography) come preloaded in the UI's Workflows panel. The gateway starts/kills ComfyUI exactly like sd-server, so the single VRAM slot stays automatic.

```bash
sudo ./scripts/install-comfyui.sh   # after install-qwen-image-sdcpp.sh
```

Composition = the Editing workflow with several reference images (up to 10); "remove this but keep the rest 1:1" = right-click the image → *Open in MaskEditor*, paint the region to regenerate — everything outside the mask stays pixel-identical. GGUF remains available as the fallback loader ( leejet/ComfyUI-GGUF node + the symlinked Q4_0/Q6_K files).

### Caveats

- One backend at a time: opening the image app kills the loaded chat model, and a coding-agent request kills the image app — even mid-generation (`drain_timeout` bounds the wait for in-flight requests; browser websockets do not block switches).
- If the tab sits idle past `auto_unload`, sd-server is unloaded and the app needs a refresh (an open websocket counts as activity, so a live tab keeps it loaded).
- ComfyUI is a drop-in alternative for graph workflows: install it with [`scripts/install-comfyui.sh`](scripts/install-comfyui.sh) (uses the leejet/ComfyUI-GGUF node plus the same GGUF/encoder/VAE files).
- Qwen-Image-2.1 weights are released under the Qwen Research License — non-commercial use only.

## ChatGPT-style UI: Open WebUI (chat + image generation)

[`scripts/install-open-webui.sh`](scripts/install-open-webui.sh) installs [Open WebUI](https://docs.openwebui.com) as its own always-on systemd service (port 8080, uv-managed venv at `/opt/open-webui`, chats/accounts persisted in `/opt/open-webui/data`). The gateway knows nothing about it — Open WebUI is a pure API client of the gateway:

- **Chat**: the model picker lists every gateway model (`qwen-3.8-27b`, `gemma-4-26b`, …); picking one loads it on demand exactly like opencode does, with streaming and full conversation history.
- **Images**: in a chat, enable the image toggle in the composer (or use `/image <prompt>`) and send with a **regular chat model selected** — never pick `qwen-image-2.1` in the model picker; it is not a chat model (and since it's a web app, the gateway doesn't even list it). The image is generated through the gateway's `/v1/images/generations` with `qwen-image-2.1` — model and endpoint are preconfigured via environment variables. Set the resolution in *Admin Settings → Images* (width/height in multiples of 32, e.g. 1024×1024 or 2048×2048). Request timeouts are also preconfigured (no total limit, no stream idle cap), so a model still loading never kills a pending chat or image request.
- **Editing** (reference images) stays in sd-server's own UI at `http://<host>:1234/app/qwen-image-2.1`.

```bash
sudo ./scripts/install-open-webui.sh          # OPEN_WEBUI_PORT=8081 to override the port
```

Open `http://<host>:8080`; the first account created becomes admin. Mixed chat-and-image conversations work, but remember there is still one backend slot: each switch between a chat model and sd-server reloads a model (~10–30 s). The same caveat applies as with any client — an incoming image request kills a loaded chat model mid-stream only after `drain_timeout`.

Manage with `systemctl {status|restart|stop} open-webui`, logs via `journalctl -u open-webui -f`; re-run the installer to upgrade.

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
