#!/usr/bin/env bash
# launch_arm.sh — correctly launch one arm of agent_driver.py detached
# in the background on a remote box, over SSH.
#
# Why this exists: `cd dir && cmd &` backgrounds the WHOLE "cd && cmd"
# list as one job, running it in a forked subshell -- the `cd` never
# affects the parent (interactive) shell. A follow-up command typed
# after that `&` runs from whatever directory the parent shell was ALREADY
# in, not the directory the first command just changed to. This broke a
# real launch in this exact shape (2026-08-18): a defn-arm job backgrounded
# with `cd ~/defn/bench/head-to-head-go && ... && nohup python3 agent_driver.py ... &`
# launched fine; the files-arm job typed right after it (same pattern,
# same missing awareness) ran `python3 agent_driver.py` from the SSH
# login shell's own cwd (~), which has no such file, and failed silently
# in its own log rather than the terminal. Using absolute paths
# throughout (this script's own dir for the interpreter path, explicit
# --corpus-dir) sidesteps the whole class of mistake instead of relying
# on getting the cd-then-background ordering right by hand each time.
#
# Usage (from the box, or via ssh ... bash launch_arm.sh ...):
#   launch_arm.sh <corpus-dir> <arm: defn|files> <model> <budget-usd> <max-turns> <log-path>
#
# Example:
#   ssh box 'bash ~/defn/bench/head-to-head-go/launch_arm.sh \
#     ~/defn/bench/prometheus-repo-opus defn opus 8.0 50 ~/prometheus_opus_defn.log'

set -euo pipefail

CORPUS_DIR="$1"
ARM="$2"
MODEL="$3"
BUDGET="$4"
MAX_TURNS="$5"
LOG_PATH="$6"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Guardrail (2026-08-20): refuse to launch against a stale/dirty defn
# checkout -- see launch_arm_parallel.sh's identical check for the full
# rationale (two full bench reruns burned on a binary missing
# already-committed fixes, discovered only after the fact). Set
# DEFN_BENCH_SKIP_SYNC_CHECK=1 to bypass for a deliberate exploratory
# run against unpushed local changes.
if [ -z "${DEFN_BENCH_SKIP_SYNC_CHECK:-}" ]; then
	git -C "$SCRIPT_DIR" fetch origin main --quiet
	if [ -n "$(git -C "$SCRIPT_DIR" status --porcelain --untracked-files=no)" ]; then
		echo "[launch_arm] ABORT: $SCRIPT_DIR has uncommitted tracked changes -- commit or stash before launching a bench run whose result you intend to trust (or set DEFN_BENCH_SKIP_SYNC_CHECK=1 to override)." >&2
		exit 1
	fi
	behind="$(git -C "$SCRIPT_DIR" rev-list --count HEAD..origin/main)"
	if [ "$behind" -gt 0 ]; then
		echo "[launch_arm] ABORT: $SCRIPT_DIR is $behind commit(s) behind origin/main -- pull before launching, or the binary under test won't match what you think it is (or set DEFN_BENCH_SKIP_SYNC_CHECK=1 to override)." >&2
		exit 1
	fi
fi

export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
if [ -f "$HOME/.anthropic_env" ]; then
	set -a
	# shellcheck disable=SC1091
	source "$HOME/.anthropic_env"
	set +a
fi

nohup python3 "$SCRIPT_DIR/agent_driver.py" \
	--all --arm "$ARM" --model "$MODEL" \
	--budget-usd "$BUDGET" --max-turns "$MAX_TURNS" \
	--corpus-dir "$CORPUS_DIR" \
	>"$LOG_PATH" 2>&1 </dev/null &
disown

echo "[launch_arm] pid=$! arm=$ARM model=$MODEL corpus=$CORPUS_DIR log=$LOG_PATH"
