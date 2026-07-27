#!/usr/bin/env bash
# Usage: sudo ./scripts/install-vllm-globally.sh
#
# Creates a uv-managed venv at /opt/vllm/venv with torch + vLLM, then
# symlinks 'vllm' into /usr/local/bin so it's callable from config.yaml.

set -euo pipefail

VENV_DIR="/opt/vllm"
PYVER="3.12"

if [ "$(id -u)" -ne 0 ]; then
	echo "ERROR: this script must be run as root."
	echo "       Usage: sudo $(basename "$0")"
	exit 1
fi


echo "======================================================"
echo "    vLLM — uv-managed venv (isolated, no system break)"
echo "------------------------------------------------------"
echo " Creates a clean Python ${PYVER} venv at ${VENV_DIR}/venv"
echo " with torch + vLLM pre-built wheels."
echo ""
echo " Works on Blackwell/Ada/Lovelace — ships its own CUDA"
echo " runtime; does not conflict with cuda-toolkit-12."
echo "======================================================"
echo

# ── 0. NVIDIA check ────────────────────────────────────────────────────────────
if ! nvidia-smi &>/dev/null; then
	echo "ERROR: no NVIDIA driver (nvidia-smi)."
	exit 1
fi
DRIVER=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)
GPU_NAME=$(nvidia-smi --query-gpu=name --format=csv,noheader | head -1)
echo "NVIDIA driver: ${DRIVER}"
echo "GPU:           ${GPU_NAME}"
echo

# ── 1. Install uv if missing ───────────────────────────────────────────────────
if ! command -v uv &>/dev/null; then
	echo "[0/4] Installing uv..."
	curl -LsSf https://astral.sh/uv/install.sh | sh -s --
	export UV_NO_MODIFY_PATH=1
	export PATH="$HOME/.local/bin:$PATH"
fi

# ── 2. Ensure Python ${PYVER} is available (managed by uv) ────────────────────
if ! uv python list | grep -q "$PYVER"; then
	echo "[1/4] Downloading uv-managed Python ${PYVER}..."
	uv python install "$PYVER"
fi

# ── 3. Create venv + install vLLM ──────────────────────────────────────────────
rm -rf "${VENV_DIR:?}/venv"  # safety: clear existing

echo "[2/4] Creating venv at ${VENV_DIR}/venv..."
uv venv "${VENV_DIR}/venv" --python "$PYVER"

echo "[3/4] Installing torch + vLLM (pre-built wheels)..."
source "${VENV_DIR}/venv/bin/activate"
uv pip install huggingface_hub vllm \
	--torch-backend=auto

# ── 4. Symlink 'vllm' CLI + python ────────────────────────────────────────────
echo "[4/4] Sym-linking into \$PATH..."
ln -sf "${VENV_DIR}/venv/bin/vllm" /usr/local/bin/vllm
ln -sf "${VENV_DIR}/venv/bin/python3" /usr/local/bin/python3-vllm

# ── 5. Download model + verify (run as original user) ─────────────────────────
ORIGINAL_HOME="$HOME"
if [ -n "$SUDO_USER" ]; then
	ORIGINAL_HOME="/home/$SUDO_USER"
fi

HF_HOME="${HF_HOME:-$ORIGINAL_HOME/.cache/huggingface}"
MODEL_DIR="$HF_HOME/hub/models--unsloth/gemma-4-12b-it-NVFP4"

echo ""
echo "[5/5] Downloading unsloth/gemma-4-12b-it-NVFP4 (~9.3 GB)..."

if [ -d "$MODEL_DIR/download" ] && \
   [ "$(find "$MODEL_DIR" -name '*.safetensors' -print -quit)" ]; then
	echo " -> already cached ($(du -sh "$MODEL_DIR" 2>/dev/null | cut -f1))"
else
	HUGGINGFACE_HUB_CACHE="$HF_HOME" "${VENV_DIR}/venv/bin/python3" -c "
from huggingface_hub import snapshot_download
snapshot_download('unsloth/gemma-4-12b-it-NVFP4', local_dir='$MODEL_DIR')
" || {
		echo " -> download failed"; exit 1
	}
	chown -R "${SUDO_USER:-root}:${SUDO_USER:+$(id -Gn "$SUDO_USER" 2>/dev/null | head -1)}" \
		"$MODEL_DIR" 2>/dev/null || true
	echo " -> cached ($(du -sh "$MODEL_DIR" | cut -f1))"
fi

echo ""
"${VENV_DIR}/venv/bin/python3" -c "import torch, vllm; print(f'PyTorch {torch.__version__} + CUDA {torch.version.cuda}'); print(f'vLLM  {vllm.__version__}')"

echo ""
echo "Done!"
echo ""
echo " In config.yaml use:"
echo "   command: vllm serve unsloth/gemma-4-12b-it-NVFP4 \\"
echo "          --port 1235 --max-model-len 131072 --kv-cache-dtype fp8"
