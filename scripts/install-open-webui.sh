#!/usr/bin/env bash
# Usage: sudo ./scripts/install-open-webui.sh
#
# Installs Open WebUI — a self-hosted ChatGPT-style front-end (chat history,
# accounts, streaming) — as its own systemd service on port 8080.
#
# Open WebUI is NOT managed by llm-gateway: it runs always-on, independent of
# model switching. It merely talks to the gateway's OpenAI-compatible API:
#   - chat models      → http://127.0.0.1:1234/v1  (llama-server models,
#                        loaded/killed on demand by the gateway)
#   - image generation → http://127.0.0.1:1234/v1/images/generations
#                        with model qwen-image-2.1 (sd-server)
# Image editing (reference images) stays in sd-server's own UI:
#   http://<host>:1234/app/qwen-image-2.1
#
# Port override: OPEN_WEBUI_PORT=8081 sudo -E ./scripts/install-open-webui.sh
# Upgrade:      re-run the script (upgrades the package in place).

set -euo pipefail

VENV_DIR="/opt/open-webui"
PYVER="3.11"
GATEWAY_API="${GATEWAY_API:-http://127.0.0.1:1234/v1}"
IMAGE_MODEL="${IMAGE_MODEL:-qwen-image-2.1}"
PORT="${OPEN_WEBUI_PORT:-8080}"
SERVICE_FILE="/etc/systemd/system/open-webui.service"

if [ "$(id -u)" -ne 0 ]; then
	echo "ERROR: this script must be run as root."
	echo "       Usage: sudo $(basename "$0")"
	exit 1
fi

# Run the service as the user who invoked sudo (same convention as the
# llm-gateway installer) so caches live in the right home directory.
RUN_USER="${SUDO_USER:-$(id -nu 1000)}"
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
if [ -z "$RUN_HOME" ]; then
	echo "ERROR: cannot resolve home directory for user ${RUN_USER}."
	exit 1
fi

echo "======================================================"
echo " Open WebUI — ChatGPT-style UI on top of llm-gateway"
echo "------------------------------------------------------"
echo " venv:   ${VENV_DIR}/venv (uv-managed Python ${PYVER})"
echo " data:   ${VENV_DIR}/data (persists chats/accounts)"
echo " API:    ${GATEWAY_API}  (models via llm-gateway)"
echo " images: model ${IMAGE_MODEL} via the same gateway"
echo " UI:     http://<host>:${PORT}"
echo "======================================================"
echo

# ── 1. uv ─────────────────────────────────────────────────────────────────────
if ! command -v uv &>/dev/null; then
	echo "[1/4] Installing uv..."
	curl -LsSf https://astral.sh/uv/install.sh | sh -s --
	export UV_NO_MODIFY_PATH=1
	export PATH="$HOME/.local/bin:$PATH"
else
	echo "[1/4] uv found: $(uv --version)"
fi

# ── 2. venv + package ─────────────────────────────────────────────────────────
echo "[2/4] Installing Open WebUI into ${VENV_DIR}/venv (CPU torch, ~2 GB)..."
mkdir -p "${VENV_DIR}/venv" "${VENV_DIR}/data" "${VENV_DIR}/python"
# Keep the uv-managed interpreter INSIDE ${VENV_DIR}: uv otherwise installs it
# under the invoking user's home (root, when run via sudo), and the service
# runs as a normal user who cannot traverse /root — the venv's interpreter
# symlink then breaks and systemd reports an opaque 203/EXEC loop.
export UV_PYTHON_INSTALL_DIR="${VENV_DIR}/python"
if ! uv python list 2>/dev/null | grep -q "$PYVER"; then
	uv python install "$PYVER"
fi

# Repair path: an older run may have linked the venv into a home directory.
VENV_PY="${VENV_DIR}/venv/bin/python3"
if [ -L "$VENV_PY" ]; then
	case "$(readlink -f "$VENV_PY")" in
		"${VENV_DIR}"/*) ;;
		*)
			echo "  Existing venv links outside ${VENV_DIR} ($(readlink -f "$VENV_PY")) — recreating it."
			rm -rf "${VENV_DIR:?}/venv"
			;;
	esac
fi

if [ ! -e "${VENV_DIR}/venv/bin/activate" ]; then
	uv venv "${VENV_DIR}/venv" --python "$PYVER"
fi
source "${VENV_DIR}/venv/bin/activate"
# CPU torch is enough: the GPU work happens in the gateway's backends, and
# Open WebUI only uses torch for local RAG embeddings.
uv pip install -U open-webui --torch-backend=cpu

[ -x "${VENV_DIR}/venv/bin/open-webui" ] || { echo "ERROR: open-webui not found in venv."; exit 1; }
chown -R "$RUN_USER" "$VENV_DIR"

# Prove the service user can actually run it — catches inaccessible
# interpreters here instead of as an opaque systemd 203/EXEC restart loop.
OWUI_VER="$(sudo -u "$RUN_USER" "${VENV_DIR}/venv/bin/open-webui" --help 2>&1)" || {
	echo "$OWUI_VER"
	echo "ERROR: ${RUN_USER} cannot execute ${VENV_DIR}/venv/bin/open-webui — check interpreter links and permissions."
	exit 1
}
echo "  Verified: ${RUN_USER} can run the venv's open-webui."

# ── 3. systemd service ────────────────────────────────────────────────────────
echo "[3/4] Installing systemd service (runs as ${RUN_USER})..."
# WEBUI_SECRET_KEY is a hard requirement once auth is enabled; without it some
# versions fall back to writing /.webui_secret_key and crash with
# PermissionError when running as a non-root service user. Generate once and
# persist across upgrades.
SECRET_FILE="${VENV_DIR}/data/.webui_secret_key"
if [ ! -s "${SECRET_FILE}" ]; then
	umask 077
	head -c 48 /dev/urandom | base64 | tr -d '\n' > "${SECRET_FILE}"
	umask 022
fi
WEBUI_SECRET="$(cat "${SECRET_FILE}")"
chmod 600 "${SECRET_FILE}"

# Wait indefinitely for local models: the gateway may spend minutes loading
# weights before the first token arrives. AIOHTTP_CLIENT_TIMEOUT=0 disables
# the total request timeout (and future Open WebUI versions default it to
# 300s), the stream idle cap is disabled so a slow-loading model can't kill
# a pending stream, and the model-list probe gets a generous minute.
HTTP_TIMEOUT_ENV=$(cat <<'EOF'
Environment=AIOHTTP_CLIENT_TIMEOUT=0
Environment=AIOHTTP_CLIENT_STREAM_IDLE_TIMEOUT=0
Environment=AIOHTTP_CLIENT_TIMEOUT_MODEL_LIST=60
EOF
)

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Open WebUI — ChatGPT-style front-end for llm-gateway
# Pure ordering: start after the gateway when it exists; Open WebUI works
# (and retries per request) even without it.
After=network-online.target llm-gateway.service
Wants=network-online.target

[Service]
Type=simple
User=${RUN_USER}
Environment=DATA_DIR=${VENV_DIR}/data
Environment=HF_HOME=${RUN_HOME}/.cache/huggingface
Environment=OPENAI_API_BASE_URL=${GATEWAY_API}
Environment=OPENAI_API_KEY=sk-llm-gateway
Environment=WEBUI_SECRET_KEY=${WEBUI_SECRET}
Environment=ENABLE_OLLAMA_API=false
Environment=ENABLE_IMAGE_GENERATION=true
Environment=IMAGE_GENERATION_ENGINE=openai
Environment=IMAGE_GENERATION_API_BASE_URL=${GATEWAY_API}
Environment=IMAGE_GENERATION_API_KEY=sk-llm-gateway
Environment=IMAGE_GENERATION_MODEL=${IMAGE_MODEL}
${HTTP_TIMEOUT_ENV}
ExecStart=${VENV_DIR}/venv/bin/open-webui serve --host 0.0.0.0 --port ${PORT}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now open-webui.service

# ── 4. Done ───────────────────────────────────────────────────────────────────
echo "[4/4] Service status:"
systemctl --no-pager --lines=3 status open-webui.service || true
echo
echo "Done!"
echo
echo " Open the UI at:  http://<host>:${PORT}"
echo " The first account you create becomes the admin."
echo
echo " Chat:    model picker lists every gateway model (qwen-3.8-27b, gemma-4,"
echo "          ...); the gateway loads/switches them on demand."
echo " Images:  in a chat, the image button (or /image <prompt>) generates via"
echo "          ${IMAGE_MODEL}. Set the resolution in Admin Settings → Images"
echo "          (width/height in multiples of 32, e.g. 1024x1024 or 2048x2048)."
echo " Editing: reference-image editing lives in sd-server's own UI:"
echo "          http://<host>:1234/app/qwen-image-2.1"
echo
echo " Manage:  systemctl {status|restart|stop} open-webui"
echo " Logs:    journalctl -u open-webui -f"
echo " Upgrade: sudo $(basename "$0")   (re-run)"
echo " Remove:  sudo systemctl disable --now open-webui &&"
echo "          sudo rm -f ${SERVICE_FILE} && sudo rm -rf ${VENV_DIR} && sudo systemctl daemon-reload"

# Common gotcha: ufw active but the UI port closed — the browser just times
# out while the service is perfectly healthy on the host.
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
	if ! ufw status 2>/dev/null | grep -qE "^${PORT}/tcp +ALLOW"; then
		echo
		echo " NOTE: ufw is active but port ${PORT} is not open — the UI will time out from other"
		echo "       machines until you run:  sudo ufw allow ${PORT}/tcp"
	fi
fi
