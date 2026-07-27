#!/usr/bin/env bash
# Usage: sudo ./scripts/install-vllm-globally.sh
#
# Creates a uv-managed venv at /opt/vllm/venv with torch + vLLM, registers
# nvidia pip libs with ldconfig, and installs a 'vllm' wrapper into
# /usr/local/bin so it's callable from config.yaml.

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
	echo "[1/5] Installing uv..."
	curl -LsSf https://astral.sh/uv/install.sh | sh -s --
	export UV_NO_MODIFY_PATH=1
	export PATH="$HOME/.local/bin:$PATH"
fi

# ── 2. Ensure Python ${PYVER} is available (managed by uv) ────────────────────
if ! uv python list | grep -q "$PYVER"; then
	echo "[2/5] Downloading uv-managed Python ${PYVER}..."
	uv python install "$PYVER"
fi

# ── 3. Create venv + install vLLM ──────────────────────────────────────────────
rm -rf "${VENV_DIR:?}/venv"  # safety: clear existing

echo "[3/5] Creating venv at ${VENV_DIR}/venv..."
uv venv "${VENV_DIR}/venv" --python "$PYVER"

echo "[4/5] Installing torch + vLLM (pre-built wheels, CUDA 13.0 for Blackwell)..."
source "${VENV_DIR}/venv/bin/activate"
uv pip install huggingface_hub vllm nvidia-cuda-nvcc \
	--torch-backend=cu130

# ── 5. Register nvidia libs with ldconfig + wrapper + verify ─────────────────
# vLLM's C extensions link against libcudart.so.13 (nvidia-cuda-runtime 13.x)
# which lives in site-packages/nvidia/*/lib — not on the default loader path.
# Register those dirs with ldconfig so the dynamic linker finds them natively.
SITE_PKG="${VENV_DIR}/venv/lib/python${PYVER}/site-packages"

echo "[5/5] Registering nvidia libs + creating wrapper in \$PATH..."
ls -d "${SITE_PKG}"/nvidia/*/lib 2>/dev/null > /etc/ld.so.conf.d/vllm-nvidia.conf
ldconfig

# Wrapper runs the pip entry-point script with the venv python explicitly
# (the script's own shebang may be broken after uv venv creation)
rm -f /usr/local/bin/vllm
cat > /usr/local/bin/vllm <<EOF
#!/usr/bin/env bash
export CUDA_HOME="${VENV_DIR}/venv/lib/python${PYVER}/site-packages/nvidia/cuda_nvcc"
export PATH="\${CUDA_HOME}/bin:\$PATH"
export FLASHINFER_CUDA_ARCH_LIST="12.0f"
export TORCH_CUDA_ARCH_LIST="12.0f"
exec "${VENV_DIR}/venv/bin/python3" "${VENV_DIR}/venv/bin/vllm" "\$@"
EOF
chmod +x /usr/local/bin/vllm

ln -sf "${VENV_DIR}/venv/bin/python3" /usr/local/bin/python3-vllm

"${VENV_DIR}/venv/bin/python3" -c \
	"import torch, vllm; print(f'PyTorch {torch.__version__} + CUDA {torch.version.cuda}'); print(f'vLLM  {vllm.__version__}')"

echo ""
echo "Done!"
echo ""
echo " To use it, add to config.yaml:"
echo "   command: vllm serve <model-name> \\"
echo "          --port 1235 --max-model-len 131072 --kv-cache-dtype fp8"
