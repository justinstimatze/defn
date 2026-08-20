#!/usr/bin/env python3
"""
render_comparison.py — build a human-readable, side-by-side HTML log of a
files-mode vs defn-mode trajectory pair for the same task, for direct visual
inspection (not just aggregate wire-cost numbers).

Expects both raw trajectories already saved under
  manual-comparison/<instance_id>/files.json
  manual-comparison/<instance_id>/defn.json
(same fncall_messages schema agent_driver.py writes to arm_files/ and
arm_defn/ -- copy or symlink them in under those two names). Task metadata
(problem statement, base commit, org/repo) is looked up from tasks.jsonl by
instance_id, no need to pass it separately.

Usage:
  python3 render_comparison.py <instance_id>
  python3 render_comparison.py <instance_id> --files path/to/files.json --defn path/to/defn.json

Writes manual-comparison/<instance_id>/comparison.html.

Visual design is intentionally plain for now (2026-08-09) -- revisit once
this has been used across many trajectories, not on the first single use.
"""

import argparse
import html
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
TASKS = os.path.join(HERE, "tasks.jsonl")
COMPARE_DIR = os.path.join(HERE, "manual-comparison")


def esc(s):
    return html.escape(str(s), quote=True)


def content_text(c):
    if isinstance(c, list):
        return "".join(x.get("text", "") if isinstance(x, dict) else str(x) for x in c)
    return c or ""


def fmt_json_args(raw):
    try:
        return json.dumps(json.loads(raw), indent=2, ensure_ascii=False)
    except Exception:
        return raw


def label_for(name, args_raw):
    if name == "mcp__defn__code":
        try:
            a = json.loads(args_raw)
            op = a.get("op", "?")
            target = (
                a.get("name")
                or a.get("pattern")
                or a.get("names")
                or a.get("file")
                or ""
            )
            if isinstance(target, list):
                target = ",".join(target)
            return f"code:{op}", target
        except Exception:
            return "code:?", ""
    try:
        a = json.loads(args_raw)
    except Exception:
        a = {}
    target = (
        a.get("file_path")
        or a.get("path")
        or a.get("pattern")
        or a.get("command")
        or ""
    )
    return name, target


def extract(path, mode):
    d = json.load(open(path))
    msgs = d["fncall_messages"]
    steps = []
    cum_in = cum_out = 0
    final_text = ""
    i, n = 0, len(msgs)
    while i < n:
        m = msgs[i]
        if m.get("role") == "assistant":
            text = content_text(m.get("content")).strip()
            tool_calls = m.get("tool_calls") or []
            if not tool_calls and text:
                final_text = text
            for tc in tool_calls:
                fn = tc.get("function", {})
                name = fn.get("name", "")
                args_raw = fn.get("arguments") or ""
                label, target = label_for(name, args_raw)
                cum_in += len(args_raw)
                out = ""
                if i + 1 < n and msgs[i + 1].get("role") == "tool":
                    out = content_text(msgs[i + 1].get("content"))
                    i += 1
                cum_out += len(out)
                steps.append(
                    {
                        "name": name,
                        "label": label,
                        "target": target,
                        "args": args_raw,
                        "in_bytes": len(args_raw),
                        "out": out,
                        "out_bytes": len(out),
                        "cum_total": cum_in + cum_out,
                    }
                )
        i += 1
    return {
        "mode": mode,
        "instance_id": d.get("instance_id"),
        "cost_usd": d.get("cost_usd"),
        "elapsed_sec": d.get("elapsed_sec"),
        "claude_rc": d.get("claude_rc"),
        "steps": steps,
        "total_calls": len(steps),
        "total_in": cum_in,
        "total_out": cum_out,
        "total_bytes": cum_in + cum_out,
        "final_text": final_text,
    }


def load_task_meta(instance_id):
    with open(TASKS) as f:
        for line in f:
            t = json.loads(line)
            if t["instance_id"] == instance_id:
                return {
                    k: t[k]
                    for k in (
                        "instance_id",
                        "org",
                        "repo",
                        "base_commit_sha",
                        "problem_statement",
                    )
                }
    return {
        "instance_id": instance_id,
        "org": "?",
        "repo": "?",
        "base_commit_sha": "?",
        "problem_statement": "",
    }


def step_card(step, idx, accent_class):
    args_pretty = esc(fmt_json_args(step["args"]))
    out_text = step["out"]
    return f"""
<details class="step {accent_class}">
  <summary>
    <span class="step-n">{idx:02d}</span>
    <span class="step-label">{esc(step["label"])}</span>
    <span class="step-target">{esc(step["target"])}</span>
    <span class="step-bytes">+{step["in_bytes"]}B in · +{step["out_bytes"]}B out</span>
    <span class="step-cum">{step["cum_total"]:,}B cum</span>
  </summary>
  <div class="step-body">
    <div class="step-block"><div class="step-block-h">call args</div><pre>{args_pretty}</pre></div>
    <div class="step-block"><div class="step-block-h">response ({step["out_bytes"]:,} bytes)</div><pre>{esc(out_text) if out_text else "(empty)"}</pre></div>
  </div>
</details>"""


def column(arm_data, accent_class):
    cards = "\n".join(
        step_card(s, i + 1, accent_class) for i, s in enumerate(arm_data["steps"])
    )
    final = esc(arm_data["final_text"])[:2000]
    return f"""
<div class="column">
  <div class="column-final"><div class="column-final-h">final message</div><p>{final}</p></div>
  {cards}
</div>"""


CSS = """
:root {
  --paper: #eef1f5; --paper-raised: #ffffff; --ink: #171b21; --ink-dim: #4c5561; --ink-faint: #7c8592;
  --line: #d7dce3; --line-soft: #e6e9ee;
  --files: #c2601f; --files-soft: #f4e3d3; --files-ink: #7a3c11;
  --defn: #1f8f82; --defn-soft: #d9f0ec; --defn-ink: #0f5f56;
  --ok: #2f8f5b;
  --mono: ui-monospace, "SF Mono", "Cascadia Code", "JetBrains Mono", Consolas, monospace;
  --sans: -apple-system, "Segoe UI", "Ubuntu", system-ui, sans-serif;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --paper: #0e1116; --paper-raised: #161b22; --ink: #e6e9ee; --ink-dim: #a6afba; --ink-faint: #6b7684;
    --line: #2a313c; --line-soft: #1e242c;
    --files: #e08a4c; --files-soft: #2e2013; --files-ink: #f0b689;
    --defn: #4dbdae; --defn-soft: #102b28; --defn-ink: #7fd9cc;
    --ok: #5cbf8b;
  }
}
:root[data-theme="dark"] {
  --paper: #0e1116; --paper-raised: #161b22; --ink: #e6e9ee; --ink-dim: #a6afba; --ink-faint: #6b7684;
  --line: #2a313c; --line-soft: #1e242c;
  --files: #e08a4c; --files-soft: #2e2013; --files-ink: #f0b689;
  --defn: #4dbdae; --defn-soft: #102b28; --defn-ink: #7fd9cc;
  --ok: #5cbf8b;
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--paper); color: var(--ink); font-family: var(--sans); font-size: 14px; line-height: 1.5; }
h1, h2, h3 { text-wrap: balance; margin: 0; }
.wrap { max-width: 1400px; margin: 0 auto; padding: 28px 24px 80px; }
.intro { margin-bottom: 20px; }
.intro .eyebrow { font-family: var(--mono); font-size: 12px; letter-spacing: 0.06em; text-transform: uppercase; color: var(--ink-faint); margin-bottom: 6px; }
.intro h1 { font-size: 22px; font-weight: 650; letter-spacing: -0.01em; }
.intro p { color: var(--ink-dim); max-width: 72ch; margin: 10px 0 0; }
.intro .meta { font-family: var(--mono); font-size: 12px; color: var(--ink-faint); margin-top: 8px; }
.summary { position: sticky; top: 0; z-index: 5; background: var(--paper); padding: 14px 0 16px; border-bottom: 1px solid var(--line); margin-bottom: 18px; }
.summary-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 1px; background: var(--line); border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.summary-cell { background: var(--paper-raised); padding: 12px 16px; }
.summary-cell .label { font-size: 11px; letter-spacing: 0.06em; text-transform: uppercase; color: var(--ink-faint); margin-bottom: 6px; }
.summary-row { display: flex; align-items: baseline; gap: 10px; font-variant-numeric: tabular-nums; }
.summary-row .v { font-family: var(--mono); font-size: 18px; font-weight: 600; }
.summary-row .v.files { color: var(--files-ink); }
.summary-row .v.defn { color: var(--defn-ink); }
.summary-row .delta { font-family: var(--mono); font-size: 12px; color: var(--ink-faint); }
.summary-row .delta.better { color: var(--ok); }
.columns { display: grid; grid-template-columns: 1fr 1fr; gap: 18px; align-items: start; }
@media (max-width: 900px) { .columns { grid-template-columns: 1fr; } .summary-grid { grid-template-columns: repeat(2, 1fr); } }
.column-head { display: flex; align-items: center; gap: 8px; padding: 8px 4px 12px; font-family: var(--mono); font-weight: 600; font-size: 13px; }
.column-head .swatch { width: 10px; height: 10px; border-radius: 2px; }
.column-head.files .swatch { background: var(--files); }
.column-head.defn .swatch { background: var(--defn); }
.column-final { background: var(--paper-raised); border: 1px solid var(--line); border-radius: 10px; padding: 12px 14px; margin-bottom: 12px; }
.column-final-h { font-size: 11px; letter-spacing: 0.06em; text-transform: uppercase; color: var(--ink-faint); margin-bottom: 6px; }
.column-final p { margin: 0; font-size: 12.5px; color: var(--ink-dim); white-space: pre-wrap; max-height: 160px; overflow-y: auto; }
details.step { background: var(--paper-raised); border: 1px solid var(--line-soft); border-left: 3px solid var(--line); border-radius: 6px; margin-bottom: 6px; }
details.step.files { border-left-color: var(--files); }
details.step.defn { border-left-color: var(--defn); }
details.step summary { list-style: none; cursor: pointer; display: flex; align-items: center; gap: 10px; padding: 8px 10px; font-family: var(--mono); font-size: 12px; }
details.step summary::-webkit-details-marker { display: none; }
details.step summary:focus-visible { outline: 2px solid var(--defn); outline-offset: 2px; }
.step-n { color: var(--ink-faint); width: 20px; flex: none; }
.step-label { font-weight: 650; flex: none; }
details.step.files .step-label { color: var(--files-ink); }
details.step.defn .step-label { color: var(--defn-ink); }
.step-target { color: var(--ink-dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1 1 auto; min-width: 0; }
.step-bytes { color: var(--ink-faint); flex: none; }
.step-cum { color: var(--ink-faint); flex: none; width: 76px; text-align: right; }
.step-body { padding: 0 10px 12px 40px; display: grid; gap: 8px; }
.step-block-h { font-size: 10px; letter-spacing: 0.05em; text-transform: uppercase; color: var(--ink-faint); margin-bottom: 3px; }
.step-block pre { margin: 0; font-family: var(--mono); font-size: 11.5px; white-space: pre-wrap; word-break: break-word; background: var(--line-soft); border-radius: 6px; padding: 8px 10px; max-height: 320px; overflow: auto; }
.footnote { margin-top: 28px; padding-top: 14px; border-top: 1px solid var(--line); color: var(--ink-faint); font-size: 12px; max-width: 72ch; }
"""


def build(files_path, defn_path, out_path, task_meta):
    files = extract(files_path, "files")
    defn = extract(defn_path, "defn")

    cost_delta = (defn["cost_usd"] - files["cost_usd"]) / files["cost_usd"] * 100
    time_delta = (
        (defn["elapsed_sec"] - files["elapsed_sec"]) / files["elapsed_sec"] * 100
    )
    bytes_delta = (
        (defn["total_bytes"] - files["total_bytes"]) / files["total_bytes"] * 100
    )
    calls_delta = (
        (defn["total_calls"] - files["total_calls"]) / files["total_calls"] * 100
    )

    out = f"""<!doctype html>
<title>files vs defn — live trajectory, {esc(task_meta["instance_id"])}</title>
<style>{CSS}</style>
<div class="wrap">
  <div class="intro">
    <div class="eyebrow">live trajectory comparison &middot; same model, same task, same session</div>
    <h1>{esc(task_meta["org"])}/{esc(task_meta["repo"])} &mdash; {esc(task_meta["instance_id"])}</h1>
    <p>{esc(task_meta["problem_statement"][:280])}&hellip;</p>
    <div class="meta">base commit {esc(task_meta["base_commit_sha"][:12])} &middot; files-mode: Read/Grep/Edit/Bash &middot; defn-mode: single `code` MCP tool only</div>
  </div>
  <div class="summary">
    <div class="summary-grid">
      <div class="summary-cell"><div class="label">cost</div><div class="summary-row"><span class="v files">${files["cost_usd"]:.3f}</span><span class="v defn">${defn["cost_usd"]:.3f}</span><span class="delta {"better" if cost_delta < 0 else ""}">{cost_delta:+.0f}%</span></div></div>
      <div class="summary-cell"><div class="label">wall time</div><div class="summary-row"><span class="v files">{files["elapsed_sec"]:.0f}s</span><span class="v defn">{defn["elapsed_sec"]:.0f}s</span><span class="delta {"better" if time_delta < 0 else ""}">{time_delta:+.0f}%</span></div></div>
      <div class="summary-cell"><div class="label">tool calls</div><div class="summary-row"><span class="v files">{files["total_calls"]}</span><span class="v defn">{defn["total_calls"]}</span><span class="delta {"better" if calls_delta < 0 else ""}">{calls_delta:+.0f}%</span></div></div>
      <div class="summary-cell"><div class="label">wire bytes</div><div class="summary-row"><span class="v files">{files["total_bytes"]:,}</span><span class="v defn">{defn["total_bytes"]:,}</span><span class="delta {"better" if bytes_delta < 0 else ""}">{bytes_delta:+.0f}%</span></div></div>
    </div>
  </div>
  <div class="columns">
    <div><div class="column-head files"><span class="swatch"></span>files-mode &middot; Read / Grep / Edit / Bash</div>{column(files, "files")}</div>
    <div><div class="column-head defn"><span class="swatch"></span>defn-mode &middot; code MCP tool only</div>{column(defn, "defn")}</div>
  </div>
  <div class="footnote">Both runs' exit codes: files={files["claude_rc"]}, defn={defn["claude_rc"]}. Every row above is a real tool call from the actual recorded trajectory &mdash; args and full response are in the expandable detail, nothing here is synthesized or truncated for effect.</div>
</div>
"""
    open(out_path, "w").write(out)
    print(f"wrote {out_path} ({len(out):,} bytes)")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("instance_id")
    ap.add_argument(
        "--files", default=None, help="override path to files-mode trajectory JSON"
    )
    ap.add_argument(
        "--defn", default=None, help="override path to defn-mode trajectory JSON"
    )
    ap.add_argument("--out", default=None, help="override output HTML path")
    args = ap.parse_args()

    task_dir = os.path.join(COMPARE_DIR, args.instance_id)
    files_path = args.files or os.path.join(task_dir, "files.json")
    defn_path = args.defn or os.path.join(task_dir, "defn.json")
    out_path = args.out or os.path.join(task_dir, "comparison.html")

    task_meta = load_task_meta(args.instance_id)
    build(files_path, defn_path, out_path, task_meta)


if __name__ == "__main__":
    main()
