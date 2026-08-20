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
# --corpus-dir (default: this script's own directory) picks which
# tasks.jsonl / arm_defn / arm_files to use -- lets the same driver run
# against any hand-curated or Multi-SWE-bench-sourced task set that
# follows the same schema (e.g. bench/prometheus-repo/), not just the
# original cli/grpc-go/go-zero corpus this script was built for.
DEFAULT_CORPUS_DIR = HERE
# Homedir, NOT /tmp: an EC2 stop/start cycle clears /tmp (systemd-tmpfiles
# on boot), which silently destroyed every task's scoring workdir the
# first time this bench got run across an instance restart (2026-08-09)
# -- score_correctness.py's resolve_defname_to_file needs these to survive
# between the agent run and a later (possibly next-session) scoring pass.
# ~/.cache survives stop/start since it's on the persistent root volume.
WORKDIR_ROOT = os.path.expanduser("~/.cache/defn-h2h-go")
DISK_FREE_MIN_GB = 5.0
# Cached fresh .defn/ per (instance_id, defn_binary_hash). Contamination
# fix (6abe8e1) forces a fresh ingest per arm — ~30-90s of pure CPU per
# arm. Snapshot after first ingest, restore on subsequent runs; hit path
# is ~2s (tarball extract) vs full re-parse. Invalidates on defn binary
# change so DB schema drift doesn't corrupt cached DBs.
DEFN_CACHE_ROOT = os.path.expanduser("~/.cache/defn-h2h-go-cache")


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


def _defn_version_string():
    """Output of `defn version` (e.g. "0.26.71") — stamped into every
    trajectory alongside _defn_binary_hash() so a trajectory file is
    self-describing about exactly which build produced it. Added
    2026-08-18 after a real investigation (etcd-multifile bench) burned
    several calls doing git-log archaeology to work out whether a
    trajectory ran on pre- or post-fix code, only reachable by comparing
    the trajectory's file mtime against when a commit landed. That
    should never require archaeology again — it's answerable by reading
    one field in the file itself."""
    try:
        return subprocess.check_output(["defn", "version"], text=True).strip()
    except subprocess.CalledProcessError:
        return "unknown"


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
#
# 2026-08-18: Read/Write/Edit added back for NON-Go files only (see
# SYSTEM_APPEND's rule below) after a real Opus trajectory
# (prometheus-18972) proved the prior Go-only-defn-only isolation was
# too strict to be a fair comparison: the gold fix required a
# docs/configuration/configuration.md edit, the defn arm had
# structurally NO way to make it (code's scope is Go source only, by
# design -- see this project's own CLAUDE.md), and the model correctly
# diagnosed the needed doc change but explicitly said it had no tool to
# make it with. 6 of 15 tasks in the prometheus-repo-opus corpus have
# at least one non-.go gold file; this was silently costing defn recall
# on every one of them, an artifact of the harness's tool isolation,
# not of defn itself. Real-world defn usage always pairs `code` for Go
# with Edit/Write for everything else (CLAUDE.md's own documented
# convention) -- this restores that pairing for the bench too, while
# keeping Bash/MultiEdit closed to preserve the original "no escape
# hatch dilutes the Go-side measurement" protection below. (Grep
# reopened non-Go-only on 2026-08-19, see below.)
#
# 2026-08-19: Glob added back too, same non-Go-only carve-out as
# Read/Write/Edit above (enforced the same way: a SYSTEM_APPEND
# instruction, not a hard tool-level restriction -- exactly as much
# trust as the existing Read/Write/Edit carve-out already relies on).
# A real trajectory (prometheus-18534) needed to find a
# promql/promqltest/testdata/*.test golden fixture and had no way to
# discover its path -- code's search op is Go-definitions-only by
# design, so there was structurally no way to enumerate non-Go files by
# pattern.
#
# 2026-08-19 (later same day): Grep added back too, same non-Go-only
# carve-out. A real trajectory (prometheus-19184) needed to check
# docs/configuration/configuration.md for stale label documentation
# after fixing the underlying code panic -- files-mode found it via a
# quick `grep -n "jmx_exporter_enabled|..."` and fixed it (recall 1.00);
# defn-mode had no way to text-search non-Go files at all and never
# thought to check docs, losing recall on a task both arms otherwise
# solved identically. Grep on .go files is still squarely
# code(op:"search")'s job -- the SYSTEM_APPEND instruction below draws
# that line the same way it already does for Glob.
ALLOWED_TOOLS = "mcp__defn__code TodoWrite Read Write Edit Glob Grep"
# Escape hatches close: Agent/Task* let it spawn subagents that use full
# tool set; dispatch is cross-session messaging. n=10 measurement
# 2026-07-20 found 170k / 481k (35%) of measured wire went to these
# off-tool paths, invisibly diluting every defn-side lever we measured.
# Closing them here so the "defn arm" actually is defn-only for its
# GO-side measurement (see the Read/Write/Edit/Glob/Grep carve-out above
# for non-Go files).
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
    "MultiEdit NotebookEdit Bash "
    "Agent Task TaskCreate TaskUpdate TaskGet TaskList TaskOutput TaskStop "
    "mcp__dispatch__dispatch mcp__dispatch__peek mcp__dispatch__ack "
    "mcp__dispatch__who mcp__dispatch__subscribe mcp__dispatch__unsubscribe "
    "SendMessage WebFetch WebSearch "
    "CronCreate CronDelete CronList DesignSync EnterWorktree ExitWorktree "
    "ListAgents Monitor PushNotification ReportFindings ScheduleWakeup "
    "Skill ToolSearch Workflow"
)

SYSTEM_APPEND = """
IMPORTANT — this session measures `mcp__defn__code` against files-mode on Go
source specifically. Use `mcp__defn__code` for ALL Go (.go) source access and
edits: op:overview for project shape, op:outline for a def's shape, op:search
for symbol/text search, op:read for a def body, op:edit / op:replace-hunk /
op:create for writes. Never call Read/Write/Edit/Glob/Grep on .go files — use
`code` for every one of those, no exceptions. Read/Write/Edit/Glob/Grep ARE
available, but ONLY for non-Go files (docs/*.md, testdata, config, etc.) that
the fix may also need to touch — `code`'s scope is Go source only, by design,
so use Glob/Grep only to find or search the PATH/CONTENT of a non-Go file
(e.g. a testdata fixture, or checking docs for stale references after a code
fix), never to search or browse .go files. For running tests, use code
op:test (scoped to defs) — direct shell is not available. Complete the task
and stop.

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


def load_task(instance_id, corpus_dir=DEFAULT_CORPUS_DIR):
    tasks_path = os.path.join(corpus_dir, "tasks.jsonl")
    with open(tasks_path) as f:
        for line in f:
            r = json.loads(line)
            if r["instance_id"] == instance_id:
                return r
    raise KeyError(f"{instance_id} not found in {tasks_path}")


def setup_workspace(task, arm="defn", corpus_dir=DEFAULT_CORPUS_DIR):
    """Clone repo at base_commit, run defn init + ingest. Returns workdir.

    Workdir is arm-scoped (inst__arm, not just inst) -- running both arms
    of the same task concurrently against a shared directory races on the
    initial `git clone` and corrupts whichever arm loses. Found 2026-08-07
    when both arms of the same task launched in parallel: one arm's clone
    failed outright (dir already existed non-empty), and cache/ingest
    interleaving under concurrent load caused a second, separate failure
    mode. Arm-scoping the workdir removes the collision entirely.

    Also corpus-scoped (corpus_label__inst__arm) since 2026-08-20: two
    corpus dirs (e.g. a v5/v6 rerun pair) can legitimately reuse the same
    instance_ids, and this workdir is exactly what score_gitdiff.py diffs
    against base_commit_sha to compute precision/recall/F1. Without the
    corpus label, launching a second corpus over the same instance_ids
    silently overwrote the first corpus's final git state (and its
    .claude-stream.jsonl raw trace) in place -- found 2026-08-20 when a
    v6 rerun made v5's "rescoring" come back byte-identical to v6's own
    results, because both were reading the same clobbered directory.
    """
    inst = task["instance_id"]
    corpus_label = os.path.basename(os.path.normpath(corpus_dir))
    workdir = os.path.join(WORKDIR_ROOT, f"{corpus_label}__{inst}__{arm}")
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

    # files-mode arm gets none of this: no .defn/, no defn-authored CLAUDE.md
    # block. It must be the honest native baseline -- Read/Write/Edit/Bash
    # against a repo that has never seen defn -- or the comparison isn't
    # measuring what CLAUDE.md's "parity is the floor" rule requires (the
    # real native baseline, not defn-on-defn). Found 2026-08-09: this function
    # ran `defn init`/`ingest` unconditionally regardless of arm, so every
    # files-mode workdir got a CLAUDE.md telling it to use `mcp__defn__code`
    # for all Go work -- a tool that arm's allowlist doesn't grant it.
    import shutil

    defn_dir = os.path.join(workdir, ".defn")
    if os.path.isdir(defn_dir):
        shutil.rmtree(defn_dir)

    if arm != "defn":
        return workdir

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


def run_claude(workdir, prompt, budget_usd, max_turns, arm="defn", model="sonnet"):
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
        #
        # model defaults to "sonnet": this bench had been running on the
        # CLI's default (Opus 5, confirmed via the raw stream-json "model"
        # field) with no explicit pin. Every bug found so far -- test op
        # parameter confusion, receiver disambiguation, cross-module name
        # collisions -- is basic tool-API friction, not something that
        # needs frontier-tier reasoning to surface. Pinning Sonnet keeps
        # the bug-finding signal while making exploratory reruns (this
        # bench has needed several in one session) much cheaper. Pass
        # --model opus for the "real", trusted-for-publication comparison
        # per the standing instruction to use Opus for that pass.
        "--model",
        model,
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
    """DEAD CODE, deliberately unused as of 2026-08-17 -- see run_one's
    comment for why it's no longer called. Kept only as a documented
    historical marker in case some future defn regression makes writes
    NOT land on disk in real time again, in which case this is the
    fallback to reach for -- not as a matter of course.

    Originally: "After the agent finishes, `defn emit` writes the
    mutated DB back to .go files so the workdir reflects the agent's
    changes. This lets the correctness scorer diff files." That premise
    was stale by the time this was traced down: every defn write op
    (edit/create/apply/rename/...) already emits its own touched file(s)
    to disk immediately on success (commitOrRollbackOnBuild /
    autoEmitAndBuild), confirmed exhaustively this session via direct
    MCP replay + the DEFN_EMIT_DEBUG instrumentation (internal/emit's
    emitDebugf). This function's own `defn emit workdir` call -- bare,
    unscoped, run AFTER every single defn-arm task regardless of what
    the agent touched -- was the actual, sole source of a real mystery
    that looked exactly like a defn correctness bug: three unrelated
    generated .pb.gw.go files in etcd, whose on-disk import grouping
    doesn't match goimports' canonical output, got silently rewritten
    on every defn-arm run, tanking that run's file-touch precision on
    every single etcd task scored this way -- not because of anything
    defn's request handlers or the agent did, but because this
    unconditional post-step re-normalized the WHOLE project's formatting
    every time. Confirmed by bisecting a live DEFN_EMIT_DEBUG trace:
    the exact three files logged "WROTE ... scoped=false" from a
    completely separate `defn emit <workdir>` CLI invocation -- not
    from anything inside the MCP server's own request handling -- right
    after the agent's Claude process exited (rc=0), with the log's own
    "emitting to ..." banner matching cmdEmit's stdout verbatim.
    """
    try:
        subprocess.check_call(
            ["defn", "emit", workdir],
            cwd=workdir,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except subprocess.CalledProcessError as e:
        print(f"[emit] defn emit failed: {e}", file=sys.stderr)


DISK_SPACE_LOCK_PATH = "/tmp/defn-h2h-disk-space.lock"


def _ensure_disk_space(min_gb=DISK_FREE_MIN_GB):
    """Bail loudly instead of silently losing a task to "no space left
    on device" -- found 2026-08-11: a 15-task prometheus rerun lost 2
    tasks to disk exhaustion mid-run, and one of them didn't even
    surface as an error -- the agent reported it as an "environment
    blocker" in its own final message, scoring as a cheap, clean-looking
    failure instead of the infra problem it actually was. Cleans go's
    build cache and orphaned go-build temp dirs (the actual repeat
    offender -- crashed/killed `go test`/`go vet` runs leave these
    behind) before giving up.

    2026-08-19: wrapped in a flock. This function is check-then-act
    (measure free space, clean if low, measure again) with no
    synchronization -- fine for the strictly-sequential --all loop this
    was written for, but a real TOCTOU race once tasks run concurrently
    (e.g. via `xargs -P<N>` over single-instance invocations, which
    needs no other harness changes to work). Without the lock, N
    concurrent tasks all observe "below threshold" at once, all run
    `go clean -cache` redundantly at the same time, and the post-clean
    free-space number any one of them sees no longer reflects what's
    actually free by the time IT starts consuming disk -- the other
    N-1 processes are consuming concurrently too. The lock only
    serializes this check-and-clean step itself (a few seconds), not
    each task's full disk usage for its whole lifetime -- it does not
    by itself guarantee peak concurrent usage stays under the limit,
    just removes the redundant-cleanup race and gives every caller an
    honest, un-raced measurement to decide against.
    """
    import fcntl
    import glob
    import shutil

    with open(DISK_SPACE_LOCK_PATH, "w") as lock_f:
        fcntl.flock(lock_f, fcntl.LOCK_EX)
        try:
            free_gb = shutil.disk_usage(WORKDIR_ROOT).free / 1e9
            if free_gb >= min_gb:
                return
            print(
                f"[disk] {free_gb:.1f}GB free, below {min_gb}GB -- cleaning caches",
                file=sys.stderr,
            )
            subprocess.run(
                ["go", "clean", "-cache"],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            for entry in glob.glob("/tmp/go-build*"):
                shutil.rmtree(entry, ignore_errors=True)
            free_gb = shutil.disk_usage(WORKDIR_ROOT).free / 1e9
            print(f"[disk] {free_gb:.1f}GB free after cleanup", file=sys.stderr)
        finally:
            fcntl.flock(lock_f, fcntl.LOCK_UN)
    if free_gb < min_gb:
        raise RuntimeError(
            f"only {free_gb:.1f}GB free on {WORKDIR_ROOT}'s filesystem after "
            f"cleanup (need {min_gb}GB) -- resize the volume or free space "
            f"manually before continuing"
        )


def run_one(
    instance_id,
    budget_usd,
    max_turns,
    arm="defn",
    corpus_dir=DEFAULT_CORPUS_DIR,
    model="sonnet",
):
    out_dir = os.path.join(corpus_dir, "arm_defn" if arm == "defn" else "arm_files")
    out_path = os.path.join(out_dir, instance_id + ".json")
    if os.path.exists(out_path):
        print(f"[skip] {out_path} already exists", file=sys.stderr)
        return

    _ensure_disk_space()
    task = load_task(instance_id, corpus_dir)
    workdir = setup_workspace(task, arm=arm, corpus_dir=corpus_dir)
    prompt = build_prompt(task)
    events, rc, elapsed = run_claude(
        workdir, prompt, budget_usd, max_turns, arm=arm, model=model
    )
    traj, cost = events_to_fncall_messages(events)
    # 2026-08-17: deliberately NOT calling apply_edits_via_defn(workdir)
    # here anymore -- see its docstring. Every defn write op already
    # writes its own touched file(s) to disk in real time; a redundant
    # unscoped `defn emit` afterward was the actual source of a real,
    # multi-hour "why does defn touch unrelated generated files" chase
    # that turned out to be this harness re-normalizing the WHOLE
    # project's formatting after every single defn-arm task, not a defn
    # bug. The scorer diffs the live workdir directly (score_gitdiff.py),
    # which already reflects every real-time write -- nothing further
    # needed here for scoring to work correctly.

    os.makedirs(out_dir, exist_ok=True)
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
                "defn_version": _defn_version_string() if arm == "defn" else None,
                "defn_binary_hash": _defn_binary_hash() if arm == "defn" else None,
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
    ap.add_argument(
        "--model",
        default="sonnet",
        help="claude -p --model value (default sonnet, cheap exploratory pass; "
        "pass 'opus' for the real, trusted-for-publication comparison)",
    )
    ap.add_argument(
        "--corpus-dir",
        default=DEFAULT_CORPUS_DIR,
        help="directory containing tasks.jsonl (and where arm_defn/arm_files get written); "
        "default is this script's own directory (the original cli/grpc-go/go-zero corpus)",
    )
    args = ap.parse_args()

    if args.all:
        with open(os.path.join(args.corpus_dir, "tasks.jsonl")) as f:
            tasks = [json.loads(l)["instance_id"] for l in f]
        for i, tid in enumerate(tasks, 1):
            print(f"\n===== [{i}/{len(tasks)}] {tid} =====", file=sys.stderr)
            try:
                run_one(
                    tid,
                    args.budget_usd,
                    args.max_turns,
                    arm=args.arm,
                    corpus_dir=args.corpus_dir,
                    model=args.model,
                )
            except Exception as e:
                print(f"[fail] {tid}: {type(e).__name__}: {e}", file=sys.stderr)
    else:
        if not args.instance_id:
            ap.error("provide instance_id or --all")
        run_one(
            args.instance_id,
            args.budget_usd,
            args.max_turns,
            arm=args.arm,
            corpus_dir=args.corpus_dir,
            model=args.model,
        )


if __name__ == "__main__":
    main()
