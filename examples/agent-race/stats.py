#!/usr/bin/env python3
"""Per-arm ranges over a platform's successful runs: task time, tool calls, median call, device
CPU and memory, the tool's host processes' CPU. Writes <dir>/summary.md and prints it.

    python3 examples/agent-race/stats.py examples/agent-race/out/android
"""
import json
import os
import statistics
import sys

ORDER = ["classic", "agent-device", "argent", "agemu"]
NAMES = {"classic": "standard + adb", "agent-device": "agent-device", "argent": "Argent", "agemu": "agemu"}


def run_stats(d):
    s = json.load(open(os.path.join(d, "summary.json")))
    ev = [json.loads(l) for l in open(os.path.join(d, "events.jsonl"))]
    sm = [json.loads(l) for l in open(os.path.join(d, "samples.jsonl"))]
    cpu = plane = 0.0
    prev = 0.0
    for x in sm:
        if 0 < x["t"] <= s["total_s"] + 0.25:
            dt = x["t"] - max(prev, 0.0)
            cpu += x["emu_cpu"] / 100 * dt
            plane += x["plane_cpu"] / 100 * dt
        prev = x["t"]
    feet = [x["emu_footprint_mb"] for x in sm if x["emu_footprint_mb"] and 0 < x["t"] <= s["total_s"]]
    return {
        "ok": s["ok"], "total": s["total_s"], "calls": len(ev),
        "median_call": statistics.median(e["end"] - e["start"] for e in ev) if ev else 0,
        "cpu": cpu, "plane": plane, "mem": statistics.median(feet) / 1024 if feet else 0,
    }


def rng(vals, fmt):
    lo, hi = min(vals), max(vals)
    return fmt.format(lo) if fmt.format(lo) == fmt.format(hi) else fmt.format(lo) + "-" + fmt.format(hi)


def main():
    out = sys.argv[1]
    runs = {}
    for d in sorted(os.listdir(os.path.join(out, "runs"))):
        p = os.path.join(out, "runs", d)
        if os.path.exists(os.path.join(p, "summary.json")):
            runs.setdefault(d.rsplit("-", 1)[0], []).append(run_stats(p))
    arms = [a for a in ORDER if a in runs]
    agemu = [r["total"] for r in runs.get("agemu", []) if r["ok"]]
    med = lambda ok: statistics.median(r["total"] for r in ok)
    rows = [
        ("runs ok", lambda a, rs, ok: f"{len(ok)}/{len(rs)}"),
        ("task time", lambda a, rs, ok: rng([r["total"] for r in ok], "{:.1f}") + " s"),
        ("agemu faster by (medians)", lambda a, rs, ok: "-" if a == "agemu" or not agemu else
            f"{med(ok) / statistics.median(agemu):.1f}×"),
        ("tool calls", lambda a, rs, ok: rng([r["calls"] for r in ok], "{}")),
        ("median tool call", lambda a, rs, ok: rng([r["median_call"] * 1000 for r in ok], "{:.0f}") + " ms"),
        ("device CPU for the task", lambda a, rs, ok: rng([r["cpu"] for r in ok], "{:.0f}") + " CPU-s"),
        ("device memory (phys_footprint)", lambda a, rs, ok: rng([r["mem"] for r in ok], "{:.2f}") + " GB"),
        ("tool's host processes, CPU", lambda a, rs, ok: rng([r["plane"] for r in ok], "{:.1f}") + " CPU-s"),
    ]
    lines = ["| | " + " | ".join(NAMES[a] for a in arms) + " |", "|---" * (len(arms) + 1) + "|"]
    for label, f in rows:
        cells = []
        for a in arms:
            ok = [r for r in runs[a] if r["ok"]]
            cells.append(f(a, runs[a], ok) if ok else "-")
        lines.append(f"| {label} | " + " | ".join(cells) + " |")
    text = "\n".join(lines) + "\n"
    open(os.path.join(out, "summary.md"), "w").write(text)
    print(text)


if __name__ == "__main__":
    main()
