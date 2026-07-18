#!/usr/bin/env python3
"""Analyze both arms of the session-cumulative bench.

Expects OUT_DIR/{files,defn}/turn-NN.json — raw stream-json output from
`claude -p --output-format stream-json --verbose ...`.

Emits:
- per-turn table showing input/cache_creation/cache_read/output tokens
- cumulative session totals
- effective billed input (using published Anthropic pricing)
- CSV for plotting

Reads all `usage` fields from assistant messages in each turn file.
Each `--resume` invocation returns ALL prior turns re-streamed too;
we take only the FINAL usage in each file (which represents the total
of that turn's assistant work + prior context).

Actually: on `claude -p --resume`, each stream-json returns messages
from the CURRENT turn only. So we can sum all usage fields per file.
"""
import json, glob, sys, os
from collections import defaultdict

# Sonnet 4.5 pricing per M tokens (approx, subject to change):
#   input:  $3.00
#   cache_creation (5-min ephemeral): $3.75 (1.25x)
#   cache_creation (1h ephemeral):    $6.00 (2.0x)
#   cache_read: $0.30 (0.10x)
#   output: $15.00
# Opus 4.8 pricing:
#   input:  $15.00
#   cache_creation (5m): $18.75
#   cache_creation (1h): $30.00
#   cache_read: $1.50
#   output: $75.00
# The smoke test showed model = claude-opus-4-8, so use Opus rates.
PRICE_PER_M = {
    'opus': {
        'input': 15.00,
        'cache_create_5m': 18.75,
        'cache_create_1h': 30.00,
        'cache_read': 1.50,
        'output': 75.00,
    },
    'sonnet': {
        'input': 3.00,
        'cache_create_5m': 3.75,
        'cache_create_1h': 6.00,
        'cache_read': 0.30,
        'output': 15.00,
    },
}

def parse_arm(arm_dir):
    """Return list of per-turn usage dicts."""
    turns = []
    turn_files = sorted(glob.glob(os.path.join(arm_dir, 'turn-*.json')))
    for tf in turn_files:
        u = {
            'input_tokens': 0,
            'cache_read_input_tokens': 0,
            'cache_creation_5m': 0,
            'cache_creation_1h': 0,
            'output_tokens': 0,
            'assistant_msgs': 0,
            'tool_uses': 0,
        }
        model = None
        for line in open(tf):
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            if obj.get('type') == 'assistant':
                msg = obj.get('message', {}) or {}
                usage = msg.get('usage') or {}
                if not usage:
                    continue
                model = msg.get('model', model)
                u['assistant_msgs'] += 1
                u['input_tokens'] += usage.get('input_tokens', 0)
                u['cache_read_input_tokens'] += usage.get('cache_read_input_tokens', 0)
                cc = usage.get('cache_creation') or {}
                u['cache_creation_5m'] += cc.get('ephemeral_5m_input_tokens', 0)
                u['cache_creation_1h'] += cc.get('ephemeral_1h_input_tokens', 0)
                u['output_tokens'] += usage.get('output_tokens', 0)
                # Count tool_use blocks
                for c in msg.get('content', []) or []:
                    if c.get('type') == 'tool_use':
                        u['tool_uses'] += 1
        u['model'] = model
        u['turn'] = int(os.path.basename(tf).replace('turn-','').replace('.json',''))
        turns.append(u)
    return turns

def cost_usd(u, price):
    return (
        u['input_tokens']       * price['input']          / 1e6 +
        u['cache_creation_5m']  * price['cache_create_5m']/ 1e6 +
        u['cache_creation_1h']  * price['cache_create_1h']/ 1e6 +
        u['cache_read_input_tokens'] * price['cache_read']/ 1e6 +
        u['output_tokens']      * price['output']         / 1e6
    )

def pick_price(model):
    if not model: return PRICE_PER_M['opus']
    if 'opus' in model: return PRICE_PER_M['opus']
    if 'sonnet' in model: return PRICE_PER_M['sonnet']
    return PRICE_PER_M['opus']

def summarize(arm_name, turns):
    price = pick_price(turns[0].get('model') if turns else None)
    print(f'\n=== {arm_name} ({turns[0]["model"] if turns else "?"}) ===')
    totals = defaultdict(int)
    cum_cost = 0.0
    print(f'{"turn":>4s} {"tool_use":>8s} {"input":>8s} {"cc_5m":>8s} {"cc_1h":>8s} {"cache_rd":>10s} {"output":>8s}   ${"cost":>8s}')
    for u in turns:
        c = cost_usd(u, price)
        cum_cost += c
        for k in ['input_tokens','cache_creation_5m','cache_creation_1h','cache_read_input_tokens','output_tokens','tool_uses']:
            totals[k] += u[k]
        print(f'{u["turn"]:4d} {u["tool_uses"]:8d} {u["input_tokens"]:8,} {u["cache_creation_5m"]:8,} {u["cache_creation_1h"]:8,} {u["cache_read_input_tokens"]:10,} {u["output_tokens"]:8,}   ${c:8.4f}')
    print(f'{"TOT":>4s} {totals["tool_uses"]:8d} {totals["input_tokens"]:8,} {totals["cache_creation_5m"]:8,} {totals["cache_creation_1h"]:8,} {totals["cache_read_input_tokens"]:10,} {totals["output_tokens"]:8,}   ${cum_cost:8.4f}')
    return totals, cum_cost, price

def main(argv):
    if len(argv) < 2:
        print("usage: analyze.py <OUT_DIR> [arm-name ...]", file=sys.stderr)
        sys.exit(2)
    out_dir = argv[1]
    arm_names = argv[2:] if len(argv) > 2 else ['files', 'defn', 'defn-forced']
    arms = {}
    for name in arm_names:
        d = os.path.join(out_dir, name)
        if os.path.isdir(d):
            turns = parse_arm(d)
            if turns:
                arms[name] = turns
    if not arms:
        print(f"no arms found in {out_dir}", file=sys.stderr)
        sys.exit(1)

    totals = {}
    for name, turns in arms.items():
        t, c, p = summarize(name.upper(), turns)
        totals[name] = (t, c, p)

    # Cross-arm delta table
    baseline = 'files' if 'files' in arms else list(arms.keys())[0]
    ft, fc, fp = totals[baseline]
    def pct(a, b):
        if b == 0: return '  n/a '
        return f'{100*(a-b)/b:+5.1f}%'
    print(f'\n=== Cross-arm deltas (vs {baseline}) ===')
    header = f'  {"metric":32s}  ' + '  '.join(f'{n:>18s}' for n in arms)
    print(header)
    for k in ['tool_uses','input_tokens','cache_creation_1h','cache_read_input_tokens','output_tokens']:
        row = [f'  {k:32s}']
        for name in arms:
            t = totals[name][0]
            row.append(f'{t[k]:>10,} {pct(t[k], ft[k]):>7s}')
        print('  '.join(row))
    row = [f'  {"USD cost":32s}']
    for name in arms:
        c = totals[name][1]
        row.append(f'${c:>9.4f} {pct(c, fc):>7s}')
    print('  '.join(row))

    # CSV
    csv_path = os.path.join(out_dir, 'session-usage.csv')
    with open(csv_path, 'w') as fh:
        fh.write('arm,turn,tool_uses,input_tokens,cache_creation_5m,cache_creation_1h,cache_read_input_tokens,output_tokens,cost_usd\n')
        for name in arms:
            turns = arms[name]
            price = totals[name][2]
            for u in turns:
                fh.write(f'{name},{u["turn"]},{u["tool_uses"]},{u["input_tokens"]},{u["cache_creation_5m"]},{u["cache_creation_1h"]},{u["cache_read_input_tokens"]},{u["output_tokens"]},{cost_usd(u, price):.6f}\n')
    print(f'\nCSV written: {csv_path}')

if __name__ == '__main__':
    main(sys.argv)
