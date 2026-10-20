#!/usr/bin/env bash
# Usage: ./scripts/client/install-claude.sh
#
# Syncs this repo's config/claude into the Claude Code config directory
# (~/.claude, or CLAUDE_CONFIG_DIR):
#   CLAUDE.md        user-level instructions (delegation + advisor rules); the old
#                    file is kept as CLAUDE.md.bak-<timestamp> when it differs
#   agents/*.md      subagents (scout, implementer, implementer-pro); other agents
#                    already in the directory are left alone
#   settings.json    only the keys in config/claude/settings.json (model, effort,
#                    advisor model, subagent model env) are merged into the live
#                    file with jq; every other key stays
#
# Only these files are managed. Credentials (.credentials.json, .claude.json),
# sessions, history and plugins are never read or copied — keep it that way: this
# repo must not contain secrets.
#
# Run as your normal user. Re-running is safe.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${SCRIPT_DIR}/../../config/claude"
DEST="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
STAMP="$(date +%Y%m%d%H%M%S)"

if [ "$(id -u)" -eq 0 ]; then
	echo "ERROR: run as your normal user; Claude Code config lives in your home directory."
	exit 1
fi
command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required (sudo apt install jq)."; exit 1; }

mkdir -p "$DEST/agents"

if [ -f "$DEST/CLAUDE.md" ] && ! cmp -s "$SRC/CLAUDE.md" "$DEST/CLAUDE.md"; then
	cp "$DEST/CLAUDE.md" "$DEST/CLAUDE.md.bak-${STAMP}"
	echo "previous CLAUDE.md kept as CLAUDE.md.bak-${STAMP}"
fi
cp "$SRC/CLAUDE.md" "$DEST/CLAUDE.md"

for f in "$SRC"/agents/*.md; do
	name="$(basename "$f")"
	if [ -f "$DEST/agents/$name" ] && ! cmp -s "$f" "$DEST/agents/$name"; then
		cp "$DEST/agents/$name" "$DEST/agents/$name.bak-${STAMP}"
		echo "previous agents/$name kept as agents/$name.bak-${STAMP}"
	fi
	cp "$f" "$DEST/agents/$name"
done

LIVE="$DEST/settings.json"
if [ -s "$LIVE" ]; then
	cp "$LIVE" "$LIVE.bak-${STAMP}"
else
	echo '{}' > "$LIVE"
fi
jq -s '.[0] * .[1]' "$LIVE" "$SRC/settings.json" > "$LIVE.new"
mv "$LIVE.new" "$LIVE"

echo "Claude Code config synced into ${DEST}: CLAUDE.md, agents/ ($(ls "$SRC"/agents | wc -l) files), settings.json keys $(jq -r 'keys | join(", ")' "$SRC/settings.json")."
