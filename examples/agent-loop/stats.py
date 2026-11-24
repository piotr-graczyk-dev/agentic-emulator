#!/usr/bin/env python3
"""Tables for a series of agent-loop runs: results.md (every run) and summary.md (per arm: the
median and the range of the runs that finished the task).

    stats.py out/ios [out/ios-next ...]      a second directory adds its arms as "<arm> (<dir>)"
"""
import json
import os
import statistics
import sys

ORDER = ["adb", "mobile-mcp", "agent-device", "argent", "agemu"]


def load(d, tag=""):
    """The runs of a series. A run kept as <run>.busy (the host was loaded during it, and it was
    made again) is listed in results.md and left out of the summary."""
    runs = []
    root = os.path.join(d, "runs")

    def key(n):
        arm, i = n.removesuffix(".busy").rsplit("-", 1)
        return arm, int(i), not n.endswith(".busy")

    for name in sorted(os.listdir(root), key=key):
        f = os.path.join(root, name, "summary.json")
        if os.path.exists(f):
            s = json.load(open(f))
            s["busy"] = name.endswith(".busy")
            s["run"] = name.removesuffix(".busy") + (" (%s)" % tag if tag else "")
            s["label"] = s["arm"] + (" (%s)" % tag if tag else "")
            runs.append(s)
    return runs


def rng(vals, fmt):
    if not vals:
        return "-"
    lo, hi = min(vals), max(vals)
    return fmt % lo if fmt % lo == fmt % hi else "%s-%s" % (fmt % lo, fmt % hi)


def med(vals, fmt):
    return fmt % statistics.median(vals) if vals else "-"


def tokens(s):
    t = s.get("tokens") or {}
    return t.get("input", 0) + t.get("cache_read", 0) + t.get("cache_write", 0)


def main():
    dirs = sys.argv[1:]
    if not dirs:
        raise SystemExit(__doc__)
    runs = []
    for i, d in enumerate(dirs):
        runs += load(d, "" if i == 0 else os.path.basename(os.path.normpath(d)))
    out = dirs[0]

    with open(os.path.join(out, "results.md"), "w") as f:
        f.write("| run | done | total | in tool calls | model | turns | tool calls | tokens in | tokens out | cost | host load (1, 5, 15 min) |\n")
        f.write("|---|---|---|---|---|---|---|---|---|---|---|\n")
        for s in runs:
            done = "yes" if s["ok"] else "no: " + s.get("error", "").replace("|", "/")[:120]
            if s["busy"]:
                done += " (host busy: not counted, run again)"
            f.write("| %s | %s | %.1f s | %.1f s | %.1f s | %d | %d | %s | %s | $%.2f | %s |\n" % (
                s["run"], done, s["wall_s"], s.get("tool_s", 0), s.get("model_s", 0), s.get("turns", 0), s.get("tool_calls", 0),
                "{:,}".format(tokens(s)), "{:,}".format((s.get("tokens") or {}).get("output", 0)), s.get("cost_usd", 0),
                s.get("loadavg", "")))

    labels = []
    for s in runs:
        if s["label"] not in labels:
            labels.append(s["label"])
    labels.sort(key=lambda l: (ORDER.index(l.split(" ")[0]) if l.split(" ")[0] in ORDER else 99, l))
    rows = [("task done", lambda ok, all_: "%d of %d" % (len(ok), len(all_))),
            ("total time, median", lambda ok, _: med([s["wall_s"] for s in ok], "%.0f s")),
            ("total time, range", lambda ok, _: rng([s["wall_s"] for s in ok], "%.0f") + " s"),
            ("in tool calls, median", lambda ok, _: med([s["tool_s"] for s in ok], "%.1f s")),
            ("model, median", lambda ok, _: med([s["model_s"] for s in ok], "%.0f s")),
            ("turns", lambda ok, _: rng([s["turns"] for s in ok], "%d")),
            ("tool calls", lambda ok, _: rng([s["tool_calls"] for s in ok], "%d")),
            ("median tool call", lambda ok, _: rng([s["median_call_s"] * 1000 for s in ok], "%.0f") + " ms"),
            ("input tokens, median (thousands)", lambda ok, _: med([tokens(s) / 1000 for s in ok], "%.0f")),
            ("output tokens, median (thousands)", lambda ok, _: med([s["tokens"]["output"] / 1000 for s in ok], "%.1f")),
            ("cost at list price, median", lambda ok, _: med([s["cost_usd"] for s in ok], "$%.2f"))]
    with open(os.path.join(out, "summary.md"), "w") as f:
        f.write("| | " + " | ".join(labels) + " |\n|---|" + "---|" * len(labels) + "\n")
        for name, fn in rows:
            cells = []
            for l in labels:
                all_ = [s for s in runs if s["label"] == l and not s["busy"]]
                cells.append(fn([s for s in all_ if s["ok"]], all_))
            f.write("| %s | %s |\n" % (name, " | ".join(cells)))
    print(open(os.path.join(out, "summary.md")).read())


if __name__ == "__main__":
    main()
