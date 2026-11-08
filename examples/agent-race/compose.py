#!/usr/bin/env python3
"""Side-by-side video of 2-4 agent-race runs, in real time.

    uv run -q --with pillow examples/agent-race/compose.py \
        --run classic=runs/classic-2 --run agent-device=runs/agent-device-1 \
        --run argent=runs/argent-1 --run agemu=runs/agemu-2 \
        --title "Android · one agent task, four tool stacks" --out race.mp4 --poster poster.png

    compose.py --left runs/classic-2 --right runs/agemu-2 --out race.mp4   # old two-arm call

Columns follow the order of --run. Each side shows the recorded screen, the elapsed time, the
current step, the tool calls as they run (the running one with a live timer) and the device's CPU
and memory; bars at the bottom show every side's progress through the plan, and a closing card
compares the runs once the slowest one has finished. Two runs get the wide side-by-side layout,
three or four get columns. Needs Pillow and ffmpeg. montage.py reuses Race and the card helpers.
"""
import argparse
import bisect
import json
import os
import subprocess
import sys

from PIL import Image, ImageDraw, ImageFont

W, H, FPS = 1920, 1080, 30
LEAD = 1.0  # seconds of the home screen before the first call
HOLD = 5.0  # seconds after the slowest side finishes
CARD_DELAY, CARD_FADE = 0.6, 0.5  # closing card: starts this long after the slowest side, fades in

BG = (13, 14, 17)
PANEL = (22, 24, 29)
LINE = (44, 47, 55)
TEXT = (236, 237, 240)
MUTED = (138, 143, 155)
DIM = (92, 96, 106)
RED = (240, 90, 90)
ARMS = {
    "classic": {"color": (242, 166, 90), "name": "Standard emulator + adb", "short": "Standard + adb",
                "sub": "adb shell input · uiautomator dump · screencap", "plane": "adb server"},
    "agent-device": {"color": (167, 139, 250), "name": "agent-device", "short": "agent-device",
                     "sub": "agent-device CLI · press/fill by selector · wait text", "plane": "agent-device"},
    "argent": {"color": (96, 165, 250), "name": "Argent", "short": "Argent",
               "sub": "argent run · describe + gesture-tap · await-ui-element", "plane": "argent"},
    "agemu": {"color": (94, 234, 212), "name": "agemu", "short": "agemu",
              "sub": "agemu CLI · action + wait in one call", "plane": "agemud"},
}
TASK = "Agemu Shop · sign in, scroll, search, add to cart, check out"
NUM_WORDS = {1: "one", 2: "two", 3: "three", 4: "four"}


def font(size, weight="Regular", mono=False):
    paths = ["/System/Library/Fonts/Menlo.ttc"] if mono else \
        ["/System/Library/Fonts/SFNS.ttf", "/System/Library/Fonts/Helvetica.ttc"]
    for p in paths:
        if os.path.exists(p):
            f = ImageFont.truetype(p, size)
            try:
                f.set_variation_by_name(weight)
            except Exception:
                pass
            return f
    return ImageFont.load_default(size)


F = {
    "title": font(34, "Bold"), "sub": font(19), "name": font(30, "Bold"), "side_sub": font(16),
    "timer": font(84, "Bold"), "timer_s": font(40, "Semibold"), "label": font(17), "value": font(22, "Semibold"),
    "step": font(20, "Semibold"), "mono": font(15, mono=True), "mono_b": font(15, "Bold", mono=True),
    "small": font(15), "card_big": font(54, "Bold"), "card": font(24), "card_b": font(24, "Semibold"),
    "card_h": font(17), "card_dev": font(15), "bar": font(17, "Semibold"), "badge": font(24, "Bold"),
    "dev": font(14, "Semibold"),
    # column layout (3-4 runs)
    "n_title": font(30, "Bold"), "n_sub": font(18), "c_name": font(25, "Bold"), "c_sub": font(14),
    "c_timer": font(56, "Bold"), "c_timer_s": font(28, "Semibold"), "c_status": font(19, "Semibold"),
    "c_step": font(19, "Semibold"), "c_label": font(15), "c_value": font(19, "Semibold"),
    "c_mono": font(13, mono=True), "c_mono_b": font(13, "Bold", mono=True),
}


def load_run(d, arm=None):
    rd = lambda n: [json.loads(l) for l in open(os.path.join(d, n)) if l.strip()]
    summary = json.load(open(os.path.join(d, "summary.json")))
    arm = arm or summary.get("arm", "classic")
    if arm not in ARMS:
        sys.exit(f"unknown arm {arm!r} (want one of {', '.join(ARMS)})")
    platform = summary.get("platform", "android")
    run = {"dir": d, "summary": summary, "arm": arm, "platform": platform, "style": ARMS[arm],
           "events": rd("events.jsonl"), "frames": rd("frames.jsonl"), "samples": rd("samples.jsonl"),
           "ok": bool(summary.get("ok")), "error": summary.get("error") or ""}
    run["device"] = "agemu device" if arm == "agemu" else \
        ("standard Simulator" if platform == "ios" else "standard emulator")
    run["frame_t"] = [f["t"] for f in run["frames"]]
    run["sample_t"] = [s["t"] for s in run["samples"]]
    # Device CPU-seconds used since the task started, at each sample.
    cum, prev, acc = [], 0.0, 0.0
    for smp in run["samples"]:
        if smp["t"] > 0:
            acc += smp["emu_cpu"] / 100 * (smp["t"] - max(prev, 0.0))
        prev = smp["t"]
        cum.append(acc)
    run["cpu_cum"] = cum
    steps = len(summary["steps"])
    run["step_end"] = [max((e["end"] for e in run["events"] if e["step"] == k), default=1e9) for k in range(1, steps + 1)]
    if not run["ok"] and run["events"]:
        # The step it failed in was not done.
        run["step_end"][run["events"][-1]["step"] - 1] = 1e9
    run["total"] = summary["total_s"]
    run["cache"] = {}
    return run


def cpu_at(run, t):
    si = bisect.bisect_right(run["sample_t"], t) - 1
    return run["cpu_cum"][si] if si >= 0 else 0.0


def task_cpu(run):
    return cpu_at(run, run["total"])


def median_call(run):
    d = sorted(e["end"] - e["start"] for e in run["events"])
    if not d:
        return 0.0
    return d[len(d) // 2] if len(d) % 2 else (d[len(d) // 2 - 1] + d[len(d) // 2]) / 2


def fmt_dur(s):
    return f"{s * 1000:.0f} ms" if s < 1 else f"{s:.2f} s"


def phone_image(run, t, size):
    i = max(0, bisect.bisect_right(run["frame_t"], t) - 1)
    name = run["frames"][i]["file"]
    key = (name, size)
    if key not in run["cache"]:
        if len(run["cache"]) > 64:
            run["cache"].clear()
        im = Image.open(os.path.join(run["dir"], "frames", name)).convert("RGB").resize(size, Image.LANCZOS)
        run["cache"][key] = im
    return run["cache"][key]


def rounded_mask(size, r):
    m = Image.new("L", size, 0)
    ImageDraw.Draw(m).rounded_rectangle([0, 0, size[0] - 1, size[1] - 1], r, fill=255)
    return m


def fit(draw, text, f, width):
    if draw.textlength(text, font=f) <= width:
        return text
    while text and draw.textlength(text + "…", font=f) > width:
        text = text[:-1]
    return text + "…"


def blend(base, top, a):
    """top over base with opacity a (both RGB)."""
    if a >= 1:
        return top
    if a <= 0:
        return base
    return Image.blend(base, top, a)


def run_state(run, t):
    evs = run["events"]
    started = evs[:bisect.bisect_right(run.setdefault("start_t", [e["start"] for e in evs]), t)]
    steps = run["summary"]["steps"]
    done = t >= run["total"]
    steps_done = sum(1 for se in run["step_end"] if se <= t)
    cur_step = started[-1]["step"] if started else 1
    if done and run["ok"]:
        cur_step = len(steps)
    return started, steps_done, cur_step, done


def device_pill(draw, x, y, run, f=None):
    f = f or F["dev"]
    txt = run["device"]
    col = run["style"]["color"] if run["arm"] == "agemu" else MUTED
    tw = draw.textlength(txt, font=f)
    draw.rounded_rectangle([x, y, x + tw + 16, y + 22], 11, outline=LINE, width=1)
    draw.text((x + 8, y + 3), txt, font=f, fill=col)
    return tw + 16


def status_text(run, t):
    if t >= run["total"]:
        return ("✓ task done", run["style"]["color"]) if run["ok"] else ("✗ failed", RED)
    return ("ready", MUTED) if t < 0 else ("running", MUTED)


# ---------------------------------------------------------------------------------------------
# Two runs: the wide side-by-side layout.

def draw_side(img, draw, run, x0, t, mask, phone_size):
    side = run["style"]
    col = side["color"]
    total = run["total"]
    tt = min(max(t, 0.0), total)
    started, steps_done, cur_step, done = run_state(run, t)

    # Header
    draw.rectangle([x0 + 40, 112, x0 + 46, 150], fill=col)
    draw.text((x0 + 60, 108), side["name"], font=F["name"], fill=TEXT)
    device_pill(draw, x0 + 60 + draw.textlength(side["name"], font=F["name"]) + 16, 116, run)
    draw.text((x0 + 60, 148), side["sub"], font=F["side_sub"], fill=MUTED)

    # Phone
    px, py = x0 + 40, 184
    ph = phone_image(run, t, phone_size)
    draw.rounded_rectangle([px - 4, py - 4, px + phone_size[0] + 3, py + phone_size[1] + 3], 17, fill=LINE)
    img.paste(ph, (px, py), mask)

    # Panel
    ix = px + phone_size[0] + 32
    iw = x0 + 960 - 32 - ix
    timer_col = (col if run["ok"] else RED) if done else TEXT
    num = f"{tt:.1f}"
    draw.text((ix, 170), num, font=F["timer"], fill=timer_col)
    draw.text((ix + draw.textlength(num, font=F["timer"]) + 10, 205), "s", font=F["timer_s"], fill=timer_col)
    stxt, scol = status_text(run, t)
    draw.text((ix + 4, 264), stxt, font=F["step"], fill=scol)

    steps = run["summary"]["steps"]
    y = 312
    draw.line([ix, y, ix + iw, y], fill=LINE, width=1)
    y += 14
    draw.text((ix, y), "Step", font=F["label"], fill=MUTED)
    draw.text((ix + 150, y - 3), f"{min(steps_done + (0 if done else 1), len(steps))} / {len(steps)}", font=F["value"], fill=TEXT)
    y += 32
    if done and not run["ok"]:
        draw.text((ix, y), fit(draw, run["error"] or "failed", F["step"], iw), font=F["step"], fill=RED)
    else:
        draw.text((ix, y), fit(draw, steps[cur_step - 1], F["step"], iw), font=F["step"], fill=col)
    y += 40
    finished = [e for e in started if e["end"] <= t]
    rows = [("Tool calls", f"{len(started)}")]
    if finished:
        rows.append(("Avg per call", fmt_dur(sum(e["end"] - e["start"] for e in finished) / len(finished))))
    else:
        rows.append(("Avg per call", "–"))
    si = bisect.bisect_right(run["sample_t"], tt) - 1
    if si >= 0:
        s = run["samples"][si]
        dev = "Simulator" if run["platform"] == "ios" else "Emulator"
        rows.append((f"{dev} CPU", f"{s['emu_cpu']:.0f}% of a core now"))
        rows.append(("CPU used", f"{run['cpu_cum'][si]:.1f} CPU-s so far"))
        if s.get("emu_footprint_mb"):
            rows.append((f"{dev} memory", f"{s['emu_footprint_mb'] / 1024:.2f} GB"))
        rows.append((side["plane"], f"{s['plane_cpu']:.0f}% · {s['plane_rss_mb']:.0f} MB"))
    for label, value in rows:
        draw.text((ix, y), label, font=F["label"], fill=MUTED)
        draw.text((ix + 150, y - 3), fit(draw, value, F["value"], iw - 150), font=F["value"], fill=TEXT)
        y += 34

    # Tool-call log: the last calls, the running one at the bottom with a live timer.
    y += 10
    draw.line([ix, y, ix + iw, y], fill=LINE, width=1)
    y += 12
    draw.text((ix, y), "Tool calls", font=F["label"], fill=MUTED)
    y += 30
    n = max(1, (960 - y) // 25)
    draw_log(draw, started[-n:], t, ix, 960 - 25 * len(started[-n:]), iw, 25, col, F["mono"], F["mono_b"])
    return steps_done, len(steps)


def draw_log(draw, shown, t, x, y, w, row_h, col, f, fb):
    for e in shown:
        running = e["end"] > t
        dur = (t - e["start"]) if running else (e["end"] - e["start"])
        dtxt = fmt_dur(dur)
        dw = draw.textlength(dtxt, font=fb)
        cmd = fit(draw, e["cmd"], fb if running else f, w - dw - 14)
        if running:
            draw.rectangle([x - 6, y - 3, x + w + 4, y + row_h - 4], fill=(36, 39, 46))
            draw.text((x, y), cmd, font=fb, fill=TEXT)
            draw.text((x + w - dw, y), dtxt, font=fb, fill=col)
        else:
            draw.text((x, y), cmd, font=f, fill=MUTED if e["ok"] else RED)
            draw.text((x + w - draw.textlength(dtxt, font=f), y), dtxt, font=f, fill=DIM if dur < 1 else MUTED)
        y += row_h


# ---------------------------------------------------------------------------------------------
# Three or four runs: columns.

COL_STATS_H = 318  # height of the stats block under the phone


def draw_column(img, draw, run, x0, cw, t, lay, mask):
    side = run["style"]
    col = side["color"]
    tt = min(max(t, 0.0), run["total"])
    started, steps_done, cur_step, done = run_state(run, t)
    steps = run["summary"]["steps"]
    mx, mw = x0 + 28, cw - 56

    # Header: name, how it drives the device, which device.
    draw.rectangle([mx, 96, mx + 5, 124], fill=col)
    draw.text((mx + 16, 92), fit(draw, side["name"], F["c_name"], mw - 16), font=F["c_name"], fill=TEXT)
    draw.text((mx + 16, 125), fit(draw, side["sub"], F["c_sub"], mw - 16), font=F["c_sub"], fill=MUTED)
    device_pill(draw, mx + 16, 147, run)

    # Phone, centred in the column.
    pw, ph = lay["size"]
    px, py = x0 + (cw - pw) // 2, lay["py"]
    im = phone_image(run, t, (pw, ph))
    draw.rounded_rectangle([px - 4, py - 4, px + pw + 3, py + ph + 3], 15, fill=LINE)
    img.paste(im, (px, py), mask)

    # Timer + status
    y = py + ph + 14
    timer_col = (col if run["ok"] else RED) if done else TEXT
    num = f"{tt:.1f}"
    draw.text((mx, y - 6), num, font=F["c_timer"], fill=timer_col)
    draw.text((mx + draw.textlength(num, font=F["c_timer"]) + 7, y + 20), "s", font=F["c_timer_s"], fill=timer_col)
    stxt, scol = status_text(run, t)
    draw.text((mx + mw - draw.textlength(stxt, font=F["c_status"]), y + 26), stxt, font=F["c_status"], fill=scol)
    y += 66

    # Step n/18 + name (or the error)
    k = min(steps_done + (0 if done else 1), len(steps))
    lab = f"{k}/{len(steps)}"
    draw.text((mx, y), lab, font=F["c_step"], fill=TEXT)
    lx = mx + draw.textlength(lab, font=F["c_step"]) + 12
    if done and not run["ok"]:
        draw.text((lx, y), fit(draw, run["error"] or "failed", F["c_step"], mx + mw - lx), font=F["c_step"], fill=RED)
    else:
        draw.text((lx, y), fit(draw, steps[cur_step - 1], F["c_step"], mx + mw - lx), font=F["c_step"], fill=col)
    y += 34
    draw.line([mx, y, mx + mw, y], fill=LINE, width=1)
    y += 10

    rows = [("Tool calls", f"{len(started)}")]
    si = bisect.bisect_right(run["sample_t"], tt) - 1
    if si >= 0:
        s = run["samples"][si]
        rows.append(("Device CPU", f"{s['emu_cpu']:.0f}% now · {run['cpu_cum'][si]:.1f} CPU-s"))
        mem = s.get("emu_footprint_mb") or s.get("emu_rss_mb") or 0
        rows.append(("Memory", f"{mem / 1024:.2f} GB" if mem else "–"))
    else:
        rows += [("Device CPU", "–"), ("Memory", "–")]
    for label, value in rows:
        draw.text((mx, y + 3), label, font=F["c_label"], fill=MUTED)
        draw.text((mx + 112, y), fit(draw, value, F["c_value"], mw - 112), font=F["c_value"], fill=TEXT)
        y += 28
    y += 4
    draw.line([mx, y, mx + mw, y], fill=LINE, width=1)
    y += 10

    shown = started[-4:]
    draw_log(draw, shown, t, mx, y + 22 * (4 - len(shown)), mw, 22, col, F["c_mono"], F["c_mono_b"])
    return steps_done, len(steps)


# ---------------------------------------------------------------------------------------------
# Shared pieces: progress bars, speed badge, closing card.

def draw_bars(draw, runs, progress, t, y0, row_h):
    lw = 40 + max(draw.textlength(r["style"]["short"], font=F["bar"]) for r in runs) + 24
    bx, bw = lw, W - lw - 170
    for i, run in enumerate(runs):
        col = run["style"]["color"]
        yy = y0 + i * row_h
        draw.text((40, yy + 2), run["style"]["short"], font=F["bar"], fill=col)
        draw.rounded_rectangle([bx, yy + 6, bx + bw, yy + 22], 8, fill=PANEL)
        sd, total = progress[i]
        failed = t >= run["total"] and not run["ok"]
        frac = 1.0 if t >= run["total"] and run["ok"] else sd / total
        if frac > 0:
            draw.rounded_rectangle([bx, yy + 6, bx + max(16, int(bw * frac)), yy + 22], 8,
                                   fill=tuple(c // 2 for c in col) if failed else col)
        lab = f"{min(max(t, 0.0), run['total']):.1f} s"
        if failed:
            lab = "✗ " + lab
        draw.text((bx + bw + 14, yy + 2), lab, font=F["bar"], fill=RED if failed else TEXT)


def draw_badge(draw, text, right=W - 40, top=22):
    tw = draw.textlength(text, font=F["badge"])
    w = tw + 76
    x = right - w
    draw.rounded_rectangle([x, top, right, top + 44], 22, fill=(48, 40, 18), outline=(250, 204, 21), width=2)
    # ▶▶ drawn as two triangles (the font may not have the glyph)
    for k in range(2):
        tx = x + 18 + k * 13
        draw.polygon([(tx, top + 13), (tx, top + 31), (tx + 13, top + 22)], fill=(250, 204, 21))
    draw.text((x + 56, top + 8), text, font=F["badge"], fill=(250, 204, 21))


def card_rows(runs):
    """Rows for the closing card: (run, ratio vs agemu or None)."""
    base = next((r for r in runs if r["arm"] == "agemu" and r["ok"]), None)
    return base, [(r, (r["total"] / base["total"]) if base and r is not base and r["ok"] else None) for r in runs]


def draw_card(img, runs):
    """Dim the frame and draw the comparison card on it (opaque; callers blend it in)."""
    img = Image.blend(img, Image.new("RGB", (W, H), (8, 9, 11)), 0.78)
    draw = ImageDraw.Draw(img)
    base, rows = card_rows(runs)
    steps = len(runs[0]["summary"]["steps"])
    row_h = 60
    cw = 1460
    ch = 40 + 70 + 34 + 34 + row_h * len(rows) + 24 + 40
    cx, cy = (W - cw) // 2, (H - ch) // 2
    draw.rounded_rectangle([cx, cy, cx + cw, cy + ch], 24, fill=PANEL, outline=(54, 58, 66))

    x, y = cx + 50, cy + 36
    if base:
        head = f"agemu {base['total']:.1f} s"
        draw.text((x, y), head, font=F["card_big"], fill=base["style"]["color"])
        tail = f"for the whole {steps}-step task"
        others = [ratio for _, ratio in rows if ratio]
        if others:
            tail += f" · {min(others):.1f}–{max(others):.1f}× faster" if len(others) > 1 else f" · {others[0]:.1f}× faster"
        draw.text((x + draw.textlength(head, font=F["card_big"]) + 22, y + 26), tail, font=F["card"], fill=MUTED)
    else:
        agemu = next((r for r in runs if r["arm"] == "agemu"), None)
        draw.text((x, y), "agemu ✗ failed" if agemu else f"Task time, {steps} steps", font=F["card_big"],
                  fill=RED if agemu else TEXT)
    y += 104

    cols = [("Task time", 520), ("vs agemu", 680), ("Tool calls", 900), ("Median call", 1060), ("Device CPU", 1250)]
    if not base:
        cols = [c for c in cols if c[0] != "vs agemu"]
    for label, ox in cols:
        draw.text((cx + ox, y), label, font=F["card_h"], fill=MUTED)
    y += 30
    draw.line([x, y, cx + cw - 50, y], fill=LINE, width=1)
    y += 12
    for run, ratio in rows:
        col = run["style"]["color"]
        draw.rectangle([x, y + 4, x + 5, y + 44], fill=col)
        draw.text((x + 18, y), run["style"]["name"], font=F["card_b"], fill=TEXT)
        draw.text((x + 18, y + 29), f"on {run['device']}", font=F["card_dev"], fill=MUTED)
        if not run["ok"]:
            msg = f"✗ failed after {run['total']:.1f} s · {run['error'] or 'no error text'}"
            draw.text((cx + 520, y + 6), fit(draw, msg, F["card"], cw - 520 - 50), font=F["card"], fill=RED)
            y += row_h
            continue
        if run is base or ratio is None:
            vs = "baseline"
        else:
            vs = f"{ratio:.1f}× slower" if ratio >= 1 else f"{1 / ratio:.1f}× faster"
        vals = {"Task time": (f"{run['total']:.1f} s", col),
                "vs agemu": (vs, MUTED if run is base else TEXT),
                "Tool calls": (f"{len(run['events'])}", TEXT),
                "Median call": (fmt_dur(median_call(run)), TEXT),
                "Device CPU": (f"{task_cpu(run):.0f} CPU-s", TEXT)}
        for label, ox in cols:
            v, c = vals[label]
            draw.text((cx + ox, y + 6), v, font=F["card_b"] if label in ("Task time", "vs agemu") else F["card"], fill=c)
        y += row_h
    y += 16
    draw.text((x, y), f"Same {steps}-step plan, same app, back to back on one Mac. Model think time not included.",
              font=F["card_h"], fill=MUTED)
    return img


# ---------------------------------------------------------------------------------------------

class Race:
    """Layout and frame drawing for 2-4 runs. frame(t) draws the scene at task time t."""

    def __init__(self, runs, title=None, subtitle=None):
        if not 2 <= len(runs) <= 4:
            sys.exit("need 2 to 4 runs")
        self.runs = runs
        n = len(runs)
        self.title = title or f"One agent task, {NUM_WORDS[n]} tool stacks"
        self.slow = max(r["total"] for r in runs)
        self.sizes, self.masks = [], []
        if n == 2:
            self.subtitle = subtitle or (TASK + " · same plan, same host · "
                                         "no model think time: each call starts when the previous one returns")
            for r in runs:
                s = r["summary"]
                ph = 776
                pw = round(ph * s["frame_w"] / s["frame_h"])
                self.sizes.append((pw, ph))
                self.masks.append(rounded_mask((pw, ph), 14))
        else:
            steps = len(runs[0]["summary"]["steps"])
            self.subtitle = subtitle or f"{TASK} · {steps} steps · same plan, same host · no model think time"
            self.cw = W // n
            self.bar_h = 26
            self.bar_y = H - 18 - n * self.bar_h
            py = 184
            ph = self.bar_y - 14 - COL_STATS_H - py
            for r in runs:
                s = r["summary"]
                h = ph
                w = round(h * s["frame_w"] / s["frame_h"])
                if w > self.cw - 56:
                    w = self.cw - 56
                    h = round(w * s["frame_h"] / s["frame_w"])
                self.sizes.append((w, h))
                self.masks.append(rounded_mask((w, h), 12))
            self.py = py
            self.ph = ph

    def frame(self, t, badge=None, card=True):
        img = Image.new("RGB", (W, H), BG)
        draw = ImageDraw.Draw(img)
        n = len(self.runs)
        badge_w = (draw.textlength(badge, font=F["badge"]) + 76 + 30) if badge else 0
        if n == 2:
            draw.text((40, 26), self.title, font=F["title"], fill=TEXT)
            draw.text((40, 70), fit(draw, self.subtitle, F["sub"], W - 80), font=F["sub"], fill=MUTED)
            draw.line([960, 112, 960, 960], fill=LINE, width=1)
            progress = [draw_side(img, draw, r, i * 960, t, self.masks[i], self.sizes[i]) for i, r in enumerate(self.runs)]
            draw_bars(draw, self.runs, progress, t, 984, 40)
        else:
            draw.text((40, 22), self.title, font=F["n_title"], fill=TEXT)
            sx = 40 + draw.textlength(self.title, font=F["n_title"]) + 24
            draw.text((sx, 33), fit(draw, self.subtitle, F["n_sub"], W - 40 - badge_w - sx), font=F["n_sub"], fill=MUTED)
            draw.line([40, 76, W - 40, 76], fill=LINE, width=1)
            progress = []
            for i, r in enumerate(self.runs):
                x0 = i * self.cw
                if i:
                    draw.line([x0, 92, x0, self.bar_y - 14], fill=LINE, width=1)
                lay = {"size": self.sizes[i], "py": self.py + (self.ph - self.sizes[i][1]) // 2}
                progress.append(draw_column(img, draw, r, x0, self.cw, t, lay, self.masks[i]))
            draw.line([40, self.bar_y - 8, W - 40, self.bar_y - 8], fill=LINE, width=1)
            draw_bars(draw, self.runs, progress, t, self.bar_y, self.bar_h)
        if badge:
            draw_badge(draw, badge)
        if card and t > self.slow + CARD_DELAY:
            a = min(1.0, (t - self.slow - CARD_DELAY) / CARD_FADE)
            img = blend(img, draw_card(img, self.runs), a)
        return img


def ffmpeg_writer(out, fps, crf=26, tune="stillimage", size=(W, H)):
    cmd = ["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24",
           "-s", f"{size[0]}x{size[1]}", "-r", str(fps), "-i", "-", "-c:v", "libx264",
           "-preset", "slow", "-crf", str(crf)]
    if tune:
        cmd += ["-tune", tune]
    cmd += ["-pix_fmt", "yuv420p", "-movflags", "+faststart", out]
    return subprocess.Popen(cmd, stdin=subprocess.PIPE)


def parse_run(spec):
    arm, sep, d = spec.partition("=")
    if not sep or arm not in ARMS:
        sys.exit(f"--run wants ARM=DIR with ARM one of {', '.join(ARMS)}, got {spec!r}")
    return arm, d


def main():
    ap = argparse.ArgumentParser(description="Side-by-side video of 2-4 agent-race runs.")
    ap.add_argument("--run", action="append", default=[], metavar="ARM=DIR",
                    help=f"a run directory and its arm ({', '.join(ARMS)}); 2-4 times, in column order")
    ap.add_argument("--left", help="old two-arm call: classic run directory")
    ap.add_argument("--right", help="old two-arm call: agemu run directory")
    ap.add_argument("--title", help="headline (default: 'One agent task, N tool stacks')")
    ap.add_argument("--out", required=True)
    ap.add_argument("--poster", help="also write the closing frame as a PNG")
    ap.add_argument("--fps", type=int, default=FPS)
    args = ap.parse_args()

    specs = [parse_run(s) for s in args.run]
    if args.left or args.right:
        if not (args.left and args.right) or specs:
            sys.exit("use --left and --right together, or --run")
        specs = [("classic", args.left), ("agemu", args.right)]
    if not 2 <= len(specs) <= 4:
        sys.exit("give 2 to 4 --run ARM=DIR")
    race = Race([load_run(d, arm) for arm, d in specs], title=args.title)

    end_t = race.slow + HOLD
    nframes = int((end_t + LEAD) * args.fps)
    ff = ffmpeg_writer(args.out, args.fps)
    last = None
    for i in range(nframes):
        t = i / args.fps - LEAD
        im = race.frame(t)
        ff.stdin.write(im.tobytes())
        last = im
        if i % (args.fps * 10) == 0:
            print(f"  {t:6.1f} s / {end_t:.1f}", file=sys.stderr)
    ff.stdin.close()
    if ff.wait() != 0:
        sys.exit("ffmpeg failed")
    if args.poster and last is not None:
        last.save(args.poster)
    print(args.out)


if __name__ == "__main__":
    main()
