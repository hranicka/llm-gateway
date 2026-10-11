#!/usr/bin/env bash
# Usage: sudo ./scripts/install-exllamav3.sh
#
# Installs an ExLlamaV3 (EXL3) chat backend for NVIDIA GPUs as a
# gateway-managed model (kind: api) on 127.0.0.1:1235:
#   - ExLlamaV3 engine (prebuilt release wheel) + CUDA torch in /opt/exllamav3/venv
#   - the OpenAI-compatible server from MiaAI-Lab's Qwen3.8-27B kit
#     (serve_openai.py + token_budget.py, MIT) in /opt/exllamav3/server,
#     patched by patches/exl3-serve-openai.patch
#   - turboderp's Qwen3.8-27B EXL3 quant in /opt/exllamav3/models
#
# The server speaks /v1/chat/completions, /v1/models and /health only — no
# Anthropic /v1/messages or /v1/responses; clients using those stay on the
# llama-server models.
#
# Everything is pinned (engine wheel, kit commit, model revision) and
# checksum-verified where the upstream offers a stable artifact. Bump the
# constants below deliberately and re-run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR="/opt/exllamav3"
PYVER="3.12"

ENGINE_VERSION="1.6.0"
TORCH_VERSION="2.11.0"
ENGINE_WHEEL="exllamav3-${ENGINE_VERSION}%2Bcu128.torch${TORCH_VERSION}-cp312-cp312-linux_x86_64.whl"
ENGINE_WHEEL_URL="https://github.com/turboderp-org/exllamav3/releases/download/v${ENGINE_VERSION}/${ENGINE_WHEEL}"
ENGINE_WHEEL_SHA256="d6efeced13ecdf6fab64811a80635c403c04ff05aa899d8e32942610daa41b19"

KIT_REF="bbb11db4b7ff688a62841c68e0892fa099bc3362"
KIT_RAW="https://raw.githubusercontent.com/MiaAI-Lab/Qwen3.8-27B-16gb-NVIDIA-GPUs-one-click-install/${KIT_REF}/tools"
SERVE_SHA256="c082eb9af9c2eec2e3ecbbbfc010d9e0b664e310d07f75db4f60863e5eac01f5"
BUDGET_SHA256="e36de1e8392645ede97980c63d523d592ca82f9c4e36d4264d53f1e241f5bfb4"

MODEL_REPO="turboderp/Qwen3.8-27B-exl3"
MODEL_REVISION="6fe61ad620abfe97c5b49f9722c2bceeea4ccc28" # branch 3.00bpw
MODEL_DIR="${INSTALL_DIR}/models/Qwen3.8-27B-EXL3-3.00bpw"

if [ "$(id -u)" -ne 0 ]; then
	echo "ERROR: this script must be run as root."
	echo "       Usage: sudo $(basename "$0")"
	exit 1
fi

RUN_USER="${SUDO_USER:-$(id -nu 1000 2>/dev/null || echo root)}"

echo "======================================================"
echo " ExLlamaV3 — Qwen3.8-27B EXL3 3.0 bpw chat + vision backend"
echo "------------------------------------------------------"
echo " install: ${INSTALL_DIR} (venv + server + model, ~21 GB)"
echo " engine:  exllamav3 ${ENGINE_VERSION}, torch ${TORCH_VERSION}+cu128"
echo " model:   ${MODEL_REPO} @ 3.00bpw (13.8 GB)"
echo " runs as: ${RUN_USER}, managed by llm-gateway (kind: api)"
echo "======================================================"
echo

# ── 0. Prerequisites ──────────────────────────────────────────────────────────
if ! nvidia-smi &>/dev/null; then
	echo "ERROR: no NVIDIA driver (nvidia-smi)."
	exit 1
fi
for dep in curl patch; do
	command -v "$dep" &>/dev/null || { echo "ERROR: '$dep' is required."; exit 1; }
done
mkdir -p "${INSTALL_DIR}/python" "${INSTALL_DIR}/server" "${INSTALL_DIR}/models"

# ── 1. uv + Python (kept inside ${INSTALL_DIR} — see install-open-webui.sh) ──
UV_VERSION="0.12.17"
UV_SHA256="fa82fd8dde8e8eefdecada6aa0889666556cfceb690d06e0c3bca49eb3070a63" # uv-x86_64-unknown-linux-gnu.tar.gz
install_uv() {
	if command -v uv &>/dev/null; then
		echo "[1/5] uv found: $(uv --version)"
		return 0
	fi
	echo "[1/5] Installing pinned uv ${UV_VERSION}..."
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

# ── 2. torch + engine wheel (exact match: the wheel is built per torch version) ──
echo "[2/5] Installing torch ${TORCH_VERSION}+cu128 and exllamav3 ${ENGINE_VERSION} (~4 GB)..."
export VIRTUAL_ENV="${INSTALL_DIR}/venv"
uv pip install "torch==${TORCH_VERSION}" --index-url https://download.pytorch.org/whl/cu128
WHEEL_TMP="$(mktemp -d)"
WHEEL_FILE="${WHEEL_TMP}/${ENGINE_WHEEL//%2B/+}"
if ! curl -fL --retry 3 -o "$WHEEL_FILE" "$ENGINE_WHEEL_URL" \
	|| ! echo "${ENGINE_WHEEL_SHA256}  ${WHEEL_FILE}" | sha256sum -c --status -; then
	echo "ERROR: engine wheel download or checksum failed."
	rm -rf "$WHEEL_TMP"
	exit 1
fi
uv pip install "$WHEEL_FILE"
rm -rf "$WHEEL_TMP"
uv pip install "aiohttp==3.14.5" "huggingface_hub==2.2.0" "pillow==12.3.0" "transformers==5.19.0"

# ── 3. Server (two files of the kit, pinned and checksum-verified) ───────────
echo "[3/5] Fetching the OpenAI-compatible server (kit ${KIT_REF:0:7})..."
fetch_verified() {
	local name="$1" sha="$2" out="${INSTALL_DIR}/server/$1"
	curl -fsSL --retry 3 -o "${out}.part" "${KIT_RAW}/${name}"
	if ! echo "${sha}  ${out}.part" | sha256sum -c --status -; then
		echo "ERROR: checksum mismatch for ${name}."
		rm -f "${out}.part"
		exit 1
	fi
	mv -f "${out}.part" "$out"
}
fetch_verified serve_openai.py "$SERVE_SHA256"
fetch_verified token_budget.py "$BUDGET_SHA256"
# Honour chat_template_kwargs.reasoning_effort (how the bundled clients send
# it), default to medium like llama-server (the template's default is xhigh),
# and accept only data: image URLs (no server-side fetches).
patch -s -d "${INSTALL_DIR}/server" -p1 < "${SCRIPT_DIR}/../patches/exl3-serve-openai.patch"

# ── 4. Model (resumable; --local-dir keeps it out of the HF hub cache) ───────
echo "[4/5] Downloading ${MODEL_REPO} 3.00bpw (13.8 GB, resumable)..."
"${INSTALL_DIR}/venv/bin/hf" download "$MODEL_REPO" --revision "$MODEL_REVISION" --local-dir "$MODEL_DIR"

chown -R "$RUN_USER" "$INSTALL_DIR"

# A running server keeps the old code; the gateway restarts it on the next request.
if pkill -f "${INSTALL_DIR}/server/serve_openai.py" 2>/dev/null; then
	echo "  Stopped the running EXL3 server — the gateway starts the updated one on the next request."
fi

# ── 5. Done — print the gateway config snippet ────────────────────────────────
echo "[5/5] Done. Gateway config (16 GB card; see config/gem12gpu.yaml):"
echo
echo "  qwen-3.8-27b-exl3:"
echo "    command: |"
echo "      ${INSTALL_DIR}/venv/bin/python -u ${INSTALL_DIR}/server/serve_openai.py"
echo "      --model ${MODEL_DIR} --model_id qwen-3.8-27b-exl3"
echo "      --cache_size 131072 --grid_size 15.2 --cache_quant 4"
echo "      --draft_model mtp --vision auto --ui off --progress_every 0"
echo "      --host 127.0.0.1 --port 1235"
echo "    host: 127.0.0.1:1235"
echo "    ready_timeout: 30m"
echo
echo " Then: sync the gateway config + 'sudo systemctl restart llm-gateway'."
