#!/usr/bin/env bash
# Usage: ./scripts/client/install-pi.sh
#
# Installs Pi (the `pi` coding agent, https://pi.dev) and syncs this repo's
# config/pi/agent into the agent directory (~/.pi/agent):
#   models.json      gateway + laptop providers and models (copied; the old
#                    file is kept as models.json.bak-<timestamp> when it differs)
#   settings.json    merged into the existing file with jq (Pi's own keys such as
#                    lastChangelogVersion survive; `skills` paths are unioned)
#   extensions/      subagent (one at a time), memory, restore (each directory is
#                    replaced, so files removed from the repo disappear)
#   agents/ prompts/ subagent roles (scout, planner, reviewer, worker) and the
#                    /implement, /scout-and-plan, /implement-and-review chains
#
# Run as your normal user. Re-running is safe and is the way to sync after
# editing the repo files. It never starts a model.
#
# PI_UPDATE=1          also run `pi update` (re-run: PI_UPDATE=1 ./scripts/client/install-pi.sh)
# PI_SKILL_DIRS=a:b    extra skill directories to register in settings.json
#                      (added to the `skills` list, never removed by a re-run)
# PI_CODING_AGENT_DIR  target agent directory (default ~/.pi/agent)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${SCRIPT_DIR}/../../config/pi/agent"
DEST="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"
STAMP="$(date +%Y%m%d%H%M%S)"

if [ "$(id -u)" -eq 0 ]; then
	echo "ERROR: run as your normal user; Pi lives in your home directory."
	exit 1
fi
command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required (sudo apt install jq)."; exit 1; }

echo "======================================================"
echo " Pi — coding agent on top of llm-gateway"
echo "------------------------------------------------------"
echo " config: ${DEST}"
echo "======================================================"

# [1/4] Pi itself. The official installer pins its dependencies and brings
# its own Node.js; `pi update` upgrades an existing install.
echo "[1/4] Installing Pi..."
if ! command -v pi >/dev/null 2>&1; then
	curl -fsSL https://pi.dev/install.sh | sh
	hash -r
	command -v pi >/dev/null 2>&1 || export PATH="$HOME/.local/bin:$PATH"
	command -v pi >/dev/null 2>&1 || { echo "ERROR: pi is not on PATH after the install."; exit 1; }
elif [ "${PI_UPDATE:-0}" = "1" ]; then
	pi update
else
	echo "  pi $(pi --version 2>&1 | head -1) already installed (PI_UPDATE=1 upgrades it)."
fi
mkdir -p "$DEST"

# [2/4] Models.
echo "[2/4] Syncing models.json..."
if [ -f "$DEST/models.json" ] && ! cmp -s "$SRC/models.json" "$DEST/models.json"; then
	cp "$DEST/models.json" "$DEST/models.json.bak-${STAMP}"
	echo "  previous file kept as models.json.bak-${STAMP}"
fi
cp "$SRC/models.json" "$DEST/models.json"

# [3/4] Settings: repo values win, everything else in the live file is kept.
echo "[3/4] Merging settings.json..."
SKILL_DIRS="${PI_SKILL_DIRS:-}"
LIVE="$DEST/settings.json"
if [ -s "$LIVE" ]; then
	cp "$LIVE" "$LIVE.bak-${STAMP}"
else
	echo '{}' > "$LIVE"
fi
jq -s --arg dirs "$SKILL_DIRS" '
	.[0] as $live | .[1] as $repo
	| ($live * $repo)
	| .skills = ((($live.skills // []) + ($dirs | split(":") | map(select(length > 0)))) | unique)
	| if (.skills | length) == 0 then del(.skills) else . end
' "$LIVE" "$SRC/settings.json" > "$LIVE.new"
mv "$LIVE.new" "$LIVE"
[ -f "$LIVE.bak-${STAMP}" ] && echo "  previous file kept as settings.json.bak-${STAMP}"

# [4/4] Extensions, subagent roles, workflow prompts, auto-learn config.
echo "[4/4] Installing extensions, agents and prompts..."
mkdir -p "$DEST/extensions"
for ext in "$SRC"/extensions/*/; do
	rm -rf "$DEST/extensions/$(basename "$ext")"
	cp -a "$ext" "$DEST/extensions/"
done
for d in agents prompts; do
	mkdir -p "$DEST/$d"
	cp -a "$SRC/$d/." "$DEST/$d/"
done

# Schema check (when python3 + jsonschema are present) and a load check.
if python3 -c 'import jsonschema' 2>/dev/null; then
	for pair in models:models settings:settings; do
		schema="$(find "$HOME/.pi/agent/install" -path "*schemas/${pair#*:}.schema.json" 2>/dev/null | head -1)"
		[ -n "$schema" ] || continue
		python3 -I - "$DEST/${pair%%:*}.json" "$schema" <<'PY' || { echo "ERROR: ${pair%%:*}.json does not match Pi's schema."; exit 1; }
import json, sys, jsonschema
jsonschema.validate(json.load(open(sys.argv[1])), json.load(open(sys.argv[2])))
PY
	done
fi
echo
pi --offline --list-models gem12 2>&1 | head -12

echo
echo "======================================================"
echo " Pi is configured."
echo "  start:        pi            (default model network-gem12/qwen-3.8-27b-exl3, medium)"
echo "  subagents:    run one at a time; /implement, /scout-and-plan, /implement-and-review"
echo "  memory:       ${DEST}/memories/<project>/MEMORY.md, tool memory_add"
echo "  restore:      pi -c, then /restore continues an interrupted turn"
echo "======================================================"
