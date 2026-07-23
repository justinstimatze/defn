#!/usr/bin/env bash
# Mine git log into plancheck-3c/tasks.yaml.
#
# Each mined task: commit subject as objective, changed non-test .go
# files (1-4 per commit) as ground truth. Ground truth = files a
# correct plan should have declared in filesToRead/Modify/Create.
#
# Filters:
#   - non-merge commits only
#   - Go source files (excludes _test.go, vendor/)
#   - 1..4 changed Go files (too many = too diffuse; zero = doc-only)
#
# Usage:
#   ./mine-tasks.sh          # writes tasks.yaml (50 tasks) in this dir
#   ./mine-tasks.sh 200      # writes 200 tasks instead
#
# Requires: git, python3 (with pyyaml).
set -euo pipefail

N=${1:-50}
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_DIR=$(git rev-parse --show-toplevel)
RAW=$(mktemp)
trap "rm -f $RAW" EXIT

cd "$REPO_DIR"

# Collect candidate commits (walk enough to find N that pass filter).
git log --oneline -$((N * 3)) --no-merges --pretty=format:'%H %s' | \
while read -r hash subject; do
  files=$(git show --stat --name-only --pretty=format: "$hash" 2>/dev/null | \
    grep -E '\.go$' | grep -v '_test\.go$' | grep -v vendor/ | sort -u)
  count=0
  if [ -n "$files" ]; then
    count=$(printf '%s\n' "$files" | grep -c .)
  fi
  if [ "$count" -ge 1 ] && [ "$count" -le 4 ]; then
    echo "---HASH:$hash---"
    echo "SUBJECT:$subject"
    echo "FILES:"
    echo "$files"
  fi
done > "$RAW"

python3 - "$RAW" "$SCRIPT_DIR/tasks.yaml" "$N" <<'PY'
import sys, yaml
raw, out, n_max = sys.argv[1], sys.argv[2], int(sys.argv[3])
with open(raw) as f:
    lines = f.read().splitlines()
tasks, cur, in_files = [], None, False
for line in lines:
    if line.startswith('---HASH:'):
        if cur and cur.get('ground_truth_files'):
            tasks.append(cur)
            if len(tasks) >= n_max:
                break
        cur = {'id': line[8:15], 'repo': '.', 'objective': '',
               'ground_truth_files': [], 'difficulty': 'unknown'}
        in_files = False
    elif line.startswith('SUBJECT:'):
        cur['objective'] = line[8:]
    elif line.startswith('FILES:'):
        in_files = True
    elif in_files and line.endswith('.go'):
        cur['ground_truth_files'].append(line)
if cur and cur.get('ground_truth_files') and len(tasks) < n_max:
    tasks.append(cur)
with open(out, 'w') as f:
    f.write("# plancheck-3c task fixtures - mined from git log via mine-tasks.sh.\n")
    f.write("# Each task: subject line of a merged commit as the objective, changed\n")
    f.write("# non-test Go files (1-4) as ground truth.\n\n")
    yaml.safe_dump({'tasks': tasks}, f, sort_keys=False, width=120)
print(f"wrote {len(tasks)} tasks to {out}")
PY
