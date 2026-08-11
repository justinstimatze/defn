# prometheus/prometheus head-to-head: defn vs files-mode

15 hand-curated real prometheus/prometheus bug-fix tasks (issue + linked
merged-PR fix), run through both arms live on 2026-08-09/10 with
`claude -p --model sonnet`, `--budget-usd 3.0 --max-turns 50` per task.

Harness bug found and fixed mid-run: `setup_workspace` in
`bench/head-to-head-go/agent_driver.py` ran `defn init`/`defn ingest`
unconditionally, so the files-mode arm's workdir got a defn-authored
CLAUDE.md telling it to use a tool (`mcp__defn__code`) that arm's
allowlist doesn't grant. All results below are from the fixed harness
(files-mode workdirs verified clean: repo's own pristine `CLAUDE.md`,
no `.defn/`).

## Aggregate (n=15 per arm)

| | defn | files |
|---|---:|---:|
| total cost | $11.90 | $9.88 |
| mean cost | $0.79 | $0.66 |
| median cost | $0.64 | $0.49 |
| mean wall time | 395s | 331s |
| rc==0 | 13/15 | 13/15 |

**Cost ratio (defn/files): 1.20x — defn costs 20% more on this batch.**
This does not meet CLAUDE.md's "parity is the floor" bar. Real-workload
result, not a synthetic sweep.

## Per-task

| instance | defn $ | defn s | defn rc | files $ | files s | files rc |
|---|---:|---:|---:|---:|---:|---:|
| prometheus-12024 | 1.63 | 638 | 0 | 1.74 | 452 | 1 |
| prometheus-16766 | 0.13 | 58 | 0 | 0.51 | 193 | 0 |
| prometheus-17395 | 1.22 | 624 | 0 | 0.32 | 90 | 0 |
| prometheus-18358 | 0.46 | 179 | 0 | 0.70 | 408 | 0 |
| prometheus-18534 | 1.30 | 498 | 0 | 1.59 | 569 | 1 |
| prometheus-18652 | 0.47 | 205 | 0 | 0.85 | 517 | 0 |
| prometheus-18712 | 1.07 | 375 | 1 | 0.31 | 233 | 0 |
| prometheus-18765 | 1.35 | 954 | 1 | 1.16 | 307 | 0 |
| prometheus-18841 | 0.10 | 67 | 0 | 0.17 | 211 | 0 |
| prometheus-18972 | 0.51 | 859 | 0 | 0.43 | 389 | 0 |
| prometheus-19017 | 0.64 | 263 | 0 | 0.37 | 405 | 0 |
| prometheus-19114 | 0.23 | 178 | 0 | 0.18 | 142 | 0 |
| prometheus-19184 | 1.32 | 495 | 0 | 0.49 | 108 | 0 |
| prometheus-19236 | 0.35 | 153 | 0 | 0.62 | 792 | 0 |
| prometheus-19338 | 1.12 | 381 | 0 | 0.43 | 146 | 0 |

`rc!=0` is a budget/turn-cap exhaustion, not necessarily a crash — not
yet cross-checked against whether the fix actually landed (correctness
scoring not run yet, only cost/completion).

## Correctness scoring and root-cause digging (2026-08-10/11)

Ran `score_correctness.py`'s files-touched approximation (P/R/F1 against
each task's gold `fix_patch`) against this batch. First pass showed defn
*losing* on correctness too (mean F1 0.497 vs files' 0.590) — but this
was a scorer artifact, not a real defn deficit: the scorer's
name-to-file resolver needs a live `.defn` DB, which only existed on the
ephemeral EC2 box the original run used, not locally. 47% of defn's
write ops (`edit`/`replace-hunk` by `name`, defn's normal calling
convention) have no `file` arg and were silently scored as touching 0
files. Fixed by adding a gold-patch-name-search fallback
(`bench/prometheus-repo/correctness_scores.json` holds the corrected
run). Corrected original-batch numbers: **defn mean F1 0.842 vs files'
0.590** — defn was more correct on this batch all along, just measured
wrong.

Digging into the trajectories (not just the scores) surfaced three real
defn bugs actively costing money/turns in this exact batch, all fixed
and committed:

1. **Cross-package `interface_dispatch` object-identity mismatch**
   (`internal/resolve/resolve.go`) — `packages.Load(Tests: true)` gives
   the same interface method two different `*types.Object` pointers
   depending on whether the implementer package has its own tests,
   silently breaking `impact`/`test`/`traverse` accuracy for any
   interface-dispatched call into a tested package. Fixed by switching
   `ifaceMethodToImpls` from object-keyed to string-keyed.
2. **`func init()` natural-key collision across sibling files**
   (`internal/store/schema_sqlite.sql`) — two files in the same package
   each declaring their own `init()` (the normal shape for generated
   protobuf/SD-registration code) collided in the DB, corrupting the
   emitted source into duplicate/wrong `init()` bodies. Live symptom:
   `panic: proto: duplicate enum registered` and `discovery: Config
   named "X" is already registered` — hit directly in 4 of 15
   trajectories (17395, 19184, 19338, and indirectly 18765 via a
   related import-collision, see below). Fixed by adding `source_file`
   to the definitions table's unique key.
3. **Emit's regenerate path wrote the whole per-module import union
   into a single file** (`internal/emit/emit.go`) — imports are tracked
   per-module (shared across every file in a package), but a brand-new
   file (e.g. via `code(op:"create")`) has no on-disk content to
   preserve a real per-file import block from, so it got the ENTIRE
   union — including multiple same-local-name-but-different-path
   imports (e.g. several AWS SDK `types` sub-packages) — causing an
   unrecoverable `"types redeclared in this block"` compile error. Hit
   directly in 17395 and 18765. Fixed by filtering to only imports a
   file's own bodies actually reference, deduped by local name.
4. **`test:"X"` silently ignored a `module:` scope hint shaped as a
   full Go import path** (`internal/mcp/server.go`'s
   `testScopeTarget`) — the same shape `module:` takes everywhere else
   in this API, but the resolver only substring-matched relative
   source-file paths, so it fell back to whole-repo `go test ./...`
   every time. Hit 18534, whose whole-repo test run exhausted the
   bench box's disk compiling every unrelated cloud-SDK dependency
   (prometheus vendors AWS/Azure/GCP/DigitalOcean/Scaleway). Fixed by
   trying an exact module-path match first.

## v2 rerun (2026-08-11, defn arm only, v0.26.40, sequential on EC2)

Full defn-arm-only rerun against a build with all four fixes above.
files-mode arm unchanged (not re-run — nothing in its path was touched).
Two tasks were lost to an unrelated EC2 disk-full incident mid-run
(harness/box issue, not a defn bug) and backfilled individually after
resizing the box's volume.

| | defn (orig) | defn (v2) | files (unchanged) |
|---|---:|---:|---:|
| total cost | $11.90 | **$10.63** (-10.7%) | $9.88 |
| mean cost | $0.79 | $0.71 | $0.66 |
| median cost | $0.64 | $0.61 | $0.49 |
| mean wall time | 395s | 304s | 331s |
| rc==0 | 13/15 | 12/15 | 13/15 |
| mean F1 (corrected scorer) | 0.842 | 0.706 | — |
| F1 >= 0.5 | 15/15 | 13/15 | — |

**Cost ratio (defn/files): 1.20x -> 1.08x.** The panic-storm signature
(`"redeclared"` / `"already registered"` / `"duplicate ... registered"`)
is completely gone from all 15 v2 trajectories, vs. 4/15 in the
original — direct confirmation the fixes address a real, live cost
driver in this exact batch, not just a synthetic concern.

Correctness (F1 proxy) came in *lower* in v2, not higher — driven
mostly by `prometheus-12024`, which ran out of budget/turns mid-fix
this run (rc=1, $1.76) after already being a marginal 12-write-op task
in the original run too (rc=0, $1.63) — reads as run-to-run variance on
an already-hard task, not a new regression, but not confirmed either
way. A few other tasks (18765, 19236, 18534) also came in with somewhat
lower F1 despite similar-or-better cost. F1 here is a rough file-touch
proxy (see `score_correctness.py`'s own caveats), not real test-pass
correctness — this result should be read as "cost improvement
confirmed, correctness improvement unconfirmed" rather than a clean
win, pending either a second rerun or real test-pass scoring.

## v3 rerun (2026-08-11, defn arm only, v0.26.40, second confirmatory pass)

Second full sequential rerun, same build as v2, to check whether v2's
apparent cost win was real or noise. It was mostly noise:

| | defn (orig) | defn (v2) | defn (v3) | files (unchanged) |
|---|---:|---:|---:|---:|
| total cost | $11.90 | $10.63 | $11.71 | $9.88 |
| rc==0 | 13/15 | 12/15 | 11/15 | 13/15 |
| mean F1 | 0.842 | 0.706 | 0.746 | — |
| F1 >= 0.5 | 15/15 | 13/15 | 15/15 | — |
| cost ratio (defn/files) | 1.20x | 1.08x | **1.19x** | — |

v3's total cost lands almost exactly back at the original's level —
v2's $10.63 was largely favorable variance on that specific run, not a
reproducible effect. Per-task cost swings by 2-4x in both directions
across all three runs (e.g. 19114: $0.23 -> $0.24 -> $1.04;
12024: $1.63 -> $1.76 -> $1.50) — normal-sized runs of a 15-task,
Sonnet-driven bench have enough inherent variance that a single rerun
cannot be trusted for a "did it get better" verdict either way.

What *did* hold across both reruns, 30/30 trajectories: **zero
occurrences of the panic-storm signature** (`"redeclared"` /
`"already registered"` / `"duplicate ... registered"`) that hit 4/15
trajectories in the original run. That specific, verified defn bug is
fixed and stays fixed. The two "new" per-run failures checked by hand
(18534 in v3: `undefined: parser.ParseExpr`; 18765 across all three
runs: `nic.IPAMIPIDs undefined`) are both agent hallucinations about
external API shapes — defn's build-gate caught and rolled back both
cleanly — not defn bugs.

**Honest bottom line:** the specific bugs found and fixed this session
are real, verified, and confirmed to stay fixed (panic-storm: 4/15 ->
0/15, 0/15). Their *aggregate* cost/correctness impact on this
15-task, n=1-2-run batch is within the batch's own run-to-run noise
floor and cannot be claimed as a settled "defn now costs X% less"
number without either many more reruns (to average out variance) or
switching to a less noisy signal than a 15-task Sonnet-driven
head-to-head.

## v4 rerun (2026-08-11, defn arm only, v0.26.42, third confirmatory pass)

Between v3 and v4, four more bugs were found and fixed by mining the
v2/v3 trajectories themselves (no new bench run needed to find them):
`testScopeTarget` resolving a short hint to the wrong subdirectory,
`testMatchedNothing` discarding a real pass as "NO TESTS MATCHED"
whenever a recursive scope touched an unrelated sibling package, three
common `add-import` op-name misspellings erroring instead of
dispatching, and a flat 60s test timeout that couldn't be raised by
the calling agent (its own suggested remedy is a server-env-var the
MCP caller can't set mid-session). Third full sequential rerun to
check the combined effect:

| | defn (orig) | defn (v2) | defn (v3) | defn (v4) | files |
|---|---:|---:|---:|---:|---:|
| total cost | $11.90 | $10.63 | $11.71 | **$9.90** | $9.88 |
| rc==0 | 13/15 | 12/15 | 11/15 | **13/15** | 13/15 |
| mean F1 | 0.842 | 0.706 | 0.746 | 0.665 | — |
| cost ratio (defn/files) | 1.20x | 1.08x | 1.19x | **1.00x** | — |
| "no tests matched" hits | — | 15 | 18 | **1** | — |
| "TIMED OUT" hits | — | 3 | 1 | **0** | — |
| "unknown op" hits | — | 1 | 10 | **0** | — |

Two results worth separating. The friction-signature counts are a
clean, direct, before/after confirmation that all four v3->v4 fixes
work exactly as intended -- these aren't noisy proxies, they're literal
occurrence counts of the exact strings each fix targets, and every one
dropped to near-zero. `prometheus-18765` (the other import-collision
task) also succeeded for the first time across all four runs (rc=1,
1, 1 -> 0).

The aggregate cost number is more encouraging than v2/v3 but still not
something to over-claim: $9.90 total is effectively exact parity with
files-mode's $9.88 (1.00x), and completion rate (13/15) matches the
original, beating both reruns -- the best result on the two most
concrete metrics across all four runs. Mean F1 is the lowest of the
four, but per-task it's recall-driven (precision stays ~1.0; the agent
edits the right file, just not always every gold file including
docs/tests) rather than wrong-file misses -- consistent with the
proxy's documented weakness (rewards touching every gold-diff file,
not "did the fix work"), and consistent across all four runs, not a
new v4 regression. One genuine miss confirmed by hand: 19236's agent
fixed a plausible-but-wrong package (`discovery/refresh` instead of
`discovery/moby`) -- a real reasoning miss, not a defn or scorer issue.

**Bottom line, sharpened after three reruns:** the panic-storm fix
(0/45 trajectories now) and the four test-tooling-friction fixes
(near-zero occurrence counts, directly measured) are real, verified,
and reproduce cleanly every time. The *aggregate* cost number is still
noisy at n=15x4 runs -- v4's 1.00x is the best result yet and lines up
with what the friction-signature drop would predict, but one run
landing exactly at parity isn't proof the swing has permanently
narrowed versus just being the run where variance happened to favor
defn. Confidence in the specific, per-bug fixes is high; confidence in
"defn is now at cost parity with files-mode" specifically needs the
larger task pool or multi-run averaging already listed below before
it's a claim worth publishing.

## Not yet done

- Real correctness scoring (apply the produced patch, run the actual
  Multi-SWE-bench per-repo test image) instead of the files-touched
  proxy — the proxy's own scorer-bug history this round is a good
  argument for eventually doing this properly.
- A larger task pool (30+) or multiple averaged reruns, if a trustworthy
  aggregate cost/correctness number is wanted — 15 tasks x 3 runs has
  shown too much run-to-run swing to support one on its own, though the
  trend (1.20x -> 1.08x -> 1.19x -> 1.00x) is directionally encouraging.
- Opus-based rerun, per standing instruction to use Opus for the "real"
  comparison later (Sonnet used here as the interim/cheaper pass).

## Data

Raw trajectories: `arm_defn/<instance_id>.json`, `arm_files/<instance_id>.json`
(same `fncall_messages` schema as the existing cli/grpc-go/go-zero corpus).
Use `bench/head-to-head-go/render_comparison.py <instance_id> --files
arm_files/<id>.json --defn arm_defn/<id>.json` for a side-by-side viewer.
