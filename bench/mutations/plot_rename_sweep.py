"""Render receipt plots from a size-sweep CSV.

Usage:
    ../../.venv-bench/bin/python3 plot_rename_sweep.py 2026-07-10-rename-sweep.csv

Emits three SVGs alongside the CSV:
    <stem>-correctness.svg          — bar chart of ok/n by (size, mode)
    <stem>-input-tokens.svg         — mean input tokens by (size, mode), with sample points
    <stem>-cost-per-correct.svg     — total input tokens / correct runs, by size + mode

The three together are the honest picture: near-parity on raw
input-token cost, wide gap on correctness, and a runaway cost-per-
correct-edit for files-mode at larger scatter. No hand-picked stat.
"""

import csv
import sys
from collections import defaultdict
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt


def load(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            r["loc_actual"] = int(r["loc_actual"])
            r["input_tokens"] = int(r["input_tokens"])
            r["correct"] = r["correct"] == "true"
            rows.append(r)
    return rows


def bymode(rows):
    per = defaultdict(lambda: defaultdict(list))  # per[mode][loc] = [row, ...]
    for r in rows:
        per[r["mode"]][r["loc_actual"]].append(r)
    return per


COLORS = {"files": "#c74c4c", "defn": "#3d7ac2"}


def plot_correctness(rows, stem):
    per = bymode(rows)
    locs = sorted({r["loc_actual"] for r in rows})
    width = 0.4
    fig, ax = plt.subplots(figsize=(8, 4.5))
    x = list(range(len(locs)))
    for i, mode in enumerate(("files", "defn")):
        vals = [
            sum(1 for r in per[mode][loc] if r["correct"]) / max(1, len(per[mode][loc]))
            for loc in locs
        ]
        offset = (i - 0.5) * width
        ax.bar([xi + offset for xi in x], vals, width, label=mode, color=COLORS[mode])
    ax.set_xticks(x)
    ax.set_xticklabels([str(l) for l in locs])
    ax.set_xlabel("fixture LOC")
    ax.set_ylabel("fraction correct")
    ax.set_ylim(0, 1.05)
    ax.set_title("rename-param: correctness vs fixture LOC")
    ax.legend(loc="lower left")
    ax.grid(axis="y", alpha=0.3)
    fig.tight_layout()
    out = Path(f"{stem}-correctness.svg")
    fig.savefig(out)
    print(f"wrote {out}")


def plot_input_tokens(rows, stem):
    per = bymode(rows)
    locs = sorted({r["loc_actual"] for r in rows})
    fig, ax = plt.subplots(figsize=(8, 4.5))
    for mode in ("files", "defn"):
        means = [
            sum(r["input_tokens"] for r in per[mode][loc]) / len(per[mode][loc])
            for loc in locs
        ]
        ax.plot(locs, means, "o-", label=mode, color=COLORS[mode])
        for loc in locs:
            samples = [r["input_tokens"] for r in per[mode][loc]]
            ax.scatter(
                [loc] * len(samples), samples, s=18, color=COLORS[mode], alpha=0.35
            )
    ax.set_xscale("log")
    ax.set_xlabel("fixture LOC (log scale)")
    ax.set_ylabel("input tokens (mean, per sample dots)")
    ax.set_title("rename-param: input tokens vs fixture LOC")
    ax.legend()
    ax.grid(True, alpha=0.3)
    fig.tight_layout()
    out = Path(f"{stem}-input-tokens.svg")
    fig.savefig(out)
    print(f"wrote {out}")


def plot_cost_per_correct(rows, stem):
    per = bymode(rows)
    locs = sorted({r["loc_actual"] for r in rows})
    fig, ax = plt.subplots(figsize=(8, 4.5))
    for mode in ("files", "defn"):
        vals = []
        for loc in locs:
            rr = per[mode][loc]
            total_tokens = sum(r["input_tokens"] for r in rr)
            n_correct = sum(1 for r in rr if r["correct"])
            vals.append(total_tokens / n_correct if n_correct else float("nan"))
        ax.plot(locs, vals, "o-", label=mode, color=COLORS[mode])
        # Note: NaN gaps are visible; annotate the size where files-mode had 0 correct.
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_xlabel("fixture LOC (log scale)")
    ax.set_ylabel("input tokens / correct edit (log scale)")
    ax.set_title("rename-param: cost per correct edit (gated on correctness)")
    ax.legend()
    ax.grid(True, alpha=0.3, which="both")
    # Annotate any missing points as ∞
    for mode in ("files", "defn"):
        for loc in locs:
            rr = per[mode][loc]
            n_correct = sum(1 for r in rr if r["correct"])
            if n_correct == 0:
                ax.annotate(
                    "∞ (0 correct)",
                    (loc, ax.get_ylim()[1] * 0.7),
                    color=COLORS[mode],
                    ha="center",
                    fontsize=9,
                    xytext=(0, -6),
                    textcoords="offset points",
                )
    fig.tight_layout()
    out = Path(f"{stem}-cost-per-correct.svg")
    fig.savefig(out)
    print(f"wrote {out}")


def main():
    if len(sys.argv) < 2:
        print("usage: plot_rename_sweep.py <sweep.csv>", file=sys.stderr)
        sys.exit(1)
    csv_path = Path(sys.argv[1])
    rows = load(csv_path)
    stem = csv_path.stem
    plot_correctness(rows, stem)
    plot_input_tokens(rows, stem)
    plot_cost_per_correct(rows, stem)


if __name__ == "__main__":
    main()
