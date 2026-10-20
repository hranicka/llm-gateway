#!/usr/bin/env bash
# Usage: ./scripts/client/install-omp.sh
#
# Syncs config/omp into ~/.omp (Oh My Pi; OMP_HOME overrides it):
#   agent/models.yml                      -> ~/.omp/agent/models.yml
#   profiles/<name>/agent/models.yml      -> ~/.omp/profiles/<name>/agent/models.yml
#   profiles/<name>/agent/config.yml      -> merged into the live file (repo values
#                                            win; omp's own keys such as theme stay)
# models.yml is copied; a differing live file is kept as <file>.bak-<timestamp>
# (config.yml gets one too when the merge changes it). profiles/<name>/project/
# config.yml is a per-project overlay: copy it to <project>/.omp/config.yml.
# Needs python3 with PyYAML. Re-running is safe.

set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../../config/omp"
DEST="${OMP_HOME:-$HOME/.omp}"
STAMP="$(date +%Y%m%d%H%M%S)"

put() { # <repo file> <live file>: copy, keeping a backup of a differing live file
	mkdir -p "$(dirname "$2")"
	if [ -f "$2" ] && ! cmp -s "$1" "$2"; then cp "$2" "$2.bak-${STAMP}"; echo "previous $2 kept as .bak-${STAMP}"; fi
	cp "$1" "$2"
}

merge() { # <repo config.yml> <live config.yml>
	if [ ! -f "$2" ]; then put "$1" "$2"; return; fi
	python3 -I - "$1" "$2" > "$2.new" <<'PY'
import sys, yaml
def merge(a, b):
    for k, v in b.items():
        a[k] = merge(a[k], v) if isinstance(v, dict) and isinstance(a.get(k), dict) else v
    return a
repo, live = (yaml.safe_load(open(p)) or {} for p in sys.argv[1:3])
yaml.safe_dump(merge(live, repo), sys.stdout, sort_keys=False)
PY
	if ! python3 -I -c 'import sys,yaml; sys.exit(yaml.safe_load(open(sys.argv[1]))!=yaml.safe_load(open(sys.argv[2])))' "$2.new" "$2"; then
		cp "$2" "$2.bak-${STAMP}"; echo "previous $2 kept as .bak-${STAMP}"; mv "$2.new" "$2"
	else
		rm -f "$2.new"
	fi
}

put "$SRC/agent/models.yml" "$DEST/agent/models.yml"
for dir in "$SRC"/profiles/*/agent; do
	name="$(basename "$(dirname "$dir")")"
	put "$dir/models.yml" "$DEST/profiles/$name/agent/models.yml"
	merge "$dir/config.yml" "$DEST/profiles/$name/agent/config.yml"
done
echo "omp config synced into ${DEST}"
