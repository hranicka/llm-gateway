#!/usr/bin/env bash
# Usage: sudo ./scripts/cleanup.sh [--apply] [--caches] [--config PATH]
#
# Finds downloads nothing references any more and (with --apply) deletes them.
# Without --apply it only lists candidates and their sizes.
#
#   - image model files in /opt/sdcpp/models and ComfyUI's diffusion_models,
#     text_encoders and vae dirs whose file name appears neither in the
#     gateway config nor in a saved ComfyUI workflow
#   - interrupted downloads (*.part) and dangling ComfyUI model symlinks
#   - llama.cpp `-hf` downloads (HF hub cache and legacy ~/.cache/llama.cpp)
#     of GGUF repos no `-hf` / `--spec-draft-hf` in the config names
#   - --caches: also clears the uv package cache (re-downloaded on demand)
#
# Comment lines in the config don't count as references. Models used only in
# unsaved ComfyUI graphs look unused — save the workflow first.

set -euo pipefail

CONFIG="/etc/llm-gateway/config.yaml"
APPLY=0
CACHES=0
while [ $# -gt 0 ]; do
	case "$1" in
		--apply) APPLY=1 ;;
		--caches) CACHES=1 ;;
		--config) CONFIG="$2"; shift ;;
		*) echo "Usage: $0 [--apply] [--caches] [--config PATH]"; exit 1 ;;
	esac
	shift
done
[ -f "$CONFIG" ] || { echo "ERROR: config not found: $CONFIG (use --config)"; exit 1; }

SDCPP_MODELS="${SDCPP_MODELS:-/opt/sdcpp/models}"
COMFY="${COMFY_DIR:-/opt/comfyui/ComfyUI}"
COMFY_MODEL_DIRS=("${COMFY}/models/diffusion_models" "${COMFY}/models/text_encoders" "${COMFY}/models/vae")

GATEWAY_USER="$(systemctl show -p User --value llm-gateway 2>/dev/null || true)"
GATEWAY_USER="${GATEWAY_USER:-${SUDO_USER:-$(id -un)}}"
GATEWAY_HOME="${GATEWAY_HOME:-$(getent passwd "$GATEWAY_USER" | cut -d: -f6)}"

REFS="$(mktemp)"
trap 'rm -f "$REFS"' EXIT
grep -v '^[[:space:]]*#' "$CONFIG" > "$REFS"
if [ -d "${COMFY}/user" ]; then
	find "${COMFY}/user" -name '*.json' -path '*workflows*' -exec cat {} + >> "$REFS" 2>/dev/null || true
fi
# Config tokens without ":quant" suffixes, for exact HF repo matches.
HF_REPOS="$(grep -v '^[[:space:]]*#' "$CONFIG" | tr -s ' \t' '\n' | sed 's/:.*//' | grep '/' | sort -u || true)"

TOTAL=0
candidate() {
	local path="$1" size
	size="$(du -sb "$path" 2>/dev/null | cut -f1)"
	TOTAL=$((TOTAL + ${size:-0}))
	printf '  %8s  %s\n' "$(numfmt --to=iec "${size:-0}")" "$path"
	if [ "$APPLY" = 1 ]; then
		rm -rf -- "$path"
	fi
}

echo "Config: ${CONFIG}   gateway user: ${GATEWAY_USER} (${GATEWAY_HOME})"
if [ "$APPLY" = 1 ]; then echo "Mode: DELETE"; else echo "Mode: dry run (add --apply to delete)"; fi

echo
echo "Unreferenced image model files:"
for dir in "$SDCPP_MODELS" "${COMFY_MODEL_DIRS[@]}"; do
	[ -d "$dir" ] || continue
	while IFS= read -r -d '' f; do
		grep -qF "$(basename "$f")" "$REFS" || candidate "$f"
	done < <(find "$dir" -maxdepth 1 -type f \( -name '*.gguf' -o -name '*.safetensors' \) -print0)
done

echo
echo "Interrupted downloads (*.part):"
for dir in "$SDCPP_MODELS" "${COMFY}/models" "${GATEWAY_HOME}/.cache"; do
	[ -d "$dir" ] || continue
	while IFS= read -r -d '' f; do candidate "$f"; done \
		< <(find "$dir" -type f \( -name '*.part' -o -name '*.downloadInProgress' \) -print0)
done

echo
echo "Dangling ComfyUI model symlinks:"
if [ -d "${COMFY}/models" ]; then
	while IFS= read -r -d '' f; do candidate "$f"; done < <(find "${COMFY}/models" -xtype l -print0)
fi

echo
echo "llama.cpp -hf downloads not named in the config:"
HUB="${GATEWAY_HOME}/.cache/huggingface/hub"
if [ -d "$HUB" ]; then
	for d in "$HUB"/models--*; do
		[ -d "$d" ] || continue
		# Only GGUF repos — Open WebUI keeps its embedding models here too.
		find "$d" -name '*.gguf' -print -quit | grep -q . || continue
		name="${d##*/models--}"
		repo="${name%%--*}/${name#*--}"
		grep -qFx "$repo" <<< "$HF_REPOS" || candidate "$d"
	done
fi
LEGACY="${GATEWAY_HOME}/.cache/llama.cpp"
if [ -d "$LEGACY" ]; then
	PREFIXES="$(sed 's#/#_#; s/$/_/' <<< "$HF_REPOS")"
	for f in "$LEGACY"/*.gguf; do
		[ -f "$f" ] || continue
		base="$(basename "$f")"
		used=0
		while IFS= read -r p; do
			[ -n "$p" ] && [[ "$base" == "$p"* ]] && { used=1; break; }
		done <<< "$PREFIXES"
		if [ "$used" = 0 ]; then
			for side in "$f" "$f".*; do [ -e "$side" ] && candidate "$side"; done
		fi
	done
fi

if [ "$CACHES" = 1 ]; then
	echo
	echo "Package caches:"
	if command -v uv >/dev/null 2>&1; then
		UV_DIR="$(uv cache dir 2>/dev/null || true)"
		if [ -n "$UV_DIR" ] && [ -d "$UV_DIR" ]; then
			size="$(du -sb "$UV_DIR" | cut -f1)"
			TOTAL=$((TOTAL + size))
			printf '  %8s  %s\n' "$(numfmt --to=iec "$size")" "$UV_DIR"
			if [ "$APPLY" = 1 ]; then uv cache clean >/dev/null; fi
		fi
	fi
fi

echo
if [ "$APPLY" = 1 ]; then
	echo "Freed $(numfmt --to=iec "$TOTAL")."
else
	echo "Would free $(numfmt --to=iec "$TOTAL"). Re-run with --apply to delete."
fi
