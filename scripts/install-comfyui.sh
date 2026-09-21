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
# Upgrade: bump COMFY_REF / GGUF_NODE_REF at the top, then re-run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR="/opt/comfyui"
PYVER="3.12"
COMFY_REPO="https://github.com/comfyanonymous/ComfyUI.git"
GGUF_NODE_REPO="https://github.com/leejet/ComfyUI-GGUF.git"
SDCPP_MODELS="/opt/sdcpp/models"
PORT="${COMFYUI_PORT:-8188}"

# Pinned upstream revisions: re-running the installer must not silently
# deploy new upstream code. Bump deliberately, e.g.:
#   COMFY_REF=<sha> GGUF_NODE_REF=<sha> sudo -E ./scripts/install-comfyui.sh
COMFY_REF="${COMFY_REF:-b0f4b7b294ce482a2e071d9d762c133d38c7aa07}"
GGUF_NODE_REF="${GGUF_NODE_REF:-edd981b10e107d3b8f58e16c498f2d08f631bc47}"

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
echo " models:  symlinked from ${SDCPP_MODELS} + NVFP4 package"
echo "          (Blackwell-native fp4 — preferred, ~12 GB download)"
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
# uv is pinned and checksum-verified (same constants as the other installers).
UV_VERSION="0.12.17"
UV_SHA256="fa82fd8dde8e8eefdecada6aa0889666556cfceb690d06e0c3bca49eb3070a63" # uv-x86_64-unknown-linux-gnu.tar.gz
install_uv() {
	if command -v uv &>/dev/null; then
		echo "[1/5] uv found: $(uv --version)"
		return 0
	fi
	echo "[1/6] Installing pinned uv ${UV_VERSION}..."
	local tmp
	tmp="$(mktemp -d)"
	if ! curl -fsSL --retry 3 -o "${tmp}/uv.tar.gz" \
		"https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz" \
		|| ! echo "${UV_SHA256}  ${tmp}/uv.tar.gz" | sha256sum -c --status -; then
		echo "ERROR: uv download or checksum failed."
		rm -rf "$tmp"
		exit 1
	fi
	tar -xzf "${tmp}/uv.tar.gz" -C "$tmp"
	install -m 0755 "${tmp}/uv-x86_64-unknown-linux-gnu/uv" "${tmp}/uv-x86_64-unknown-linux-gnu/uvx" /usr/local/bin
	rm -rf "$tmp"
}
install_uv
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
echo "[2/6] Fetching ComfyUI + leejet/ComfyUI-GGUF custom node..."
COMFY_DIR="${INSTALL_DIR}/ComfyUI"
PATCH_FILE="${INSTALL_DIR}/patches/qwen21-nvfp4-conditioning.patch"
PATCH_URL="https://huggingface.co/BennyDaBall/Qwen-Image-2.1-NVFP4/resolve/main/runtime/qwen21-nvfp4-conditioning.patch"
# The patch is bundled in the repo (patches/) and reviewed there; the HF
# URL is only a fallback for extracted release zips without the file.
REPO_PATCH="${SCRIPT_DIR}/../patches/qwen21-nvfp4-conditioning.patch"

# Native NVFP4 text conditioning: one-file ComfyUI patch (GPL-3.0, same as
# ComfyUI). Detects NVFP4 checkpoints by metadata at runtime, falls back to
# the original path on other checkpoints/GPUs, reversible via git apply -R.
apply_nvfp4_patch() {
	if [ ! -s "${PATCH_FILE}" ]; then
		mkdir -p "$(dirname "${PATCH_FILE}")"
		if [ -s "${REPO_PATCH}" ]; then
			cp "${REPO_PATCH}" "${PATCH_FILE}"
		else
			echo "  Fetching patch from upstream (not bundled in this checkout)..."
			curl -fL --retry 3 -o "${PATCH_FILE}" "${PATCH_URL}"
		fi
	fi
	if git -C "${COMFY_DIR}" apply --reverse --check "${PATCH_FILE}" 2>/dev/null; then
		echo "  NVFP4 conditioning patch: already applied."
	elif git -C "${COMFY_DIR}" apply --check "${PATCH_FILE}" 2>/dev/null; then
		git -C "${COMFY_DIR}" apply "${PATCH_FILE}"
		echo "  NVFP4 conditioning patch applied — native FP4 text encoding."
	else
		echo "  WARN: NVFP4 conditioning patch does not apply to this ComfyUI"
		echo "        revision — skipping (encoding runs slower, generation works)."
	fi
}

# checkout_ref fetches the pinned revision into an existing-or-new clone and
# checks it out (detached). No silent drift to a moving branch head.
checkout_ref() {
	local repo="$1" dir="$2" ref="$3"
	if [ ! -d "${dir}/.git" ]; then
		git clone "$repo" "$dir"
	fi
	if ! git -C "$dir" fetch --depth 1 origin "$ref" 2>/dev/null \
		|| ! git -C "$dir" checkout --detach FETCH_HEAD; then
		echo "ERROR: cannot checkout ${ref} in ${dir}."
		echo "       If the pinned revision was removed upstream, re-run with e.g. ${repo##*/} ref override."
		exit 1
	fi
}

# Unapply the patch first so it can never block the checkout.
if [ -s "${PATCH_FILE}" ] && git -C "${COMFY_DIR}" apply --reverse --check "${PATCH_FILE}" 2>/dev/null; then
	git -C "${COMFY_DIR}" apply --reverse "${PATCH_FILE}"
fi
checkout_ref "$COMFY_REPO" "${COMFY_DIR}" "$COMFY_REF"
checkout_ref "$GGUF_NODE_REPO" "${COMFY_DIR}/custom_nodes/ComfyUI-GGUF" "$GGUF_NODE_REF"
echo "  ComfyUI at ${COMFY_REF:0:12}, GGUF node at ${GGUF_NODE_REF:0:12} (pin via COMFY_REF / GGUF_NODE_REF)."
apply_nvfp4_patch

# ── 3. Dependencies (CUDA torch for Blackwell via uv's torch backend) ────────
echo "[3/6] Installing dependencies (CUDA torch, ~5 GB)..."
source "${INSTALL_DIR}/venv/bin/activate"
uv pip install -r "${INSTALL_DIR}/ComfyUI/requirements.txt" --torch-backend=cu130
NODE_REQ="${INSTALL_DIR}/ComfyUI/custom_nodes/ComfyUI-GGUF/requirements.txt"
if [ -f "$NODE_REQ" ]; then
	uv pip install -r "$NODE_REQ"
else
	uv pip install gguf
fi

# ── 4. Model symlinks (shared with sd-server — no extra disk) ────────────────
echo "[4/6] Linking Qwen-Image-2.1 models into ComfyUI..."
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

# ── 5. NVFP4 package (Blackwell-native) + ready-made workflows ────────────────
# BennyDaBall/Qwen-Image-2.1-NVFP4: mixed precision — attention/MLP matrices
# in native Blackwell FP4 (E2M1 + FP8 block scales), sensitive layers kept
# BF16. Loads via CORE ComfyUI nodes (UNETLoader + CLIPLoader qwen_image);
# ~2x faster than BF16 on RTX 50-series. Workflows land in the UI's panel.
NVFP4_REPO="https://huggingface.co/BennyDaBall/Qwen-Image-2.1-NVFP4"
fetch_hf() {
	local repo="$1" path="$2" out="$3"
	if [ -s "$out" ]; then
		echo "  exists: ${path}"
		return 0
	fi
	echo "  ${path} ..."
	mkdir -p "$(dirname "$out")"
	if ! curl -fL --retry 3 -C - -o "$out" "${repo}/resolve/main/${path}"; then
		rm -f "$out"
		echo "ERROR: download failed — ${repo}/resolve/main/${path}"
		exit 1
	fi
}

echo "[5/6] Downloading the NVFP4 package + workflows (~12 GB, resumable)..."
fetch_hf "${NVFP4_REPO}" "diffusion_models/qwen_image_2.1_nvfp4.safetensors" \
	"${INSTALL_DIR}/ComfyUI/models/diffusion_models/qwen_image_2.1_nvfp4.safetensors"
fetch_hf "${NVFP4_REPO}" "text_encoders/qwen3vl_8b_nvfp4.safetensors" \
	"${INSTALL_DIR}/ComfyUI/models/text_encoders/qwen3vl_8b_nvfp4.safetensors"
for wf in 01_Text_to_Image 02_Image_Editing 03_Transparent_RGBA 04_2K_Typography; do
	fetch_hf "${NVFP4_REPO}" "workflows/${wf}.json" \
		"${INSTALL_DIR}/ComfyUI/user/default/workflows/${wf}.json"
done
fetch_hf "${NVFP4_REPO}" "input/qwen_image_2.1_edit_reference.png" \
	"${INSTALL_DIR}/ComfyUI/input/qwen_image_2.1_edit_reference.png"
echo "  (VAE: already linked from ${SDCPP_MODELS})"
echo "  NOTE: optional FP4 text-encoder acceleration patch + prompting guide:"
echo "        ${NVFP4_REPO} (runtime/, PROMPTING.md)"

chown -R "$RUN_USER" "$INSTALL_DIR"

# ── 5. Done — print the gateway config snippet ────────────────────────────────
echo "[6/6] Done. The gateway manages ComfyUI's lifecycle (kind: web):"
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
echo "  - Preinstalled (Workflows panel): 01 Text-to-Image, 02 Image Editing,"
echo "    03 Transparent RGBA, 04 2K Typography — all wired to the NVFP4"
echo "    models (40 steps, euler, cfg 1, 1024x1024 / 2K)."
echo "  - GGUF alternative: templates → search 'qwen' + UnetLoader (GGUF)"
echo "    with the Q4_0/Q6_K gguf; CLIP loader type 'qwen_image' (Qwen3VL gguf)."
echo "  - Compose multiple photos: the Editing workflow accepts several"
echo "    reference images (the model takes up to 10)."
echo "  - Remove something but keep the rest 1:1: right-click the image →"
echo "    'Open in MaskEditor', paint the region to regenerate — everything"
echo "    outside the mask stays pixel-identical."
echo "  - Prompting guide: PROMPTING.md in the NVFP4 repo."
echo "  - ComfyUI shares the single VRAM slot through the gateway: opening it"
echo "    unloads the chat/image model, and any chat request unloads ComfyUI."
