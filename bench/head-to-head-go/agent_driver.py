#!/usr/bin/env python3
"""
agent_driver.py — run one Multi-SWE-bench Go task through Claude Code with
defn's `code` MCP as the ONLY code-access tool (Bash allowed for tests/find).

Sets up the workspace, launches `claude -p` in headless streaming mode, parses
the stream-json output into an fncall_messages-shape trajectory, and writes it
to arm_defn/<instance_id>.json for analyze.py.

Usage:
  python3 agent_driver.py <instance_id> [--budget-usd 3.0] [--max-turns 60]
  python3 agent_driver.py --all [--budget-usd 3.0] [--max-turns 60]

The bench's product measurement is agnostic to which model powered the
files-mode baseline vs the defn arm — the metric is bytes across the tool
boundary. Cost caps are per-task via --budget-usd.
"""

import argparse
import json
import os
import shlex
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
TASKS = os.path.join(HERE, "tasks.jsonl")
ARM_DIR = os.path.join(HERE, "arm_defn")
ARM_DIR_FILES = os.path.join(HERE, "arm_files")
WORKDIR_ROOT = "/tmp/defn-h2h-go"
# Cached fresh .defn/ per (instance_id, defn_binary_hash). Contamination
# fix (6abe8e1) forces a fresh ingest per arm — ~30-90s of pure CPU per
# arm. Snapshot after first ingest, restore on subsequent runs; hit path
# is ~2s (tarball extract) vs full re-parse. Invalidates on defn binary
# change so DB schema drift doesn't corrupt cached DBs.
DEFN_CACHE_ROOT = "/tmp/defn-h2h-go-cache"


def _defn_binary_hash():
    """sha256[:12] of the defn binary on PATH — cache key component so
    a rebuilt defn (schema drift, ingest-logic change) invalidates
    stale snapshots automatically."""
    try:
        which = subprocess.check_output(["which", "defn"], text=True).strip()
        out = subprocess.check_output(["sha256sum", which], text=True)
        return out.split()[0][:12]
    except subprocess.CalledProcessError:
        return "unknown"


def _defn_cache_path(inst_id, binhash):
    return os.path.join(DEFN_CACHE_ROOT, f"{inst_id}__{binhash}.tar")


DEFN_MCP_CONFIG = {
    "mcpServers": {"defn": {"type": "stdio", "command": "defn", "args": ["serve"]}}
}
EMPTY_MCP_CONFIG = {"mcpServers": {}}

# SECURITY: with --permission-mode bypassPermissions, any allowed tool runs
# without user approval. Bash is intentionally NOT in the allowlist — an
# adversarial problem_statement + a cloned public repo could inject shell
# commands that damage the host. defn's `test` op covers scoped test runs;
# `code` covers all source access. If a task truly requires arbitrary bash
# (e.g., `go build` for compile check), it will fail visibly rather than
# silently exec unknown commands.
ALLOWED_TOOLS = "mcp__defn__code TodoWrite"
# Escape hatches close: Grep/Glob let the model bypass defn's `search` op;
# Agent/Task* let it spawn subagents that use full tool set; dispatch is
# cross-session messaging. n=10 measurement 2026-07-20 found 170k / 481k
# (35%) of measured wire went to these off-tool paths, invisibly diluting
# every defn-side lever we measured. Closing them here so the "defn arm"
# actually is defn-only.
#
# 2026-08-07: found the hard way that this list goes stale every time
# Claude Code ships a new tool -- Monitor didn't exist when this list was
# written, wasn't in it, and the agent found it via ToolSearch after
# Bash was blocked (literally searched "select:Bash,BashOutput" then
# "write file shell command execute"), then used Monitor's `command`
# param as a full shell escape hatch: go install, git checkout --, raw
# Python file edits, gofmt -w. The "defn arm" was paying for BOTH defn's
# code tool AND unrestricted shell. Also found --allowedTools does NOT
# act as an exclusive allowlist under --permission-mode bypassPermissions
# -- only --disallowedTools actually excludes a tool from being callable.
# So this list must stay a superset of every tool NOT in ALLOWED_TOOLS,
# not a hand-picked "the dangerous ones" list -- re-check against
# `claude -p ... -- "list your tools"` output before trusting any future
# run's arm isolation.
DISALLOWED_TOOLS = (
    "Read Write Edit MultiEdit NotebookEdit Bash "
    "Grep Glob "
    "Agent Task TaskCreate TaskUpdate TaskGet TaskList TaskOutput TaskStop "
    "mcp__dispatch__dispatch mcp__dispatch__peek mcp__dispatch__ack "
    "mcp__dispatch__who mcp__dispatch__subscribe mcp__dispatch__unsubscribe "
    "SendMessage WebFetch WebSearch "
    "CronCreate CronDelete CronList DesignSync EnterWorktree ExitWorktree "
    "ListAgents Monitor PushNotification ReportFindings ScheduleWakeup "
    "Skill ToolSearch Workflow"
)

SYSTEM_APPEND = """
IMPORTANT — this session is Go-only, defn-only. Use `mcp__defn__code` for ALL
Go source access and edits: op:overview for project shape, op:outline for a
def's shape, op:search for symbol/text search, op:read for a def body, op:edit
/ op:replace-hunk / op:create for writes. Never call Read/Write/Edit on .go
files — those are disabled. For running tests, use code op:test (scoped to
defs) — direct shell is not available. Complete the task and stop.

The issue describes a bug that CURRENTLY EXISTS in this codebase. Assume
the fix is not already in place until you have PROVEN it — either by
reading the exact code the issue names and confirming the failing input
would still fail, OR by running `code op:test` on the target and
observing the failure. If your entire set of writes ends up in
`_test.go` files, you have NOT implemented the fix — production code
must change. Do not conclude the task complete without a production-code
write unless you can cite the exact def and line whose current
implementation already handles the issue.

If the issue names a failing test (`TestFoo` / `TestBar`), REPRODUCE it
before writing anything: `code op:test test:"TestFoo"` runs one test by
name. A test that passes today means the bug is not what you think it is
— re-read before editing. A test that fails is a concrete anchor for
your fix; iterate against it.

Read-then-give-up is the most common failure mode on this bench. After
5 read/outline/read-file/overview calls WITHOUT a write or a test-run,
STOP READING. Instead: form a concrete hypothesis (name the def and
the exact behavior change), then either (a) `op:test test:"TestX"` to
observe the current behavior, or (b) `op:edit` / `op:replace-hunk` to
implement your best guess and iterate. Additional reads past that point
almost never surface new information — you already have what you need.
""".strip()

# files-mode arm: the live comparison counterpart. No defn MCP tool at all
# (EMPTY_MCP_CONFIG) -- Read/Grep/Glob to explore, Edit/Write to change,
# Bash for `go build`/`go test`. Same off-tool escape hatches closed as the
# defn arm, for parity.
FILES_ALLOWED_TOOLS = "Read Write Edit MultiEdit Bash Grep Glob TodoWrite"
# Same staleness caveat as DISALLOWED_TOOLS above -- Monitor is less of a
# functional escape here (Bash is already allowed) but EnterWorktree/
# Workflow could still move work outside the measured workdir or spawn a
# subagent with a different tool set, so closed for parity.
FILES_DISALLOWED_TOOLS = (
    "Agent Task TaskCreate TaskUpdate TaskGet TaskList TaskOutput TaskStop "
    "mcp__dispatch__dispatch mcp__dispatch__peek mcp__dispatch__ack "
    "mcp__dispatch__who mcp__dispatch__subscribe mcp__dispatch__unsubscribe "
    "SendMessage WebFetch WebSearch "
    "CronCreate CronDelete CronList DesignSync EnterWorktree ExitWorktree "
    "ListAgents Monitor PushNotification ReportFindings ScheduleWakeup "
    "Skill ToolSearch Workflow"
)

FILES_SYSTEM_APPEND = """
IMPORTANT — this session is Go-only. Use Read/Grep/Glob to explore the
codebase, Edit/Write to make changes, Bash for `go build`/`go test`.
Complete the task and stop.

The issue describes a bug that CURRENTLY EXISTS in this codebase. Assume
the fix is not already in place until you have PROVEN it — either by
reading the exact code the issue names and confirming the failing input
would still fail, OR by running the relevant test and observing the
failure. If your entire set of writes ends up in `_test.go` files, you
have NOT implemented the fix — production code must change. Do not
conclude the task complete without a production-code write unless you
can cite the exact function and line whose current implementation
already handles the issue.

If the issue names a failing test (`TestFoo` / `TestBar`), REPRODUCE it
before writing anything: `go test -run TestFoo ./...` runs one test by
name. A test that passes today means the bug is not what you think it is
— re-read before editing. A test that fails is a concrete anchor for
your fix; iterate against it.

Read-then-give-up is the most common failure mode on this bench. After
5 Read/Grep/Glob calls WITHOUT a write or a test-run, STOP READING.
Instead: form a concrete hypothesis (name the function and the exact
behavior change), then either (a) run the relevant test to observe
current behavior, or (b) edit your best guess and iterate. Additional
reads past that point almost never surface new information — you
already have what you need.
""".strip()


def load_task(instance_id):
    with open(TASKS) as f:
        for line in f:
            r = json.loads(line)
            if r["instance_id"] == instance_id:
                return r
    raise KeyError(instance_id)


def setup_workspace(task, arm="defn"):
    """Clone repo at base_commit, run defn init + ingest. Returns workdir.

    Workdir is arm-scoped (inst__arm, not just inst) -- running both arms
    of the same task concurrently against a shared directory races on the
    initial `git clone` and corrupts whichever arm loses. Found 2026-08-07
    when both arms of the same task launched in parallel: one arm's clone
    failed outright (dir already existed non-empty), and cache/ingest
    interleaving under concurrent load caused a second, separate failure
    mode. Arm-scoping the workdir removes the collision entirely.
    """
    inst = task["instance_id"]
    workdir = os.path.join(WORKDIR_ROOT, f"{inst}__{arm}")
    os.makedirs(WORKDIR_ROOT, exist_ok=True)
    print(f"[setup] instance {inst} (arm={arm})", file=sys.stderr)

    if not os.path.isdir(os.path.join(workdir, ".git")):
        print(f"[setup] cloning {task['org']}/{task['repo']}", file=sys.stderr)
        subprocess.check_call(
            [
                "git",
                "clone",
                "--quiet",
                f"https://github.com/{task['org']}/{task['repo']}",
                workdir,
            ]
        )
    subprocess.check_call(
        ["git", "-C", workdir, "fetch", "--quiet", "origin", task["base_commit_sha"]],
        stderr=subprocess.DEVNULL,
    )
    subprocess.check_call(
        ["git", "-C", workdir, "checkout", "--quiet", task["base_commit_sha"]],
        stderr=subprocess.DEVNULL,
    )

    # Contamination fix: prior arm runs left modified tracked files AND
    # untracked scratch files in the workdir. On rerun the model was reading
    # its own historical writes as "the current state" — completely invalidating
    # every measurement made on a rerun'd workdir. Reset tracked files to
    # base_commit_sha and clean untracked, preserving only bench-harness
    # artifacts (.defn/, .mcp-*, .claude-stream.jsonl, CLAUDE.md).
    subprocess.check_call(
        ["git", "-C", workdir, "reset", "--hard", "--quiet", task["base_commit_sha"]],
        stderr=subprocess.DEVNULL,
    )
    subprocess.check_call(
        [
            "git",
            "-C",
            workdir,
            "clean",
            "-fd",
            "--quiet",
            "-e",
            ".mcp-defn-only.json",
            "-e",
            ".mcp.json",
            "-e",
            ".claude-stream.jsonl",
            "-e",
            "CLAUDE.md",
        ],
        stderr=subprocess.DEVNULL,
    )

    # Force a fresh .defn/ per arm. Keeping it across reruns wedges the DB
    # in the previous run's post-write state (fabricated tests etc.);
    # subsequent op:test would call emit.Emit and write that stale state
    # back to the freshly-reset disk. Cache the fresh ingest result per
    # (inst_id, defn binary hash) so repeat runs pay ~2s tarball extract
    # instead of ~30-90s full re-parse.
    import shutil

    defn_dir = os.path.join(workdir, ".defn")
    if os.path.isdir(defn_dir):
        shutil.rmtree(defn_dir)

    binhash = _defn_binary_hash()
    cache_path = _defn_cache_path(inst, binhash)
    os.makedirs(DEFN_CACHE_ROOT, exist_ok=True)

    if os.path.exists(cache_path):
        print(
            f"[setup] restoring cached .defn/ ({os.path.basename(cache_path)})",
            file=sys.stderr,
        )
        subprocess.check_call(["tar", "-xf", cache_path, "-C", workdir])
        return workdir

    print(f"[setup] defn init + ingest (~1 min)", file=sys.stderr)
    # Bug-fix bench workdirs contain broken code (that's the whole
    # point) — package-parse errors are expected on some ingests.
    # Use subprocess.run and check that `.defn/` was created rather
    # than trusting exit status; ingest returns non-zero when any
    # package fails but still persists what it could parse.
    # `defn init <path>`/`defn ingest <path>` don't actually operate on
    # the path argument -- they write .defn relative to CWD instead,
    # silently (exit 0, "done. N modules...") and REPORT SUCCESS while
    # writing to the wrong place. Found 2026-08-07: every concurrent
    # harness run was racing to write the SAME shared ~/.defn (the
    # script's own cwd) instead of each process's own workdir. Passing
    # cwd=workdir works around it; this looks like a real defn CLI bug
    # independent of this harness, worth filing separately.
    subprocess.run(
        ["defn", "init", "."],
        cwd=workdir,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    subprocess.run(
        ["defn", "ingest", "."],
        cwd=workdir,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    if not os.path.isdir(defn_dir):
        raise RuntimeError(
            f"[setup] defn init/ingest did not create {defn_dir} — "
            f"see manual `defn init {workdir}` for the underlying error"
        )

    # Cache the fresh ingest. Tar (no compression) — Dolt's noms store
    # is already densely packed; gzip barely helps and doubles extract cost.
    tmp_cache = cache_path + ".tmp"
    subprocess.check_call(["tar", "-cf", tmp_cache, "-C", workdir, ".defn"])
    os.replace(tmp_cache, cache_path)
    print(
        f"[setup] cached fresh .defn/ -> {os.path.basename(cache_path)}",
        file=sys.stderr,
    )
    return workdir


def build_prompt(task):
    return f"""You are working in a Go repository. Please solve the following issue.

<issue>
{task["problem_statement"]}
</issue>

Use defn's code MCP for all source access. When done, stop — do not open a shell for the next task.
"""


def run_claude(workdir, prompt, budget_usd, max_turns, arm="defn"):
    """Invoke claude -p with the given arm's tool set; return list of stream-json event dicts."""
    mcp_config_path = os.path.join(workdir, ".mcp-defn-only.json")
    with open(mcp_config_path, "w") as f:
        json.dump(DEFN_MCP_CONFIG if arm == "defn" else EMPTY_MCP_CONFIG, f)

    allowed = ALLOWED_TOOLS if arm == "defn" else FILES_ALLOWED_TOOLS
    disallowed = DISALLOWED_TOOLS if arm == "defn" else FILES_DISALLOWED_TOOLS
    system_append = SYSTEM_APPEND if arm == "defn" else FILES_SYSTEM_APPEND

    # --add-dir and --allowedTools are variadic in claude's CLI parser, so
    # any positional prompt arg that follows can be swallowed. Feed the
    # prompt through stdin instead — --input-format text is the default.
    cmd = [
        "claude",
        "-p",
        # NOTE: --bare requires ANTHROPIC_API_KEY. We drop it so the driver
        # uses the invoking user's OAuth session. Downside: parent hooks +
        # CLAUDE.md may still fire; use --strict-mcp-config + tool filters
        # to isolate. Set CLAUDE_CODE_SIMPLE=1 in env for lighter runs.
        "--mcp-config",
        mcp_config_path,
        "--strict-mcp-config",
        "--allowedTools",
        allowed,
        "--disallowedTools",
        disallowed,
        "--append-system-prompt",
        system_append,
        "--output-format",
        "stream-json",
        "--verbose",
        "--permission-mode",
        "bypassPermissions",
        "--max-budget-usd",
        str(budget_usd),
        "--max-turns",
        str(max_turns),
        "--add-dir",
        workdir,
    ]
    stream_path = os.path.join(workdir, ".claude-stream.jsonl")
    open(stream_path, "w").close()
    print(
        f"[claude] launching: {' '.join(shlex.quote(a) for a in cmd[:5])} ... (stdin prompt, stream -> {stream_path})",
        file=sys.stderr,
    )
    start = time.time()
    events = []
    with (
        subprocess.Popen(
            cmd,
            cwd=workdir,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ) as p,
        open(stream_path, "a") as sf,
    ):
        p.stdin.write(prompt)
        p.stdin.close()
        for line in p.stdout:
            sf.write(line)
            sf.flush()
            line = line.strip()
            if not line:
                continue
            try:
                ev = json.loads(line)
                events.append(ev)
            except json.JSONDecodeError:
                pass
        rc = p.wait()
        stderr = p.stderr.read()
    elapsed = time.time() - start
    print(
        f"[claude] rc={rc} elapsed={elapsed:.1f}s events={len(events)}", file=sys.stderr
    )
    if rc != 0 and stderr:
        print(f"[claude] stderr tail: {stderr[-500:]}", file=sys.stderr)
    return events, rc, elapsed


def events_to_fncall_messages(events):
    """Convert claude stream-json events into an fncall_messages-shape trajectory.

    Claude stream-json emits an outer envelope per event:
      {"type": "user"|"assistant"|"system"|"result", "message": {...}, ...}
    where message follows the Anthropic API shape. We flatten to the same
    role/tool_calls schema used by Multi-SWE-bench trajectories so analyze.py's
    wire_cost() works unchanged.
    """
    out = []
    total_cost = None
    for ev in events:
        et = ev.get("type")
        if et == "system":
            continue
        if et == "result":
            total_cost = ev.get("total_cost_usd") or ev.get("cost_usd")
            continue
        msg = ev.get("message") or {}
        role = msg.get("role") or et
        content = msg.get("content", "")
        # Anthropic content can be a list of blocks: text, tool_use, tool_result
        if isinstance(content, list):
            tool_calls = []
            text_parts = []
            for block in content:
                bt = block.get("type") if isinstance(block, dict) else None
                if bt == "text":
                    text_parts.append(block.get("text", ""))
                elif bt == "tool_use":
                    tool_calls.append(
                        {
                            "id": block.get("id"),
                            "type": "function",
                            "function": {
                                "name": block.get("name"),
                                "arguments": json.dumps(block.get("input", {})),
                            },
                        }
                    )
                elif bt == "tool_result":
                    inner = block.get("content", "")
                    if isinstance(inner, list):
                        inner = "".join(
                            x.get("text", "") if isinstance(x, dict) else str(x)
                            for x in inner
                        )
                    out.append({"role": "tool", "content": inner or ""})
            if role == "assistant":
                entry = {"role": "assistant", "content": "\n".join(text_parts)}
                if tool_calls:
                    entry["tool_calls"] = tool_calls
                out.append(entry)
            elif role == "user":
                # user turns from stream-json are usually tool_result batches;
                # already handled above. Any leftover text becomes a user msg.
                if text_parts:
                    out.append({"role": "user", "content": "\n".join(text_parts)})
        else:
            out.append({"role": role, "content": content or ""})
    return out, total_cost


def apply_edits_via_defn(workdir):
    """After the agent finishes, `defn emit` writes the mutated DB back to
    .go files so the workdir reflects the agent's changes. This lets the
    correctness scorer diff files."""
    try:
        subprocess.check_call(
            ["defn", "emit", workdir],
            cwd=workdir,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except subprocess.CalledProcessError as e:
        print(f"[emit] defn emit failed: {e}", file=sys.stderr)


def run_one(instance_id, budget_usd, max_turns, arm="defn"):
    task = load_task(instance_id)
    workdir = setup_workspace(task, arm=arm)
    prompt = build_prompt(task)
    events, rc, elapsed = run_claude(workdir, prompt, budget_usd, max_turns, arm=arm)
    traj, cost = events_to_fncall_messages(events)
    if arm == "defn":
        # files-mode already writes directly to disk via Edit/Write --
        # calling `defn emit` here would overwrite those changes with the
        # DB's (untouched, since files-mode never called defn) stale state.
        apply_edits_via_defn(workdir)

    out_dir = ARM_DIR if arm == "defn" else ARM_DIR_FILES
    os.makedirs(out_dir, exist_ok=True)
    out_path = os.path.join(out_dir, instance_id + ".json")
    with open(out_path, "w") as f:
        json.dump(
            {
                "instance_id": instance_id,
                "fncall_messages": traj,
                "workdir": workdir,
                "claude_rc": rc,
                "elapsed_sec": elapsed,
                "cost_usd": cost,
                "n_raw_events": len(events),
            },
            f,
        )
    print(
        f"[done] wrote {out_path} ({len(traj)} msgs, ${cost}, {elapsed:.1f}s)",
        file=sys.stderr,
    )
    return out_path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("instance_id", nargs="?")
    ap.add_argument("--all", action="store_true")
    ap.add_argument("--budget-usd", type=float, default=3.0)
    ap.add_argument("--max-turns", type=int, default=50)
    ap.add_argument("--arm", choices=["defn", "files"], default="defn")
    args = ap.parse_args()

    if args.all:
        with open(TASKS) as f:
            tasks = [json.loads(l)["instance_id"] for l in f]
        for i, tid in enumerate(tasks, 1):
            print(f"\n===== [{i}/{len(tasks)}] {tid} =====", file=sys.stderr)
            try:
                run_one(tid, args.budget_usd, args.max_turns, arm=args.arm)
            except Exception as e:
                print(f"[fail] {tid}: {type(e).__name__}: {e}", file=sys.stderr)
    else:
        if not args.instance_id:
            ap.error("provide instance_id or --all")
        run_one(args.instance_id, args.budget_usd, args.max_turns, arm=args.arm)


if __name__ == "__main__":
    main()
