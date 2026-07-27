#!/usr/bin/env bash
# Usage: sudo ./scripts/install-vllm-globally.sh

set -e

echo "======================================================"
echo "    vLLM — global install (pip-installed) + NVIDIA"
echo "------------------------------------------------------"
echo " Installs torch + vllm system-wide so vllm serve can be"
echo " called directly (no venv, no symlinks)."
echo ""
echo " Works on Blackwell/Ada/Lovelace — ships its own CUDA"
echo " runtime; does not conflict with cuda-toolkit-12."
echo "======================================================"
echo

# 0. NVIDIA driver check
if ! command -v nvidia-smi &>/dev/null; then
    echo "ERROR: no NVIDIA driver (nvidia-smi)."
    echo "       Run install-8845hs-gpu.sh first."
    exit 1
fi

echo "NVIDIA driver: $(sudo nvidia-smi --query-gpu=driver_version \
    --format=csv,noheader | head -1)"
echo GPU: "$(sudo nvidia-smi --query-gpu=name --format=csv,noheader | head -1)"
echo

# 1. Install system-wide via pip (no venv)
echo "[1/3] Installing torch + vllm system-wide..."

pip3 install --upgrade pip || true

pip3 install \
    torch torchvision torchaudio \
    --index-url https://download.pytorch.org/whl/cu129 \
    2>&1 | tee /tmp/vllm_pip.log || {
    echo "pip install failed — see /tmp/vllm_pip.log"; exit 1
}

pip3 install \
    "huggingface_hub[cli]" \
    vllm \
    2>&1 | tee -a /tmp/vllm_pip.log || {
    echo "pip install failed — see /tmp/vllm_pip.log"; exit 1
}

echo ""
python3 -c "import torch, vllm; print(f'  PyTorch {torch.__version__} + CUDA {torch.version.cuda}'); print(f'  vLLM       {vllm.__version__}')"

# Preserve original user's home when run as sudo — without this the HF cache
# lands in /root/ instead of /home/<user>/ and subsequent non-root processes
# won't find the downloaded model.
ORIGINAL_HOME="$HOME"
if [ -n "$SUDO_USER" ]; then
    ORIGINAL_HOME="/home/$SUDO_USER"
fi

# 2. Download the safetensors model into the standard HF cache
HF_HOME="${HF_HOME:-$ORIGINAL_HOME/.cache/huggingface}"
MODEL_DIR="$HF_HOME/hub/models--unsloth/gemma-4-12b-it-NVFP4"

echo ""
echo "[2/3] Downloading unsloth/gemma-4-12b-it-NVFP4 (~9.3 GB)..."

if [ "$(find "$MODEL_DIR" -name '*.safetensors' 2>/dev/null | wc -l)" -gt 0 ]; then
    echo " -> already cached ($(du -sh "$MODEL_DIR" | cut -f1))"
else
    sudo -u "${SUDO_USER:-root}" huggingface-cli download unsloth/gemma-4-12b-it-NVFP4 \
        --local-dir "$MODEL_DIR" || {
        echo " -> download failed — check /tmp/vllm_pip.log"; exit 1
    }
    echo " -> cached ($(du -sh "$MODEL_DIR" | cut -f1))"
fi

# 3. Quick verification that the model can be instantiated on GPU
echo ""
echo "[3/3] Verifying..."
python3 -c "
import torch
from transformers import AutoConfig
c = AutoConfig.from_pretrained('$MODEL_DIR', trust_remote_code=True)
print(f'  Architecture: {type(c).__name__}')
qt = getattr(c, 'quantization_config', None)
if qt: print('  Quant method: compressed-tensors (NVFP4 + Float8)')
print('  OK')
"

echo ""
echo "Done!"
echo ""
echo " Usage in config.yaml (command field):"
echo "   vllm serve unsloth/gemma-4-12b-it-NVFP4 \\
       --port 1235 --max-model-len 131072 --kv-cache-dtype fp8"
