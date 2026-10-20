#!/usr/bin/env bash
# Usage: sudo ./scripts/server/cleanup.sh [--apply] [--caches] [--build] [--backends] [--config PATH]
#
# Finds downloads nothing references any more and (with --apply) deletes them.
# Without --apply it only lists candidates and their sizes.
#
#   - image model files in /opt/sdcpp/models and ComfyUI's diffusion_models,
#     text_encoders and vae dirs whose file name appears neither in the
#     gateway config nor in a saved ComfyUI workflow
#   - EXL3 model dirs in /opt/exllamav3/models the config doesn't name
#   - interrupted downloads (*.part, *.incomplete) and dangling ComfyUI
#     model symlinks
#   - llama.cpp `-hf` downloads (HF hub cache and legacy ~/.cache/llama.cpp)
#     of GGUF repos no `-hf` / `--spec-draft-hf` in the config names, with
#     the shared blobs (hub/blobs/xx/<sha>) only they point to
#   - --caches: uv package caches (root's and the gateway user's), Hugging
#     Face Xet chunk caches, ccache and vLLM's compile cache (all re-created
#     on demand)
#   - --build: the llama.cpp source/build tree (~/llama_build; install-llama.sh
#     re-clones it on every run, llama-server does not use it)
#   - --backends: /opt/vllm and /opt/exllamav3 when no config entry runs them
#
# Sync the gateway config before running it after an install — a model the
# config doesn't name yet counts as unused.
# Comment lines in the config don't count as references. Models used only in
# unsaved ComfyUI graphs look unused — save the workflow first.

set -euo pipefail

CONFIG="/etc/llm-gateway/config.yaml"
APPLY=0
CACHES=0
BUILD=0
BACKENDS=0
while [ $# -gt 0 ]; do
	case "$1" in
		--apply) APPLY=1 ;;
		--caches) CACHES=1 ;;
		--build) BUILD=1 ;;
		--backends) BACKENDS=1 ;;
		--config) CONFIG="$2"; shift ;;
		*) echo "Usage: $0 [--apply] [--caches] [--build] [--backends] [--config PATH]"; exit 1 ;;
	esac
	shift
done
[ -f "$CONFIG" ] || { echo "ERROR: config not found: $CONFIG (use --config)"; exit 1; }

SDCPP_MODELS="${SDCPP_MODELS:-/opt/sdcpp/models}"
EXL3_MODELS="${EXL3_MODELS:-/opt/exllamav3/models}"
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
echo "Unreferenced EXL3 models:"
if [ -d "$EXL3_MODELS" ]; then
	while IFS= read -r -d '' d; do
		grep -qF "$(basename "$d")" "$REFS" || candidate "$d"
	done < <(find "$EXL3_MODELS" -mindepth 1 -maxdepth 1 -type d -print0)
fi

echo
echo "Interrupted downloads (*.part, *.incomplete):"
for dir in "$SDCPP_MODELS" "${COMFY}/models" "$EXL3_MODELS" "${GATEWAY_HOME}/.cache"; do
	[ -d "$dir" ] || continue
	while IFS= read -r -d '' f; do candidate "$f"; done \
		< <(find "$dir" -type f \( -name '*.part' -o -name '*.incomplete' -o -name '*.downloadInProgress' \) -print0)
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
	DROP=()
	KEEP=()
	for d in "$HUB"/models--*; do
		[ -d "$d" ] || continue
		name="${d##*/models--}"
		repo="${name%%--*}/${name#*--}"
		# Only GGUF repos — Open WebUI keeps its embedding models here too.
		if find "$d" -name '*.gguf' -print -quit | grep -q . && ! grep -qFx "$repo" <<< "$HF_REPOS"; then
			DROP+=("$d")
		else
			KEEP+=("$d")
		fi
	done
	# Newer huggingface_hub keeps the data in hub/blobs/xx/<sha> and only
	# symlinks to it from models--*/blobs; such a blob goes once no kept repo
	# links to it.
	KEPT_BLOBS="$( { for k in "${KEEP[@]}"; do find "$k" -type l -printf '%l\n'; done; } 2>/dev/null | sed 's#.*/##' | sort -u)"
	for d in "${DROP[@]}"; do
		shared=()
		while IFS= read -r -d '' l; do
			t="$(readlink -f "$l")"
			case "$t" in "$HUB/blobs/"*) ;; *) continue ;; esac
			grep -qxF "${t##*/}" <<< "$KEPT_BLOBS" || shared+=("$t")
		done < <(find "$d" -type l -print0)
		candidate "$d"
		for t in $(printf '%s\n' "${shared[@]}" | sort -u); do
			[ -e "$t" ] && candidate "$t"
			[ -e "$t.refs" ] && candidate "$t.refs"
		done
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
	echo "Caches:"
	UV_ROOT="$(uv cache dir 2>/dev/null || true)"
	while IFS= read -r c; do
		[ -n "$c" ] && [ -d "$c" ] && candidate "$c"
	done < <(printf '%s\n' "$UV_ROOT" "${GATEWAY_HOME}/.cache/uv" \
		"${GATEWAY_HOME}/.cache/huggingface/xet" "${HOME}/.cache/huggingface/xet" \
		"${GATEWAY_HOME}/.cache/ccache" "${GATEWAY_HOME}/.cache/vllm" | sort -u)
fi

if [ "$BUILD" = 1 ]; then
	echo
	echo "Build trees:"
	[ -d "${GATEWAY_HOME}/llama_build" ] && candidate "${GATEWAY_HOME}/llama_build"
fi

if [ "$BACKENDS" = 1 ]; then
	echo
	echo "Backends no config entry runs:"
	if [ -d /opt/vllm ] && ! grep -qw vllm "$REFS"; then
		candidate /opt/vllm
		# install-vllm-globally.sh's wrapper script and python symlink.
		for f in /usr/local/bin/vllm /usr/local/bin/python3-vllm; do
			if [ -L "$f" ] || grep -qs /opt/vllm "$f"; then candidate "$f"; fi
		done
	fi
	if [ -d /opt/exllamav3 ] && ! grep -qF /opt/exllamav3 "$REFS"; then
		candidate /opt/exllamav3
	fi
fi

echo
if [ "$APPLY" = 1 ]; then
	echo "Freed $(numfmt --to=iec "$TOTAL")."
else
	echo "Would free $(numfmt --to=iec "$TOTAL"). Re-run with --apply to delete."
fi
