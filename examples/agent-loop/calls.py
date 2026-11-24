#!/usr/bin/env python3
"""One agent-loop run's tool calls, from its stream.jsonl.

    calls.py out/ios/runs/agemu-1 [width]     in order, with what the agent said and what came back
    calls.py --jsonl out/ios/runs/agemu-1     one JSON object per call: start, end, tool, args, error

The device argument is left out of the arguments (it is the same in every call).
"""
import json
import os
import sys


def events(d):
    for line in open(os.path.join(d, "stream.jsonl")):
        r = json.loads(line)
        ev = r.get("ev") or {}
        if ev.get("type") == "assistant":
            for b in ev["message"]["content"]:
                if b["type"] == "tool_use":
                    args = {k: v for k, v in b["input"].items() if k not in ("device", "udid")}
                    yield "call", r["t"], b["id"], b["name"].split("__")[-1], args
                elif b["type"] == "text" and b["text"].strip():
                    yield "says", r["t"], None, None, b["text"].strip()
        elif ev.get("type") == "user" and isinstance(ev["message"].get("content"), list):
            for b in ev["message"]["content"]:
                if b.get("type") == "tool_result":
                    t = b.get("content")
                    if isinstance(t, list):
                        t = " ".join(x.get("text", "<image>") for x in t)
                    yield "result", r["t"], b.get("tool_use_id"), bool(b.get("is_error")), str(t)


def main():
    argv = sys.argv[1:]
    as_jsonl = argv[:1] == ["--jsonl"]
    if as_jsonl:
        argv = argv[1:]
    if not argv:
        raise SystemExit(__doc__)
    d = argv[0]
    width = int(argv[1]) if len(argv) > 1 else 150
    if as_jsonl:
        calls, by_id = [], {}
        for kind, t, call_id, a, b in events(d):
            if kind == "call":
                by_id[call_id] = {"start": t, "end": None, "tool": a, "args": b, "error": False}
                calls.append(by_id[call_id])
            elif kind == "result" and call_id in by_id:
                by_id[call_id]["end"], by_id[call_id]["error"] = t, a
        for c in calls:
            print(json.dumps(c, ensure_ascii=False))
        return
    for kind, t, _, a, b in events(d):
        if kind == "call":
            print("%6.1f  %-12s %s" % (t, a, json.dumps(b, ensure_ascii=False)[:width]))
        elif kind == "says":
            print("%6.1f  SAYS %s" % (t, b[:width].replace("\n", " ")))
        else:
            print("%6.1f     -> %s%s" % (t, "ERROR " if a else "", b[:width].replace("\n", " | ")))


if __name__ == "__main__":
    main()
