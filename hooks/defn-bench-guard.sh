#!/bin/bash
# Claude Code hook: block LOCAL invocation of defn's bench trajectory
# driver — agent_driver.py / launch_arm.sh / launch_arm_parallel.sh.
# Place in .claude/settings.json hooks.PreToolUse, matcher "Bash".
#
# WHY THIS EXISTS: memory alone (a winze-memory entry reinforced 5 times
# across separate sessions between 2026-08-09 and 2026-08-24) did not stop
# this same mistake from recurring — each time, a bench trajectory batch
# got launched directly on the local laptop instead of the defn-bench EC2
# box, pegging an 8-core machine to load average 8+ and making any
# wall-clock numbers from the run unreliable (the whole reason the EC2 box
# exists: local memory pressure/swap skews wall-time by 2-3x). Recall alone
# is reactive — it only surfaces *after* the command is already typed.
# Moving enforcement to the harness makes it impossible to skip by not
# thinking of it in the moment, same rationale as defn-go-guard.sh.
#
# THREAT MODEL: same as defn-go-guard.sh — an adoption-nudge for a
# cooperative-but-habitual model, not a security sandbox. Regex-based
# command classification can be defeated by obfuscation; this covers the
# common, unobfuscated invocation shapes and stops there deliberately.
#
# Escape hatch: DEFN_BENCH_LOCAL_OK=1 set on the command allows ONE local
# run to proceed (not single-use like the go-guard sentinel — this is
# meant for a deliberate, already-decided exception, e.g. "EC2 is down and
# we're doing a tiny one-task smoke test on purpose"). DEFN_GUARD=0 (the
# same kill switch defn-go-guard.sh honors) disables this hook too.
if [ "${DEFN_GUARD:-1}" = "0" ]; then
    exit 0
fi

INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // empty')

[ "$TOOL" = "Bash" ] || exit 0

CMD=$(echo "$INPUT" | jq -r '.tool_input.command // empty')

# Already going over ssh (to the EC2 box or anywhere else) -- that's the
# correct shape, let it through regardless of what runs on the far end.
if echo "$CMD" | grep -Eq '(^|[;&|]|\s)ssh\s'; then
    exit 0
fi

# An explicit, deliberate override for this one call.
if echo "$CMD" | grep -Eq '(^|[;&|]|\s)DEFN_BENCH_LOCAL_OK=1(\s|$)'; then
    exit 0
fi

if echo "$CMD" | grep -Eq '(^|[;&|]|\s)([./a-zA-Z0-9_/-]*(agent_driver\.py|launch_arm(_parallel)?\.sh))(\s|$)'; then
    echo "[defn-bench-guard] agent_driver.py/launch_arm(_parallel).sh must run on the defn-bench EC2 box, not locally -- local runs peg this machine (load avg 8+ observed) and produce unreliable wall-clock numbers (the EC2 box exists specifically to avoid local memory-pressure skew). SSH in first: see the 'EC2 bench box connection details' memory for the current IP/key, or run \`aws ec2 describe-instances\` to find it (start the instance first if it's stopped). If this is a deliberate, already-decided local exception (e.g. EC2 is down and this is a genuine tiny one-task smoke test), prefix the command with DEFN_BENCH_LOCAL_OK=1." >&2
    exit 2
fi

# Heavy local `go test` runs -- a SEPARATE memory ("Scope test runs,
# don't default to full suite") reinforced this same class of mistake 4
# times on its own: internal/mcp alone regularly takes 300-500s, and a
# broad local run pegs the machine the same way agent_driver.py does,
# just via CPU/wall-time instead of API cost. Added 2026-08-24 after it
# happened a 5th time (a `go test ./...` launched immediately after this
# very hook was written for the OTHER half of the same problem class).
# Block anything not clearly scoped to a small, specific set of tests:
#   - `./...` anywhere (whole-repo or whole-subtree recursive) -- always broad.
#   - no -run flag at all -- runs everything in the targeted package(s).
#   - a -run pattern containing `|` (alternation) -- looks scoped but can
#     still match hundreds of tests; this exact false confidence is what
#     bit the project the first 4 times.
# A -run with a single, non-alternated pattern (typically one exact test
# name) is let through as the "genuinely tiny, single-test-or-few
# verification" case the memory itself carves out as fine locally.
if echo "$CMD" | grep -Eq '(^|[;&|]|\s)go\s+test\b'; then
    if echo "$CMD" | grep -Eq '/\.\.\.'; then
        echo "[defn-bench-guard] \`go test\` with a recursive './...' target must run on the defn-bench EC2 box, not locally -- internal/mcp alone regularly takes 300-500s and this repeatedly pegs the laptop. SSH to EC2 (see 'EC2 bench box connection details' memory), or scope this to a single test with -run '^TestExactName\$' if this really is a tiny check. Override: DEFN_BENCH_LOCAL_OK=1." >&2
        exit 2
    fi
    if ! echo "$CMD" | grep -Eq -- '-run[= ]'; then
        echo "[defn-bench-guard] \`go test\` with no -run flag runs every test in the target package(s) -- must run on EC2, not locally (same reason as './...' above). Scope with -run '^TestExactName\$' for a genuine quick local check, or SSH to EC2 for anything broader. Override: DEFN_BENCH_LOCAL_OK=1." >&2
        exit 2
    fi
    if echo "$CMD" | grep -Eq -- '-run[= ][^ ]*\|'; then
        echo "[defn-bench-guard] \`go test -run\` with a '|'-alternated pattern can still match dozens/hundreds of tests despite looking scoped -- this exact false confidence has bitten this project multiple times already. Must run on EC2 unless you're certain it matches only a handful tests. Override: DEFN_BENCH_LOCAL_OK=1." >&2
        exit 2
    fi
fi

exit 0
