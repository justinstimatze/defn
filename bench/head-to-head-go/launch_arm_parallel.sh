#!/usr/bin/env bash
# launch_arm_parallel.sh — like launch_arm.sh, but runs every task in one
# arm CONCURRENTLY (N at a time) instead of strictly sequentially.
#
# Why this exists: agent_driver.py --all processes every task one at a
# time even though each is fully independent (separate git clone,
# separate .defn database, separate workdir) -- a real 15-task/arm run
# took ~2.5 hours wall-clock this way despite the box being mostly idle
# (audited 2026-08-19: 4 vCPUs, per-task time is API-latency-bound, not
# CPU-bound). agent_driver.py's `instance_id` positional arg already
# supports single-task invocation with no other code changes needed;
# this script just fans that out via xargs -P.
#
# _ensure_disk_space() is flock-protected (2026-08-19) so concurrent
# invocations don't race on cache cleanup, but that lock only serializes
# the check-and-clean step itself -- it does NOT guarantee peak
# concurrent disk usage stays under any limit across N simultaneous
# tasks' full lifetimes.
#
# GOMAXPROCS: each task's own `go build`/`defn init`/`defn ingest` step
# defaults to using ALL cores (Go's runtime sets GOMAXPROCS=NumCPU per
# process, with no awareness of sibling concurrent processes). The
# first live run of this script (2026-08-19) hit this directly: N=3 per
# arm x 2 arms = 6 concurrent tasks, each independently spawning its own
# multi-threaded go toolchain, drove load average to 40+ on a 4-vCPU
# box before it was killed. Pinning GOMAXPROCS=1 here bounds each task
# to one core's worth of Go-toolchain parallelism, so PARALLELISM
# concurrent tasks cost at most ~PARALLELISM cores, not
# PARALLELISM*NumCPU. Still start conservative (PARALLELISM=2 on a
# 4-vCPU box running both arms at once, i.e. 4 total workers) and watch
# `uptime`/`df -h`/`free -h` before pushing higher; purge stale workdirs
# from old/completed corpora first for headroom.
#
# Usage:
#   launch_arm_parallel.sh <corpus-dir> <arm: defn|files> <model> <budget-usd> <max-turns> <parallelism> <log-dir>
#
# Example:
#   ssh box 'bash ~/defn/bench/head-to-head-go/launch_arm_parallel.sh \
#     ~/defn/bench/prometheus-repo-opus defn opus 8.0 100 2 ~/logs/prom-opus-defn'
#
# Per-task output still lands at <corpus-dir>/arm_<arm>/<instance_id>.json
# same as --all; this script only changes HOW MANY run at once, not
# where results go or how they're scored. Per-task stdout/stderr goes to
# <log-dir>/<instance_id>.log instead of one combined stream, since N
# concurrent processes interleaving on one fd is unreadable.

set -euo pipefail

CORPUS_DIR="$1"
ARM="$2"
MODEL="$3"
BUDGET="$4"
MAX_TURNS="$5"
PARALLELISM="$6"
LOG_DIR="$7"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
if [ -f "$HOME/.anthropic_env" ]; then
	set -a
	# shellcheck disable=SC1091
	source "$HOME/.anthropic_env"
	set +a
fi

mkdir -p "$LOG_DIR"

run_task() {
	local iid="$1"
	GOMAXPROCS=1 python3 "$SCRIPT_DIR/agent_driver.py" "$iid" \
		--arm "$ARM" --model "$MODEL" \
		--budget-usd "$BUDGET" --max-turns "$MAX_TURNS" \
		--corpus-dir "$CORPUS_DIR" \
		>"$LOG_DIR/$iid.log" 2>&1
	echo "[done] $iid"
}
export -f run_task
export SCRIPT_DIR ARM MODEL BUDGET MAX_TURNS CORPUS_DIR LOG_DIR

python3 -c "
import json
with open('$CORPUS_DIR/tasks.jsonl') as f:
    for line in f:
        print(json.loads(line)['instance_id'])
" | xargs -P "$PARALLELISM" -I{} bash -c 'run_task "$@"' _ {}

echo "[launch_arm_parallel] all tasks dispatched (parallelism=$PARALLELISM), per-task logs in $LOG_DIR"
