# plancheck-3c — semantic-summary A/B bench (#160 stage 3c)

Decision this bench flips: **should semantic-summary reads become the
default for `code(op:"read")` in defn?**

Two conditions per task, same task set:

- **on** — `DEFN_SUMMARY_READ=1` (Haiku one-liners returned in place of
  bodies until the caller explicitly asks for `full: true`)
- **off** — `DEFN_SUMMARY_READ=0` (current behavior)

Metric per condition:

- **recall** — of the ground-truth files that a correct plan must
  touch, how many appear in the plan's `filesToRead` ∪ `filesToModify`
  ∪ `filesToCreate` after the session's read budget is spent.
- **wall** — end-to-end task wall (session start → plan JSON emitted).
- **tokens** — session input+output as reported by the harness.

Decision rule: flip default if recall drop <1pp AND wall/tokens
improved by ≥15% (to be worth the Haiku spend + backfill worker).

## Layout

```
tasks.yaml        # canonical task set (start small; grow)
harness_test.go   # Go test harness, mirrors bench/retrieval/ shape
producer.go       # session driver stub (see PRODUCER STUB)
scoring.go        # plancheck call + recall calc
```

## PRODUCER STUB

The `producer.go` step — "give a task, get back a plan JSON" — is
currently stubbed to enumerate ground truth directly (so a shakedown
run should show recall=1.0 both conditions). The real producer must
spawn a Claude Code subprocess, feed it the task prompt, and parse the
emitted plan JSON. Two candidate implementations left open:

1. `claude-agent-sdk` in-process (fast, but same host as harness — CPU
   contention risk noted in CLAUDE.md)
2. `claude-code --print` subprocess per task (clean isolation, slower
   startup, real user shape)

Pick when the shakedown recall/wall numbers make sense.

## Running

```bash
# shakedown (stub producer, ~seconds, no API spend)
go test ./bench/plancheck-3c/ -run TestPlancheck3c -v

# real (once producer is wired, needs ANTHROPIC_API_KEY, ~$10-60)
PLANCHECK_3C_REAL=1 go test ./bench/plancheck-3c/ -run TestPlancheck3c -v -timeout 4h
```
