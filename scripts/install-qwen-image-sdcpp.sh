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
# Quant override: DIFFUSION_QUANT=Q8_0 sudo -E ./scripts/install-qwen-image-sdcpp.sh
# (default Q6_K; also available: Q2_K 2.6 GB, Q4_0 4.2 GB, Q5_0 5.1 GB, Q8_0 7.7 GB)

set -euo pipefail

INSTALL_DIR="/opt/sdcpp"
BIN_DIR="${INSTALL_DIR}/bin"
MODEL_DIR="${INSTALL_DIR}/models"
SRC_DIR="${INSTALL_DIR}/src"
CUDA_ARCH="${CUDA_ARCH:-120}"            # 5060 Ti = Blackwell sm_120
DIFFUSION_QUANT="${DIFFUSION_QUANT:-Q6_K}"

if [ "$(id -u)" -ne 0 ]; then
	echo "ERROR: this script must be run as root."
	echo "       Usage: sudo $(basename "$0")"
	exit 1
fi

echo "======================================================"
echo " Qwen-Image-2.1 via sd-server (stable-diffusion.cpp)"
echo "------------------------------------------------------"
echo " Installs to ${INSTALL_DIR} (binary + ~14 GB of models)."
echo " Weights ≈ 13.7 GB (${DIFFUSION_QUANT} DiT + Qwen3-VL-8B"
echo " Q4_K_M + mmproj + VAE) → with --offload-to-cpu they live"
echo " in RAM (~14 GB of 32 GB), so no OOM kills on 16 GB VRAM."
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

# nvcc_version <path> prints e.g. "12.4"; ver_ge <a> <b> returns 0 when a >= b.
nvcc_version() { "$1" --version 2>/dev/null | sed -n 's/.*release \([0-9.]*\).*/\1/p' | head -n1; }
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]; }

# scan_nvcc fills NVCC/NVCC_VER with the best nvcc >= 12.8 (sm_120 / Blackwell
# requires CUDA 12.8+) and PTX_NVCC/PTX_VER with the best older one, which can
# still produce an sm_89 SASS+PTX build that the Blackwell driver JITs.
# Deliberately skips pip-layout nvcc copies (e.g. the vllm venv) — CMake's
# toolkit detection cannot use them.
scan_nvcc() {
	local cands=() c v
	command -v nvcc >/dev/null 2>&1 && cands+=("$(command -v nvcc)")
	for c in /usr/local/cuda*/bin/nvcc; do
		[ -x "$c" ] && cands+=("$c")
	done
	NVCC=""; NVCC_VER=""; PTX_NVCC=""; PTX_VER=""
	for c in "${cands[@]}"; do
		v="$(nvcc_version "$c")"
		[ -z "$v" ] && continue
		if ver_ge "$v" 12.8; then
			if [ -z "$NVCC_VER" ] || ver_ge "$v" "$NVCC_VER"; then
				NVCC="$c"; NVCC_VER="$v"
			fi
		elif [ -z "$PTX_VER" ] || ver_ge "$v" "$PTX_VER"; then
			PTX_NVCC="$c"; PTX_VER="$v"
		fi
	done
}

mkdir -p "$BIN_DIR" "$MODEL_DIR"

# The browser UI is compiled INTO the binary (-DSD_SERVER_BUILD_FRONTEND=ON,
# needs Node >= 20 + pnpm >= 10 at build time). Without it the server works
# but / answers with a plain "Stable Diffusion Server is running" text.
# The marker file lets re-runs detect and rebuild such a frontend-less binary.
FRONTEND_MARKER="${BIN_DIR}/.embedded-webui"
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
ensure_frontend_toolchain() {
	command -v node >/dev/null 2>&1 || return 1
	[ "$(node --version | sed -n 's/^v\([0-9]*\).*/\1/p')" -ge 20 ] || return 1
	command -v pnpm >/dev/null 2>&1 && return 0
	# Prefer a real pnpm binary over corepack shims: CMake must find pnpm at
	# configure time, and a missing one only produces a soft warning plus a
	# UI-less binary.
	npm install -g pnpm@10 >/dev/null 2>&1 && command -v pnpm >/dev/null 2>&1 && return 0
	if command -v corepack >/dev/null 2>&1; then
		corepack enable >/dev/null 2>&1 && corepack prepare pnpm@10 --activate >/dev/null 2>&1 || true
	fi
	command -v pnpm >/dev/null 2>&1
}

# ── 1. Get sd-server: prebuilt CUDA release, else build from source ───────────
if [ -x "${BIN_DIR}/sd-server" ] && [ -f "${FRONTEND_MARKER}" ]; then
	echo "[1/3] sd-server already installed at ${BIN_DIR}/sd-server — skipping."
else
	if [ -x "${BIN_DIR}/sd-server" ]; then
		echo "[1/3] sd-server exists but was built without the embedded web UI — rebuilding."
	else
		echo "[1/3] Fetching sd-server..."
	fi
	API_JSON=$(curl -sSf https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/latest)
	ASSET_URL=$(echo "$API_JSON" \
		| grep -oE '"browser_download_url": *"[^"]+"' \
		| cut -d'"' -f4 \
		| grep -i 'linux' | grep -i 'cuda' | head -n1 || true)

	if [ -n "$ASSET_URL" ]; then
		echo "  Downloading prebuilt CUDA release: $(basename "$ASSET_URL")"
		TMP_ZIP=$(mktemp --suffix=.zip)
		curl -fL --retry 3 -o "$TMP_ZIP" "$ASSET_URL"
		TMP_UNZIP=$(mktemp -d)
		unzip -q -o "$TMP_ZIP" -d "$TMP_UNZIP"
		find "$TMP_UNZIP" -name 'sd-server' -type f -exec cp {} "${BIN_DIR}/sd-server" \;
		rm -rf "$TMP_ZIP" "$TMP_UNZIP"
		touch "${FRONTEND_MARKER}"   # official releases ship the embedded web UI
	else
		echo "  No prebuilt Linux CUDA asset found — building from source (~5-10 min)."
		echo "  Linux assets in the release (for reference):"
		echo "$API_JSON" | grep -oE '"browser_download_url": *"[^"]+"' \
			| cut -d'"' -f4 | grep -i linux | head -n5 | sed 's/^/    /' || true
		if [ ! -d "$SRC_DIR/.git" ]; then
			git clone --recursive https://github.com/leejet/stable-diffusion.cpp "$SRC_DIR"
		else
			git -C "$SRC_DIR" pull --ff-only && git -C "$SRC_DIR" submodule update --init --recursive
		fi

		# The embedded web UI needs Node >= 20 + pnpm >= 10; try to provision
		# them, but degrade gracefully (gateway + Open WebUI work without it).
		if ! ensure_frontend_toolchain && command -v apt-get >/dev/null 2>&1; then
			echo "  Node.js >= 20 not found — installing Node 22 from NodeSource (needed for the embedded web UI)..."
			curl -fsSL https://deb.nodesource.com/setup_22.x | bash - || true
			apt-get install -y nodejs || true
		fi
		HAS_FRONTEND=no
		if ensure_frontend_toolchain; then
			HAS_FRONTEND=yes
			echo "  Web UI toolchain ready: Node $(node --version), pnpm $(pnpm --version)."
		else
			echo "  WARN: Node.js >= 20 / pnpm unavailable — building WITHOUT the embedded web UI."
			echo "        sd-server will still serve images (gateway API + Open WebUI), but its own"
			echo "        browser page stays a plain-text placeholder. Install nodejs 20+ and"
			echo "        pnpm 10+, then re-run to embed the UI."
		fi

		# sm_120 (Blackwell, e.g. RTX 5060 Ti) needs nvcc >= 12.8; distro
		# toolchains are often older (Ubuntu 26.04 ships 12.0). Use the best
		# available, bootstrap CUDA 13 from NVIDIA's apt repo if needed, and
		# fall back to an sm_89 SASS+PTX build (the driver JITs it).
		scan_nvcc

		if [ -z "$NVCC" ] && command -v apt-get >/dev/null 2>&1; then
			# shellcheck disable=SC1091
			. /etc/os-release
			if [ "${ID:-}" = "ubuntu" ] && [ -n "${VERSION_ID:-}" ]; then
				DISTRO="${ID}${VERSION_ID//./}"   # e.g. ubuntu2604
				echo "  No nvcc >= 12.8 found (required for sm_120) — installing CUDA 13 toolkit bits from NVIDIA's ${DISTRO} repo..."
				KEYRING_URL="https://developer.download.nvidia.com/compute/cuda/repos/${DISTRO}/x86_64/cuda-keyring_1.1-1_all.deb"
				if curl -fsSL -o /tmp/cuda-keyring.deb "$KEYRING_URL" \
					&& dpkg -i /tmp/cuda-keyring.deb \
					&& apt-get update -qq; then
					apt-get install -y cuda-nvcc-13-0 cuda-cudart-dev-13-0 libcublas-dev-13-0 || true
				else
					echo "  WARN: NVIDIA CUDA repo unreachable for ${DISTRO} — falling back."
				fi
				scan_nvcc
			fi
		fi

		CMAKE_ARGS=(-DSD_CUDA=ON -DSD_CUBLAS=ON -DGGML_CUDA=ON)
		if [ -n "$NVCC" ]; then
			echo "  Using nvcc ${NVCC_VER} (${NVCC}) — native sm_${CUDA_ARCH} build."
			CMAKE_ARGS+=(-DCMAKE_CUDA_COMPILER="${NVCC}" -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCH}")
			export PATH="$(dirname "${NVCC}"):$PATH"
		elif [ -n "$PTX_NVCC" ]; then
			echo "  WARN: best nvcc is ${PTX_VER} (< 12.8) — building sm_89 SASS+PTX instead of native sm_${CUDA_ARCH}."
			echo "        The Blackwell driver JIT-compiles the PTX on first run (works, slightly slower start)."
			echo "        For a native build, install CUDA >= 12.8 (NVIDIA repo: cuda-nvcc-13-0) and re-run."
			CMAKE_ARGS+=(-DCMAKE_CUDA_COMPILER="${PTX_NVCC}" -DCMAKE_CUDA_ARCHITECTURES="89")
			export PATH="$(dirname "${PTX_NVCC}"):$PATH"
		else
			echo "ERROR: no nvcc found on the system. Install CUDA toolkit >= 12.8 and re-run."
			exit 1
		fi

		# SD_CUDA / SD_CUBLAS cover old and new build option names; extras are
		# harmless unused cache entries on whichever naming applies. Wipe any
		# stale build dir first — a failed configure (e.g. unsupported arch)
		# poisons the CMake cache and would fail again even with a fixed
		# toolchain.
		rm -rf "${SRC_DIR}/build"
		if [ "$HAS_FRONTEND" = yes ]; then
			CMAKE_ARGS+=(-DSD_SERVER_BUILD_FRONTEND=ON)
		fi
		cmake -B "${SRC_DIR}/build" -S "$SRC_DIR" "${CMAKE_ARGS[@]}" \
			-DCMAKE_BUILD_TYPE=Release
		cmake --build "${SRC_DIR}/build" --config Release -j"$(nproc)"
		find "${SRC_DIR}/build" -name 'sd-server' -type f -exec cp {} "${BIN_DIR}/sd-server" \;
		# Ground truth for "UI embedded": the generated header from the
		# frontend build. CMake only WARNS ("pnpm not found; frontend build
		# disabled") and happily produces a UI-less binary otherwise.
		if [ "$HAS_FRONTEND" = yes ] \
			&& [ -f "${SRC_DIR}/examples/server/frontend/dist/gen_index_html.h" ]; then
			touch "${FRONTEND_MARKER}"
			echo "  Embedded web UI compiled in."
		else
			rm -f "${FRONTEND_MARKER}"
			echo "  WARN: this binary has NO embedded web UI — / will show a plain placeholder."
			if [ "$HAS_FRONTEND" != yes ]; then
				echo "        Reason: Node.js >= 20 / pnpm unavailable during the build."
			else
				echo "        Reason: frontend build did not produce dist/gen_index_html.h —"
				echo "                check the cmake output above for 'pnpm not found'."
			fi
		fi
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

# hf_resolve <repo> <grep -E pattern> <fallback> prints the exact filename of
# the first file in the repo listing matching the pattern, so renames on
# HuggingFace don't 404 the install. Falls back to the last verified name
# when the API is unreachable or nothing matches.
hf_resolve() {
	local repo="$1" pattern="$2" fallback="$3" resolved=""
	resolved="$(curl -fsSL "https://huggingface.co/api/models/${repo}" 2>/dev/null \
		| grep -oE '"rfilename": *"[^"]+"' \
		| cut -d'"' -f4 \
		| grep -E "${pattern}" | head -n1 || true)"
	echo "${resolved:-${fallback}}"
}

echo "[2/3] Downloading models to ${MODEL_DIR} (resumable)..."

DIFFUSION_NAME="$(hf_resolve 'leejet/Qwen-Image-2.1-GGUF' \
	'qwen_image_2\.1-[^"]*'"${DIFFUSION_QUANT}"'\.gguf' \
	"qwen_image_2.1-${DIFFUSION_QUANT}.gguf")"
fetch_model \
	"https://huggingface.co/leejet/Qwen-Image-2.1-GGUF/resolve/main/${DIFFUSION_NAME}" \
	"${MODEL_DIR}/${DIFFUSION_NAME}"

TE_NAME="$(hf_resolve 'Qwen/Qwen3-VL-8B-Instruct-GGUF' \
	'Qwen3VL-8B-Instruct-Q4_K_M\.gguf' \
	'Qwen3VL-8B-Instruct-Q4_K_M.gguf')"
fetch_model \
	"https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct-GGUF/resolve/main/${TE_NAME}" \
	"${MODEL_DIR}/${TE_NAME}"

MMPROJ_NAME="$(hf_resolve 'Qwen/Qwen3-VL-8B-Instruct-GGUF' \
	'mmproj-[^"]*F16\.gguf' \
	'mmproj-Qwen3VL-8B-Instruct-F16.gguf')"
fetch_model \
	"https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct-GGUF/resolve/main/${MMPROJ_NAME}" \
	"${MODEL_DIR}/${MMPROJ_NAME}"

VAE_NAME="$(hf_resolve 'Comfy-Org/Qwen-Image-2.1' \
	'vae/qwen_image_2\.1_vae_bf16\.safetensors' \
	'vae/qwen_image_2.1_vae_bf16.safetensors')"
fetch_model \
	"https://huggingface.co/Comfy-Org/Qwen-Image-2.1/resolve/main/${VAE_NAME}" \
	"${MODEL_DIR}/$(basename "${VAE_NAME}")"
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
echo "      --llm_vision ${MODEL_DIR}/${MMPROJ_NAME}"
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
echo "editing, --offload-to-cpu keeps the ~13.7 GB of weights in RAM instead of"
echo "the 16 GB VRAM. Max speed at ≤1024 px: drop --offload-to-cpu — with Q6_K"
echo "the weights then fit entirely in VRAM."
