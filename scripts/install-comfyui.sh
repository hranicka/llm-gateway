#!/usr/bin/env bash
# Usage: sudo ./scripts/install-comfyui.sh
#
# Installs ComfyUI — the node-based editor for the "beyond simple prompts"
# cases: composing multiple photos, masked inpainting that keeps untouched
# pixels 1:1 original, ControlNet, LoRAs, upscalers — as a GATEWAY-MANAGED
# web app (kind: web) on 127.0.0.1:8188. The gateway starts/kills it like
# sd-server, so the single VRAM slot is respected automatically.
#
# Reuses the Qwen-Image-2.1 models already downloaded by
# install-qwen-image-sdcpp.sh via symlinks (no extra disk for weights).
# Upgrade: re-run the script (git pull + dependency refresh).

set -euo pipefail

INSTALL_DIR="/opt/comfyui"
PYVER="3.12"
COMFY_REPO="https://github.com/comfyanonymous/ComfyUI.git"
GGUF_NODE_REPO="https://github.com/leejet/ComfyUI-GGUF.git"
SDCPP_MODELS="/opt/sdcpp/models"
PORT="${COMFYUI_PORT:-8188}"

if [ "$(id -u)" -ne 0 ]; then
	echo "ERROR: this script must be run as root."
	echo "       Usage: sudo $(basename "$0")"
	exit 1
fi

RUN_USER="${SUDO_USER:-$(id -nu 1000 2>/dev/null || echo root)}"
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"

echo "======================================================"
echo " ComfyUI — graph workflows on top of the Qwen models"
echo "------------------------------------------------------"
echo " install: ${INSTALL_DIR} (venv + ComfyUI + GGUF node)"
echo " models:  symlinked from ${SDCPP_MODELS} (no extra disk)"
echo " runs as: ${RUN_USER}, managed by llm-gateway (kind: web)"
echo " UI:      http://<host>:1234/ → click 'comfyui'"
echo "======================================================"
echo

# ── 0. Prerequisites ──────────────────────────────────────────────────────────
if ! nvidia-smi &>/dev/null; then
	echo "ERROR: no NVIDIA driver (nvidia-smi)."
	exit 1
fi
if ! ls "${SDCPP_MODELS}"/qwen*image*2.1-*.gguf >/dev/null 2>&1; then
	echo "ERROR: no Qwen-Image-2.1 models in ${SDCPP_MODELS}."
	echo "       Run ./scripts/install-qwen-image-sdcpp.sh first."
	exit 1
fi
for dep in git curl; do
	command -v "$dep" &>/dev/null || { echo "ERROR: '$dep' is required."; exit 1; }
done

mkdir -p "${INSTALL_DIR}/python"

# ── 1. uv + Python (kept inside ${INSTALL_DIR} — see install-open-webui.sh) ──
if ! command -v uv &>/dev/null; then
	echo "[1/5] Installing uv..."
	curl -LsSf https://astral.sh/uv/install.sh | sh -s --
	export UV_NO_MODIFY_PATH=1
	export PATH="$HOME/.local/bin:$PATH"
else
	echo "[1/5] uv found: $(uv --version)"
fi
export UV_PYTHON_INSTALL_DIR="${INSTALL_DIR}/python"
if ! uv python list 2>/dev/null | grep -q "$PYVER"; then
	uv python install "$PYVER"
fi
if [ -L "${INSTALL_DIR}/venv/bin/python3" ] && \
	! case "$(readlink -f "${INSTALL_DIR}/venv/bin/python3")" in "${INSTALL_DIR}"/*) true;; *) false;; esac; then
	echo "  Recreating venv that links outside ${INSTALL_DIR}."
	rm -rf "${INSTALL_DIR:?}/venv"
fi
if [ ! -e "${INSTALL_DIR}/venv/bin/activate" ]; then
	uv venv "${INSTALL_DIR}/venv" --python "$PYVER"
fi

# ── 2. ComfyUI + GGUF custom node ─────────────────────────────────────────────
echo "[2/5] Fetching ComfyUI + leejet/ComfyUI-GGUF custom node..."
if [ ! -d "${INSTALL_DIR}/ComfyUI/.git" ]; then
	git clone --depth 1 "$COMFY_REPO" "${INSTALL_DIR}/ComfyUI"
else
	git -C "${INSTALL_DIR}/ComfyUI" pull --ff-only
fi
if [ ! -d "${INSTALL_DIR}/ComfyUI/custom_nodes/ComfyUI-GGUF/.git" ]; then
	git clone --depth 1 "$GGUF_NODE_REPO" "${INSTALL_DIR}/ComfyUI/custom_nodes/ComfyUI-GGUF"
else
	git -C "${INSTALL_DIR}/ComfyUI/custom_nodes/ComfyUI-GGUF" pull --ff-only
fi

# ── 3. Dependencies (CUDA torch for Blackwell via uv's torch backend) ────────
echo "[3/5] Installing dependencies (CUDA torch, ~5 GB)..."
source "${INSTALL_DIR}/venv/bin/activate"
uv pip install -r "${INSTALL_DIR}/ComfyUI/requirements.txt" --torch-backend=cu130
NODE_REQ="${INSTALL_DIR}/ComfyUI/custom_nodes/ComfyUI-GGUF/requirements.txt"
if [ -f "$NODE_REQ" ]; then
	uv pip install -r "$NODE_REQ"
else
	uv pip install gguf
fi

# ── 4. Model symlinks (shared with sd-server — no extra disk) ────────────────
echo "[4/5] Linking Qwen-Image-2.1 models into ComfyUI..."
mkdir -p "${INSTALL_DIR}/ComfyUI/models/diffusion_models" \
	"${INSTALL_DIR}/ComfyUI/models/text_encoders" \
	"${INSTALL_DIR}/ComfyUI/models/vae"
for f in "${SDCPP_MODELS}"/qwen*image*2.1-*.gguf; do
	[ -e "$f" ] || continue
	ln -sfn "$f" "${INSTALL_DIR}/ComfyUI/models/diffusion_models/$(basename "$f")"
	echo "  diffusion_models/$(basename "$f")"
done
for f in "${SDCPP_MODELS}"/Qwen3VL*.gguf; do
	[ -e "$f" ] || continue
	ln -sfn "$f" "${INSTALL_DIR}/ComfyUI/models/text_encoders/$(basename "$f")"
	echo "  text_encoders/$(basename "$f")"
done
for f in "${SDCPP_MODELS}"/qwen_image_2.1_vae*.safetensors; do
	[ -e "$f" ] || continue
	ln -sfn "$f" "${INSTALL_DIR}/ComfyUI/models/vae/$(basename "$f")"
	echo "  vae/$(basename "$f")"
done

chown -R "$RUN_USER" "$INSTALL_DIR"

# ── 5. Done — print the gateway config snippet ────────────────────────────────
echo "[5/5] Done. The gateway manages ComfyUI's lifecycle (kind: web):"
echo
echo "  comfyui:"
echo "    kind: web"
echo "    command: |"
echo "      cd ${INSTALL_DIR}/ComfyUI && exec ${INSTALL_DIR}/venv/bin/python main.py"
echo "      --listen 127.0.0.1 --port ${PORT}"
echo "    host: 127.0.0.1:${PORT}"
echo "    health_endpoint: /"
echo "    ready_timeout: 30m"
echo
echo " Then: sync the gateway config + 'sudo systemctl restart llm-gateway',"
echo " open http://<host>:1234/ and click 'comfyui' (first start ~1 min)."
echo
echo " Workflow tips:"
echo "  - In the UI: Workflow → Browse Templates → search 'qwen' for the"
echo "    official T2I / Edit templates; swap the model loader for"
echo "    'UnetLoader (GGUF)' and pick the Q4_0 gguf; CLIP loader type"
echo "    'qwen_image' with the Qwen3VL gguf; select the linked VAE."
echo "  - Compose multiple photos: load the Edit template and feed several"
echo "    reference images (the model accepts up to 10)."
echo "  - Remove something but keep the rest 1:1: right-click the image →"
echo "    'Open in MaskEditor', paint the region to regenerate — everything"
echo "    outside the mask stays pixel-identical."
echo "  - ComfyUI shares the single VRAM slot through the gateway: opening it"
echo "    unloads the chat/image model, and any chat request unloads ComfyUI."
