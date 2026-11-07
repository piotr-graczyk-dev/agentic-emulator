#!/usr/bin/env python3
"""Side-by-side video of two agent-race runs, in real time.

    uv run --with pillow examples/agent-race/compose.py \
        --left runs/classic-2 --right runs/agemu-2 --out agent-race.mp4 --poster poster.png

Each side shows the recorded screen, the elapsed time, the current step, every tool call as it
runs (the running one with a live timer) and the emulator's and control plane's CPU and memory.
A bar at the bottom shows both sides' progress through the plan. Needs Pillow and ffmpeg.
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
HOLD = 5.0  # seconds after the slower side finishes

BG = (13, 14, 17)
PANEL = (22, 24, 29)
LINE = (44, 47, 55)
TEXT = (236, 237, 240)
MUTED = (138, 143, 155)
DIM = (92, 96, 106)
SIDES = {
    "classic": {"color": (242, 166, 90), "name": "Standard emulator + adb",
                "sub": "emulator -avd · adb shell input · uiautomator dump · screencap"},
    "agemu": {"color": (94, 234, 212), "name": "agemu",
              "sub": "agemu CLI, one process per call · action + wait in one call"},
}


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
    "small": font(15), "card_big": font(54, "Bold"), "card": font(24), "bar": font(17, "Semibold"),
}


def load_run(d):
    rd = lambda n: [json.loads(l) for l in open(os.path.join(d, n)) if l.strip()]
    run = {"dir": d, "summary": json.load(open(os.path.join(d, "summary.json"))),
           "events": rd("events.jsonl"), "frames": rd("frames.jsonl"), "samples": rd("samples.jsonl")}
    run["frame_t"] = [f["t"] for f in run["frames"]]
    run["sample_t"] = [s["t"] for s in run["samples"]]
    # Emulator CPU-seconds used since the task started, at each sample.
    cum, prev, acc = [], 0.0, 0.0
    for smp in run["samples"]:
        if smp["t"] > 0:
            acc += smp["emu_cpu"] / 100 * (smp["t"] - max(prev, 0.0))
        prev = smp["t"]
        cum.append(acc)
    run["cpu_cum"] = cum
    steps = len(run["summary"]["steps"])
    run["step_end"] = [max((e["end"] for e in run["events"] if e["step"] == k), default=1e9) for k in range(1, steps + 1)]
    run["total"] = run["summary"]["total_s"]
    run["cache"] = {}
    return run


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


def fmt_s(s):
    return f"{s:5.1f} s"


def draw_side(img, draw, run, kind, x0, t, mask, phone_size):
    side = SIDES[kind]
    col = side["color"]
    total = run["total"]
    done = t >= total
    tt = min(max(t, 0.0), total)

    # Header
    draw.rectangle([x0 + 40, 112, x0 + 46, 150], fill=col)
    draw.text((x0 + 60, 108), side["name"], font=F["name"], fill=TEXT)
    draw.text((x0 + 60, 148), side["sub"], font=F["side_sub"], fill=MUTED)

    # Phone
    px, py = x0 + 40, 184
    ph = phone_image(run, t, phone_size)
    draw.rounded_rectangle([px - 4, py - 4, px + phone_size[0] + 3, py + phone_size[1] + 3], 17, fill=LINE)
    img.paste(ph, (px, py), mask)

    # Panel
    ix = px + phone_size[0] + 32
    iw = x0 + 960 - 32 - ix
    timer_col = col if done else TEXT
    num = f"{tt:.1f}"
    draw.text((ix, 170), num, font=F["timer"], fill=timer_col)
    draw.text((ix + draw.textlength(num, font=F["timer"]) + 10, 205), "s", font=F["timer_s"], fill=timer_col)
    if done:
        ok = run["summary"]["ok"]
        draw.text((ix + 4, 264), "✓ task done" if ok else "✗ failed", font=F["step"], fill=col if ok else (240, 90, 90))
    elif t < 0:
        draw.text((ix + 4, 264), "ready", font=F["step"], fill=MUTED)
    else:
        draw.text((ix + 4, 264), "running", font=F["step"], fill=MUTED)

    evs = run["events"]
    started = [e for e in evs if e["start"] <= t]
    finished = [e for e in evs if e["end"] <= t]
    steps = run["summary"]["steps"]
    steps_done = sum(1 for se in run["step_end"] if se <= t)
    cur_step = started[-1]["step"] if started else 1
    if done:
        cur_step = len(steps)

    y = 312
    draw.line([ix, y, ix + iw, y], fill=LINE, width=1)
    y += 14
    draw.text((ix, y), "Step", font=F["label"], fill=MUTED)
    draw.text((ix + 150, y - 3), f"{min(steps_done + (0 if done else 1), len(steps))} / {len(steps)}", font=F["value"], fill=TEXT)
    y += 32
    draw.text((ix, y), fit(draw, steps[cur_step - 1], F["step"], iw), font=F["step"], fill=col)
    y += 40
    calls = len(started)
    rows = [("Tool calls", f"{calls}")]
    if finished:
        avg = sum(e["end"] - e["start"] for e in finished) / len(finished)
        rows.append(("Avg per call", f"{avg * 1000:.0f} ms" if avg < 1 else f"{avg:.2f} s"))
    else:
        rows.append(("Avg per call", "–"))
    si = bisect.bisect_right(run["sample_t"], tt) - 1
    if si >= 0:
        s = run["samples"][si]
        rows.append(("Emulator CPU", f"{s['emu_cpu']:.0f}% of a core now"))
        rows.append(("CPU used", f"{run['cpu_cum'][si]:.1f} CPU-s so far"))
        if s.get("emu_footprint_mb"):
            rows.append(("Emulator memory", f"{s['emu_footprint_mb'] / 1024:.2f} GB"))
        plane = "adb server" if kind == "classic" else "agemud"
        rows.append((plane, f"{s['plane_cpu']:.0f}% · {s['plane_rss_mb']:.0f} MB"))
    for label, value in rows:
        draw.text((ix, y), label, font=F["label"], fill=MUTED)
        draw.text((ix + 150, y - 3), value, font=F["value"], fill=TEXT)
        y += 34

    # Tool-call log: the last calls, the running one at the bottom with a live timer.
    y += 10
    draw.line([ix, y, ix + iw, y], fill=LINE, width=1)
    y += 12
    draw.text((ix, y), "Tool calls", font=F["label"], fill=MUTED)
    y += 30
    log_h = 960 - y
    n = max(1, log_h // 25)
    shown = started[-n:]
    yy = 960 - 25 * len(shown)
    for e in shown:
        running = e["end"] > t
        dur = (t - e["start"]) if running else (e["end"] - e["start"])
        dtxt = f"{dur * 1000:.0f} ms" if dur < 1 else f"{dur:.2f} s"
        dw = draw.textlength(dtxt, font=F["mono"])
        cmd = fit(draw, e["cmd"], F["mono"], iw - dw - 16)
        if running:
            draw.rectangle([ix - 6, yy - 3, ix + iw + 2, yy + 21], fill=(36, 39, 46))
            draw.text((ix, yy), cmd, font=F["mono_b"], fill=TEXT)
            draw.text((ix + iw - dw, yy), dtxt, font=F["mono_b"], fill=col)
        else:
            draw.text((ix, yy), cmd, font=F["mono"], fill=MUTED if e["ok"] else (240, 90, 90))
            draw.text((ix + iw - dw, yy), dtxt, font=F["mono"], fill=DIM if dur < 1 else MUTED)
        yy += 25
    return steps_done, len(steps)


def median_ms(run):
    d = sorted(e["end"] - e["start"] for e in run["events"])
    m = d[len(d) // 2] if len(d) % 2 else (d[len(d) // 2 - 1] + d[len(d) // 2]) / 2
    return f"{m * 1000:.0f} ms"


def draw_frame(runs, t, masks, sizes, end_t):
    img = Image.new("RGB", (W, H), BG)
    draw = ImageDraw.Draw(img)
    draw.text((40, 26), "One agent task, two tool stacks", font=F["title"], fill=TEXT)
    draw.text((40, 70), "Agemu Shop · sign in, scroll, search, add to cart, check out · same plan, same host · "
                        "no model think time: each call starts when the previous one returns",
              font=F["sub"], fill=MUTED)
    draw.line([960, 112, 960, 960], fill=LINE, width=1)
    progress = []
    for i, (kind, run) in enumerate(runs):
        progress.append(draw_side(img, draw, run, kind, i * 960, t, masks[i], sizes[i]))

    # Progress bars
    by = 984
    bw = W - 210 - 150
    for i, (kind, run) in enumerate(runs):
        col = SIDES[kind]["color"]
        yy = by + i * 40
        draw.text((40, yy + 2), SIDES[kind]["name"].split(" +")[0].replace("Standard emulator", "Standard"), font=F["bar"], fill=col)
        draw.rounded_rectangle([210, yy + 6, 210 + bw, yy + 22], 8, fill=PANEL)
        sd, total = progress[i]
        if t >= run["total"]:
            frac = 1.0
        else:
            frac = sd / total
        if frac > 0:
            draw.rounded_rectangle([210, yy + 6, 210 + max(16, int(bw * frac)), yy + 22], 8, fill=col)
        lab = f"{min(t, run['total']):.1f} s" if t >= 0 else "0.0 s"
        draw.text((210 + bw + 14, yy + 2), lab, font=F["bar"], fill=TEXT)

    # Closing card
    slow = max(r["total"] for _, r in runs)
    if t > slow + 0.6:
        a = min(1.0, (t - slow - 0.6) / 0.5)
        over = Image.new("RGBA", (W, H), (0, 0, 0, 0))
        od = ImageDraw.Draw(over)
        od.rectangle([0, 0, W, H], fill=(8, 9, 11, int(200 * a)))
        cw, ch = 1100, 420
        cx, cy = (W - cw) // 2, (H - ch) // 2
        od.rounded_rectangle([cx, cy, cx + cw, cy + ch], 24, fill=(22, 24, 29, int(255 * a)), outline=(54, 58, 66, int(255 * a)))
        img = Image.alpha_composite(img.convert("RGBA"), over)
        draw = ImageDraw.Draw(img)
        c, a_ = dict(runs)["classic"], dict(runs)["agemu"]
        ratio = c["total"] / a_["total"]
        fa = lambda rgb: rgb + (int(255 * a),)
        draw.text((cx + 50, cy + 40), f"{ratio:.1f}× faster with agemu", font=F["card_big"], fill=fa(SIDES["agemu"]["color"]))
        lines = [
            (f"agemu  {a_['total']:.1f} s · {len(a_['events'])} tool calls", SIDES["agemu"]["color"]),
            (f"standard emulator + adb  {c['total']:.1f} s · {len(c['events'])} tool calls", SIDES["classic"]["color"]),
            (f"Median tool call: agemu {median_ms(a_)} · standard {median_ms(c)}", TEXT),
            (f"Emulator CPU for the task: agemu {a_['cpu_cum'][-1]:.0f} CPU-s · standard {c['cpu_cum'][-1]:.0f} CPU-s", TEXT),
            ("Same 18-step plan, same app, back to back on one Mac. Model think time not included.", MUTED),
        ]
        yy = cy + 140
        for text, col in lines:
            draw.text((cx + 52, yy), text, font=F["card"], fill=fa(col))
            yy += 46
        img = img.convert("RGB")
    return img


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--left", required=True, help="classic run directory")
    ap.add_argument("--right", required=True, help="agemu run directory")
    ap.add_argument("--out", required=True)
    ap.add_argument("--poster", help="also write the closing frame as a PNG")
    ap.add_argument("--fps", type=int, default=FPS)
    args = ap.parse_args()

    runs = [("classic", load_run(args.left)), ("agemu", load_run(args.right))]
    sizes, masks = [], []
    for _, r in runs:
        s = r["summary"]
        ph = 776
        pw = round(ph * s["frame_w"] / s["frame_h"])
        sizes.append((pw, ph))
        masks.append(rounded_mask((pw, ph), 14))
    end_t = max(r["total"] for _, r in runs) + HOLD
    nframes = int((end_t + LEAD) * args.fps)
    ff = subprocess.Popen(["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24",
                           "-s", f"{W}x{H}", "-r", str(args.fps), "-i", "-", "-c:v", "libx264",
                           "-preset", "slow", "-crf", "26", "-tune", "stillimage", "-pix_fmt", "yuv420p",
                           "-movflags", "+faststart", args.out], stdin=subprocess.PIPE)
    last = None
    for i in range(nframes):
        t = i / args.fps - LEAD
        im = draw_frame(runs, t, masks, sizes, end_t)
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
