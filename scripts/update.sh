#!/usr/bin/env bash
# Usage: ./scripts/update.sh [llama] [sdcpp] [comfyui] [openwebui]
#
# Updates the installed backends and apps to their latest upstream versions
# by re-running their installers in "latest" mode. Without arguments it
# updates everything that is installed. Run as your normal user — it calls
# sudo where an installer needs root.
#
#   llama      llama.cpp master, rebuilt for the hardware chosen at install
#              (remembered in ~/.config/llm-gateway/llama-hw-choice)
#   sdcpp      newest stable-diffusion.cpp release tag (rebuilds only if newer)
#   comfyui    newest ComfyUI release + ComfyUI-GGUF default branch
#   openwebui  newest open-webui from PyPI, service restarted
#
# Running backends are stopped; the gateway starts the new version on the
# next request. Model files are kept. Old downloads: scripts/cleanup.sh.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "$(id -u)" -eq 0 ]; then
	echo "ERROR: run as your normal user (llama.cpp builds in your home); sudo is used where needed."
	exit 1
fi

installed() {
	case "$1" in
		llama) command -v llama-server >/dev/null 2>&1 ;;
		sdcpp) [ -x /opt/sdcpp/bin/sd-server ] ;;
		comfyui) [ -d /opt/comfyui/ComfyUI ] ;;
		openwebui) [ -d /opt/open-webui/venv ] ;;
	esac
}

update() {
	case "$1" in
		llama)
			local hw_file="$HOME/.config/llm-gateway/llama-hw-choice"
			if [ -z "${HW_CHOICE:-}" ] && [ -s "$hw_file" ]; then
				HW_CHOICE="$(cat "$hw_file")"
			fi
			HW_CHOICE="${HW_CHOICE:-}" "${SCRIPT_DIR}/install-llama.sh"
			;;
		sdcpp) sudo env SDCPP_RELEASE=latest "${SCRIPT_DIR}/install-qwen-image-sdcpp.sh" ;;
		comfyui) sudo env COMFY_REF=latest GGUF_NODE_REF=latest "${SCRIPT_DIR}/install-comfyui.sh" ;;
		openwebui) sudo env OPEN_WEBUI_VERSION=latest "${SCRIPT_DIR}/install-open-webui.sh" ;;
	esac
}

COMPONENTS=("$@")
for c in "${COMPONENTS[@]}"; do
	case "$c" in
		llama|sdcpp|comfyui|openwebui) ;;
		*) echo "Unknown component: $c (llama, sdcpp, comfyui, openwebui)"; exit 1 ;;
	esac
done
if [ ${#COMPONENTS[@]} -eq 0 ]; then
	for c in llama sdcpp comfyui openwebui; do
		installed "$c" && COMPONENTS+=("$c")
	done
fi
[ ${#COMPONENTS[@]} -gt 0 ] || { echo "Nothing installed to update."; exit 0; }

sudo -v || exit 1
FAILED=()
for c in "${COMPONENTS[@]}"; do
	if ! installed "$c"; then
		echo "Skipping ${c}: not installed (run its installer first)."
		continue
	fi
	echo
	echo "################ Updating ${c} ################"
	update "$c" || FAILED+=("$c")
done

echo
if [ ${#FAILED[@]} -gt 0 ]; then
	echo "Failed: ${FAILED[*]} — see the output above."
	exit 1
fi
echo "Updated: ${COMPONENTS[*]}."
