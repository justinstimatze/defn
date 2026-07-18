# 2026-07-17 — chi-explore session bench: expand op + phantom-callers fix

**Result headline:** phantom-callers fix (`be4baa8`) compressed the
defn-vs-files gap from **+40% (turns.txt, 2026-07-11)** down to
**+12-13% (chi-explore, 2026-07-17)** on Opus 4.8. Expand op v1 did
not add value on top — defn-natural (10 tool calls) beat expand-forced
(13 tool calls) and matched it on cost. Remaining gap traced to
`cache_creation_1h` shape (+21-28%), not tool count.

## Setup

- **Repo:** `github.com/go-chi/chi` (fresh clone per arm from HEAD)
- **Turns:** `chi-explore.txt` — 10 exploration-heavy turns
  (understanding chi's routing, callers, interfaces, extension points)
- **Rigid script**, same prompts across all three arms, ground-truth
  answer key in `chi-explore-ground-truth.md`
- **Arms:**
  - `files` — empty MCP, `Read`/`Bash`/`Write`/`Edit`/etc.
  - `defn-natural` — `.mcp.json` mounting `mcp__defn__code`
  - `defn-expand-forced` — same MCP + `--append-system-prompt`
    prescribing `code(op:"expand")` for multi-hop reads
- **Model:** `claude-opus-4-8` (Claude Code default)
- **Harness:** `run-arm.sh` (2026-07-11)

## Cost table (Opus 4.8 rates)

    Arm                   tool_uses  input   cache_creation_1h  cache_read  output   USD
    files                    13         80             227,034   2,247,096    417   $10.2141
    defn-natural             10         79             275,979   2,141,421    105   $11.5006
    defn-expand-forced       13         71             291,763   1,853,515    130   $11.5440

## Deltas (vs files)

    metric                 defn-natural       defn-expand-forced
    tool_uses              -23.1% (10)        +0.0% (13)
    input_tokens            -1.2%             -11.2%
    cache_creation_1h      +21.6%             +28.5%
    cache_read              -4.7%             -17.5%
    output_tokens          -74.8%             -68.8%
    USD cost              +12.6% ($11.50)    +13.0% ($11.54)

## Per-phase breakdown (early vs tail)

Expand's mechanism (kill turns → save cache-read compounding) shows
up in the tail cost, not aggregate:

    Phase           files   defn-natural  defn-expand-forced
    Turn 1-2        $5.94    $7.20         $9.14
    Turn 3-10 tail  $4.28    $4.30         $2.40  (-44% vs files)
    Total          $10.21   $11.50        $11.54

- Files-mode front-loads reads in turn 1 (8 tools), then answers 6/8
  remaining turns from context alone.
- Defn-natural front-loads slightly less (7 tools turn 1), but tail
  ties with files.
- Defn-expand-forced front-loads MORE (13 tools) because the preamble
  makes the model reach for expand aggressively; then tail is 44%
  cheaper than files. Crossover ~turn 12-14 — longer sessions would
  break even.

## Correctness

Spot-check on turn 2 (routeHTTP callers — the pattern most affected
by the phantom-callers bug):

- All three arms correctly identify **2 direct callers**
  (updateRouteHandler + handle), **no test callers**, and note that
  routeHTTP is only ever wrapped in http.HandlerFunc, never called
  directly. Defn-expand-forced explicitly notes "There are also no
  interface-dispatch callers" — evidence the fix is showing up
  cleanly in the live output.

No spot-checked answers show phantom-caller confusion in any arm.

## What the phantom-callers fix bought

Compared to 2026-07-11 turns.txt (same harness, different workload):

    Bench                                   defn-vs-files gap
    turns.txt (write-heavy, pre-fix)        +28% to +40%
    chi-explore (read-heavy, post-fix)      +12% to +13%

Roughly 2× compression of the gap. Two variables changed at once
(workload + fix), but the primary attribution:

- Workload change alone (add exploratory reads, drop writes) would
  hurt defn-natural on cache_creation (more reads = more cache
  fills). The fact that defn-natural is BETTER on this workload
  suggests the fix is doing most of the work.
- Post-fix, `impact` and `expand` no longer waste turns chasing
  phantom callers. The model gets a precise answer in one call.

## Why expand-v1 didn't help

The `expand` preamble was designed to collapse read+impact chains
into one call. In practice on this workload:

1. Model in defn-natural already substituted defn ops for
   Read/Bash aggressively — no additional prescription needed.
2. Forcing `expand` shifted the model from a small number of
   targeted `impact` or `read` calls into a larger number of
   `expand` calls in turn 1. Expand outputs are bigger per call
   (body + callers together), inflating cache_creation.
3. The tail win (-44%) shows expand's mechanism DOES work — the
   accumulated context is smaller and re-billing is cheaper — but
   turn 1 front-loading eats the win at 10 turns.

**Do not promote expand in the preamble.** Model doesn't need the
push and forcing it costs more than it saves. Keep expand as an
available op; let the model reach for it when appropriate.

## Remaining gap: cache_creation_1h

Both defn arms lose +21-28% on `cache_creation_1h`. This is where
the remaining ~13% overall gap comes from. Root causes to
investigate:

1. **Larger per-op outputs**. Even after the phantom fix, defn's
   markdown format has more overhead per response than files-mode's
   raw file bytes.
2. **1h cache block boundaries.** Anthropic's cache-creation
   cost is 20× cache-read. Each new tool output that spans a
   cache boundary re-creates a block.
3. **Terser messages don't help create-side.** Defn arms produce
   68-75% fewer output tokens, but that doesn't move cache_creation
   — that's set by tool result content, not model messages.

## What this means for shipping

**Ship (already committed as `be4baa8`):**
- Phantom-callers fix. Universally better `impact`/`expand`.

**Do NOT ship expand preamble prescription.**
- Model uses defn naturally without it.
- Forcing it via `--append-system-prompt` slightly worsens cost.

**Keep expand op v1 (already shipped as `0805cb8`).**
- Available for the model to reach for.
- Neutral to slightly positive on tail cost.

**Next work to close the 13% gap: cache_creation_1h shape.**
- Investigate whether smaller per-op output would reduce
  cache-creation cost enough to reach parity.
- Candidate: promote `full: false` mode (no body, just header +
  location) as default for read/expand.
- Bench-gate any change against this baseline.

## Reproducing

```bash
# From /home/justin/Documents/defn (defn binary built at HEAD)
BENCH_DIR=/tmp/chi-explore-run
mkdir -p $BENCH_DIR
cd $BENCH_DIR
git clone --depth 1 https://github.com/go-chi/chi.git chi-files
git clone --depth 1 https://github.com/go-chi/chi.git chi-defn
(cd chi-defn && defn ingest .)
cat > $BENCH_DIR/empty-mcp.json <<'EOF'
{"mcpServers": {}}
EOF
# Author chi-defn/.mcp.json pointing at /home/justin/go/bin/defn
# with DEFN_DB=$BENCH_DIR/chi-defn/.defn — see 2026-07-11 setup.

cd /home/justin/Documents/defn/bench/session-cumulative
TURNS_FILE=./chi-explore.txt OUT_DIR=$BENCH_DIR/out/files \
  ./run-arm.sh files $BENCH_DIR/chi-files --mcp-config=$BENCH_DIR/empty-mcp.json

TURNS_FILE=./chi-explore.txt OUT_DIR=$BENCH_DIR/out/defn-natural \
  ./run-arm.sh defn-natural $BENCH_DIR/chi-defn --mcp-config=$BENCH_DIR/chi-defn/.mcp.json

PREAMBLE=$(cat ./expand-preamble.md)
TURNS_FILE=./chi-explore.txt OUT_DIR=$BENCH_DIR/out/defn-expand-forced \
  ./run-arm.sh defn-expand-forced $BENCH_DIR/chi-defn \
    --mcp-config=$BENCH_DIR/chi-defn/.mcp.json --append-system-prompt="$PREAMBLE"

./analyze.py $BENCH_DIR/out files defn-natural defn-expand-forced
```

## Data

- Per-turn stream-json preserved at
  `/tmp/claude-1001/.../scratchpad/chi-explore-run/out/*/turn-NN.json`
- CSV: `chi-explore-run/out/session-usage.csv`
