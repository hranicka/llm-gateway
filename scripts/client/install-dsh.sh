#!/usr/bin/env bash
# Usage: ./scripts/client/install-dsh.sh
#
# Installs the `dsh` launcher (DeepSeek Harness browser UI on :3080, gem12
# agent preset for the single-slot backend) as a symlink to scripts/client/dsh
# in ~/.local/bin, so `dsh update` can mirror the regenerated profile patch back
# into config/dsh. The harness itself runs through npx (@deepseek-ai/dsh), so
# there is nothing else to install; Node must be available (nvm is picked up
# by the launcher).
#
# The live profile patch (~/.dsh/profiles/web/cordis.patch.yml) is the source
# of truth — dsh's settings UI rewrites it — so it is seeded from
# config/dsh only when missing.
#
# BIN_DIR=/path  target directory for the symlink (default ~/.local/bin)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
PATCH_LIVE="$HOME/.dsh/profiles/web/cordis.patch.yml"
PATCH_REPO="${REPO_DIR}/config/dsh/profiles/web/cordis.patch.yml"

if [ "$(id -u)" -eq 0 ]; then
	echo "ERROR: run as your normal user; dsh lives in your home directory."
	exit 1
fi

chmod +x "${SCRIPT_DIR}/dsh"
mkdir -p "$BIN_DIR"
ln -sfn "${SCRIPT_DIR}/dsh" "${BIN_DIR}/dsh"
echo "dsh -> ${SCRIPT_DIR}/dsh (${BIN_DIR}/dsh)"

if [ ! -f "$PATCH_LIVE" ]; then
	mkdir -p "$(dirname "$PATCH_LIVE")"
	cp "$PATCH_REPO" "$PATCH_LIVE"
	echo "seeded ${PATCH_LIVE} from the repo"
fi

case ":$PATH:" in
	*":${BIN_DIR}:"*) ;;
	*) echo "NOTE: ${BIN_DIR} is not on PATH." ;;
esac
other="$(type -ap dsh 2>/dev/null | grep -vxF "${BIN_DIR}/dsh" | head -1 || true)"
[ -z "$other" ] || echo "NOTE: another dsh exists at ${other}; ${BIN_DIR} must come first on PATH."
echo "Start with: dsh   (UI on http://127.0.0.1:3080) — refresh with: dsh update"
