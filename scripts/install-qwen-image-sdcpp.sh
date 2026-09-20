#!/usr/bin/env bash
# Usage: sudo ./scripts/install-qwen-image-sdcpp.sh
#
# Installs stable-diffusion.cpp's sd-server (single C++ binary with an
# embedded web UI and OpenAI-compatible images API) plus the Qwen-Image-2.1
# models into /opt/sdcpp, sized for a 16 GB VRAM GPU (RTX 5060 Ti) with 32 GB
# RAM. Then add the `qwen-image-2.1` model from config/gem12gpu.yaml to the
# gateway config — the gateway launches/kills sd-server like llama-server and
# proxies its web UI at /app/qwen-image-2.1.
#
# Quant override: DIFFUSION_QUANT=Q6_K sudo -E ./scripts/install-qwen-image-sdcpp.sh
# (Q2_K 2.6 GB, Q4_0 4.2 GB, Q5_0 5.1 GB, Q6_K 6.0 GB, Q8_0 7.7 GB)

set -euo pipefail

INSTALL_DIR="/opt/sdcpp"
BIN_DIR="${INSTALL_DIR}/bin"
MODEL_DIR="${INSTALL_DIR}/models"
SRC_DIR="${INSTALL_DIR}/src"
CUDA_ARCH="${CUDA_ARCH:-120}"            # 5060 Ti = Blackwell sm_120
DIFFUSION_QUANT="${DIFFUSION_QUANT:-Q8_0}"

if [ "$(id -u)" -ne 0 ]; then
	echo "ERROR: this script must be run as root."
	echo "       Usage: sudo $(basename "$0")"
	exit 1
fi

echo "======================================================"
echo " Qwen-Image-2.1 via sd-server (stable-diffusion.cpp)"
echo "------------------------------------------------------"
echo " Installs to ${INSTALL_DIR} (binary + ~16 GB of models)."
echo " Weights ≈ 15.5 GB (Q${DIFFUSION_QUANT} DiT + Qwen3-VL-8B"
echo " Q4_K_M + mmproj + VAE) → with --offload-to-cpu they live"
echo " in RAM (~16 GB of 32 GB), so no OOM kills on 16 GB VRAM."
echo "======================================================"
echo

# ── 0. NVIDIA check + build/download deps ─────────────────────────────────────
if ! nvidia-smi &>/dev/null; then
	echo "ERROR: no NVIDIA driver (nvidia-smi)."
	exit 1
fi
GPU_NAME=$(nvidia-smi --query-gpu=name --format=csv,noheader | head -1)
echo "GPU: ${GPU_NAME}"
echo

for dep in curl git cmake unzip; do
	if ! command -v "$dep" &>/dev/null; then
		if command -v apt-get &>/dev/null; then
			echo "Installing missing dependency: $dep"
			apt-get update -qq && apt-get install -y -qq "$dep"
		else
			echo "ERROR: '$dep' is required."
			exit 1
		fi
	fi
done

mkdir -p "$BIN_DIR" "$MODEL_DIR"

# ── 1. Get sd-server: prebuilt CUDA release, else build from source ───────────
if [ -x "${BIN_DIR}/sd-server" ]; then
	echo "[1/3] sd-server already installed at ${BIN_DIR}/sd-server — skipping."
else
	echo "[1/3] Fetching sd-server..."
	API_JSON=$(curl -sSf https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/latest)
	ASSET_URL=$(echo "$API_JSON" \
		| grep -oE '"browser_download_url": *"[^"]+"' \
		| cut -d'"' -f4 \
		| grep -i 'linux' | grep -i 'cuda' | grep -i 'x64' | head -1 || true)

	if [ -n "$ASSET_URL" ]; then
		echo "  Downloading prebuilt CUDA release: $(basename "$ASSET_URL")"
		TMP_ZIP=$(mktemp --suffix=.zip)
		curl -fL --retry 3 -o "$TMP_ZIP" "$ASSET_URL"
		TMP_UNZIP=$(mktemp -d)
		unzip -q -o "$TMP_ZIP" -d "$TMP_UNZIP"
		find "$TMP_UNZIP" -name 'sd-server' -type f -exec cp {} "${BIN_DIR}/sd-server" \;
		rm -rf "$TMP_ZIP" "$TMP_UNZIP"
	else
		echo "  No prebuilt Linux CUDA asset found — building from source (~5-10 min)."
		if [ ! -d "$SRC_DIR/.git" ]; then
			git clone --recursive https://github.com/leejet/stable-diffusion.cpp "$SRC_DIR"
		else
			git -C "$SRC_DIR" pull --ff-only && git -C "$SRC_DIR" submodule update --init --recursive
		fi
		# The build needs nvcc. Prefer PATH; fall back to the CUDA toolkit and
		# to the nvcc shipped inside the vllm venv (install-vllm-globally.sh
		# installs it there, off the default PATH).
		NVCC="$(command -v nvcc || true)"
		if [ -z "$NVCC" ]; then
			for c in /usr/local/cuda/bin/nvcc \
				/opt/vllm/venv/lib/python*/site-packages/nvidia/cuda_nvcc/bin/nvcc; do
				if [ -x "$c" ]; then NVCC="$c"; break; fi
			done
		fi
		# SD_CUDA / SD_CUBLAS cover old and new build option names; extras are
		# harmless unused cache entries on whichever naming applies.
		CMAKE_ARGS=(-DSD_CUDA=ON -DSD_CUBLAS=ON -DGGML_CUDA=ON \
			-DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCH}")
		if [ -n "$NVCC" ]; then
			echo "  Using nvcc: ${NVCC}"
			CMAKE_ARGS+=(-DCMAKE_CUDA_COMPILER="${NVCC}")
		else
			echo "  WARN: nvcc not found — cmake may fail; install cuda toolkit first."
		fi
		cmake -B "${SRC_DIR}/build" -S "$SRC_DIR" "${CMAKE_ARGS[@]}" \
			-DCMAKE_BUILD_TYPE=Release
		cmake --build "${SRC_DIR}/build" --config Release -j"$(nproc)"
		find "${SRC_DIR}/build" -name 'sd-server' -type f -exec cp {} "${BIN_DIR}/sd-server" \;
	fi
	[ -x "${BIN_DIR}/sd-server" ] || { echo "ERROR: sd-server binary not found after install."; exit 1; }
	echo "  Installed: ${BIN_DIR}/sd-server"
fi
echo

# ── 2. Download models ────────────────────────────────────────────────────────
# curl -C - resumes partial downloads; -f fails loudly on a wrong filename.
fetch_model() {
	local url="$1" out="$2" optional="${3:-no}"
	if [ -s "$out" ]; then
		echo "  exists: $(basename "$out")"
		return 0
	fi
	echo "  $(basename "$out") ..."
	if ! curl -fL --retry 3 -C - -o "$out" "$url"; then
		rm -f "$out"
		if [ "$optional" = "yes" ]; then
			echo "  WARN: optional download failed — ${url} (check the repo for the exact filename)"
			return 0
		fi
		echo "ERROR: download failed — verify the filename at:"
		echo "  ${url%\/*}"
		exit 1
	fi
}

echo "[2/3] Downloading models to ${MODEL_DIR} (resumable)..."
fetch_model \
	"https://huggingface.co/leejet/Qwen-Image-2.1-GGUF/resolve/main/qwen_image_2.1-${DIFFUSION_QUANT}.gguf" \
	"${MODEL_DIR}/qwen_image_2.1-${DIFFUSION_QUANT}.gguf"

fetch_model \
	"https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct-GGUF/resolve/main/Qwen3VL-8B-Instruct-Q4_K_M.gguf" \
	"${MODEL_DIR}/Qwen3VL-8B-Instruct-Q4_K_M.gguf"

fetch_model \
	"https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct-GGUF/resolve/main/Qwen3VL-8B-Instruct-mmproj-BF16.gguf" \
	"${MODEL_DIR}/Qwen3VL-8B-Instruct-mmproj-BF16.gguf"

fetch_model \
	"https://huggingface.co/Comfy-Org/Qwen-Image-2.1/resolve/main/vae/qwen_image_2.1_vae_bf16.safetensors" \
	"${MODEL_DIR}/qwen_image_2.1_vae_bf16.safetensors"
echo

# ── 3. Done — print the gateway config snippet ────────────────────────────────
echo "[3/3] Done. Disk usage:"
du -sh "$MODEL_DIR" "$BIN_DIR"
df -h "$INSTALL_DIR" | tail -1
echo
echo "Add this model to the gateway config (see config/gem12gpu.yaml):"
echo "  qwen-image-2.1:"
echo "    kind: web"
echo "    command: |"
echo "      ${BIN_DIR}/sd-server"
echo "      --diffusion-model ${MODEL_DIR}/qwen_image_2.1-${DIFFUSION_QUANT}.gguf"
echo "      --llm ${MODEL_DIR}/Qwen3VL-8B-Instruct-Q4_K_M.gguf"
echo "      --llm_vision ${MODEL_DIR}/Qwen3VL-8B-Instruct-mmproj-BF16.gguf"
echo "      --vae ${MODEL_DIR}/qwen_image_2.1_vae_bf16.safetensors"
echo "      --diffusion-fa --cfg-scale 6.0 --offload-to-cpu"
echo "      --listen-ip 127.0.0.1 --listen-port 1235"
echo "    host: 127.0.0.1:1235"
echo "    health_endpoint: /"
echo "    ready_timeout: 15m"
echo
echo "Then restart the gateway and open:"
echo "  Web UI: http://<gateway-host>:1234/  → click 'qwen-image-2.1'"
echo "  API:    POST /v1/images/generations  {\"model\": \"qwen-image-2.1\", \"prompt\": \"...\"}"
echo
echo "Editing and OOM protection are on by default: --llm_vision enables image"
echo "editing, --offload-to-cpu keeps the ~15.5 GB of weights in RAM instead of"
echo "the 16 GB VRAM. Speed over safety at ≤1024 px: drop --offload-to-cpu."
