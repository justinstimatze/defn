#!/usr/bin/env bash
# run_defn_arm.sh — set up per-task workspace for the defn-mode arm.
#
# Given a Multi-SWE-bench Go instance_id, this script:
#   1. Clones the repo at the base_commit
#   2. Ingests it with defn (isolated .defn/)
#   3. Prints the problem_statement so an agent driver can consume it
#
# Actual agent invocation is NOT wired here — the shape depends on
# how we drive Claude (see README "What still needs building"). This
# gets the workspace ready; the driver reads problem_statement +
# invokes the agent + writes trajectory.jsonl.
#
# Usage:
#   ./run_defn_arm.sh <instance_id> [workdir-root]

set -euo pipefail

instance_id="${1:?usage: $0 <instance_id> [workdir-root]}"
workdir_root="${2:-/tmp/defn-h2h-go}"
tasks_file="$(dirname "$0")/tasks.jsonl"

if [ ! -f "$tasks_file" ]; then
  echo "no tasks.jsonl — run select_tasks.py first" >&2
  exit 2
fi

# Extract task metadata
task_json=$(jq -c "select(.instance_id == \"$instance_id\")" "$tasks_file")
if [ -z "$task_json" ]; then
  echo "instance_id not found in tasks.jsonl: $instance_id" >&2
  exit 2
fi

org=$(jq -r .org <<<"$task_json")
repo=$(jq -r .repo <<<"$task_json")
sha=$(jq -r .base_commit_sha <<<"$task_json")
workdir="$workdir_root/$instance_id"

echo "instance:   $instance_id" >&2
echo "repo:       $org/$repo" >&2
echo "base_sha:   $sha" >&2
echo "workdir:    $workdir" >&2

mkdir -p "$workdir_root"

# 1. Clone (shallow, single commit) if missing
if [ ! -d "$workdir/.git" ]; then
  echo ">> cloning $org/$repo" >&2
  git clone "https://github.com/$org/$repo" "$workdir" >&2
fi
cd "$workdir"
git fetch origin "$sha" >&2 || true
git checkout "$sha" >&2

# 2. Ingest with defn
if [ ! -d ".defn" ]; then
  echo ">> defn init + ingest" >&2
  defn init >&2
  defn ingest . >&2
else
  echo ">> defn already initialized (skipping ingest)" >&2
fi

# 3. Emit problem_statement for the agent driver
echo ">> READY. workdir: $workdir" >&2
echo ">> problem_statement follows on stdout <<<END>>>"
jq -r .problem_statement <<<"$task_json"
echo "<<<END>>>"
