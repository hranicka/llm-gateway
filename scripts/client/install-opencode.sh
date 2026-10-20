#!/usr/bin/env bash
# Usage: ./scripts/client/install-opencode.sh
#
# Copies config/opencode/opencode.json (providers, models, agents, LSP) to
# ~/.config/opencode/opencode.json (OPENCODE_CONFIG_DIR overrides the directory).
# A differing live file is kept as opencode.json.bak-<timestamp>. The repo copy
# holds no credentials (the gateway is keyless); put any key in the live file's
# environment, not here. Re-running is safe.

set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../../config/opencode/opencode.json"
DEST_DIR="${OPENCODE_CONFIG_DIR:-$HOME/.config/opencode}"
DEST="$DEST_DIR/opencode.json"

mkdir -p "$DEST_DIR"
if [ -f "$DEST" ] && ! cmp -s "$SRC" "$DEST"; then
	cp "$DEST" "$DEST.bak-$(date +%Y%m%d%H%M%S)"
	echo "previous opencode.json kept as a .bak-<timestamp> copy"
fi
cp "$SRC" "$DEST"
echo "opencode config synced into ${DEST_DIR}"
