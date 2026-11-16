#!/usr/bin/env python3
"""The Agent Grand Prix: the agent race cut as a vintage race broadcast, 1080x1080.

Cream paper, race numbers, five lanes for five tool stacks, a commentary strip. It shows the
median Android run of every stack from out/android (the runs picked for race.mp4), fast-forwards
once agemu has finished, and closes with both platforms' medians, the single-call timings and
what a device costs the Mac.

    uv run --with pillow examples/agent-race/grandprix.py --out out/grand-prix.mp4 --gif out/grand-prix.gif
    uv run --with pillow examples/agent-race/grandprix.py --sheet sheet.png --at 0.5,5,12,16.1,23,33

Every time on screen is read from the runs' summary.json and events.jsonl; the single-call
timings on the pit-stop card and the memory and CPU figures on the weigh-in card are the ones in
the main README's tables. The commentary lines are written
for the committed runs and name what each stack was doing at that second, so check them after a
new series. It needs the frames of the picked runs (not committed) and macOS's fonts (Futura,
Avenir Next Condensed, SignPainter, American Typewriter).
"""
import argparse
import bisect
import json
import math
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import compose as C  # noqa: E402
from PIL import Image, ImageChops, ImageDraw, ImageFont  # noqa: E402

W = H = 1080
FPS = 30
OUT = os.path.join(HERE, "out")

PAPER = (239, 231, 212)
INK = (27, 30, 38)
RED = (204, 54, 38)
MUSTARD = (220, 158, 34)
BLUE = (40, 84, 152)
SILVER = (128, 133, 142)
TEAL = (8, 134, 118)
CREAM = (248, 242, 228)
FADED = (128, 120, 104)      # secondary text on paper
RULE = (196, 186, 164)

SUP = "/System/Library/Fonts/Supplemental/"
FONTS = {
    "fut": (SUP + "Futura.ttc", 4), "futm": (SUP + "Futura.ttc", 3),
    "avh": ("/System/Library/Fonts/Avenir Next Condensed.ttc", 8),
    "avhi": ("/System/Library/Fonts/Avenir Next Condensed.ttc", 9),
    "avd": ("/System/Library/Fonts/Avenir Next Condensed.ttc", 2),
    "avm": ("/System/Library/Fonts/Avenir Next Condensed.ttc", 5),
    "sign": (SUP + "SignPainter.ttc", 1),
    "type": (SUP + "AmericanTypewriter.ttc", 0), "typeb": (SUP + "AmericanTypewriter.ttc", 2),
}
_fc = {}


def F(name, size):
    key = (name, int(size))
    if key not in _fc:
        path, idx = FONTS[name]
        if not os.path.exists(path):
            sys.exit(f"grandprix.py needs the macOS font {path}")
        _fc[key] = ImageFont.truetype(path, int(size), index=idx)
    return _fc[key]


TEAMS = [("classic", "adb", 1, RED), ("mobile-mcp", "mobile-mcp", 2, MUSTARD),
         ("agent-device", "agent-device", 3, BLUE), ("argent", "Argent", 4, SILVER),
         ("agemu", "agemu", 5, TEAL)]


def picked(platform, arm):
    p = json.load(open(os.path.join(OUT, platform, "picked.json")))
    return C.load_run(os.path.join(OUT, platform, "runs", p[arm]), arm)


RUNS = []
for arm, name, num, color in TEAMS:
    run = picked("android", arm)
    run.update(name=name, num=num, color=color)
    run["start_t"] = [e["start"] for e in run["events"]]
    span = {}
    for e in run["events"]:
        a, b = span.get(e["step"], (e["start"], e["end"]))
        span[e["step"]] = (min(a, e["start"]), max(b, e["end"]))
    run["span"] = span
    RUNS.append(run)
ADB, AGE = RUNS[0], RUNS[4]
STEPS = AGE["summary"]["steps"]
N = len(STEPS)
ORDER = sorted(RUNS, key=lambda r: r["total"])
for k, run in enumerate(ORDER):
    run["pos"] = k + 1
ORD = {1: "1ST", 2: "2ND", 3: "3RD", 4: "4TH", 5: "5TH"}


def medians(platform):
    by = {}
    root = os.path.join(OUT, platform, "runs")
    for name in sorted(os.listdir(root)):
        s = json.load(open(os.path.join(root, name, "summary.json")))
        if s.get("ok"):
            by.setdefault(name.rsplit("-", 1)[0], []).append(s["total_s"])
    out = {}
    for arm, v in by.items():
        v.sort()
        out[arm] = v[len(v) // 2] if len(v) % 2 else (v[len(v) // 2 - 1] + v[len(v) // 2]) / 2
    return out


MED = {"android": medians("android"), "ios": medians("ios")}
TEAM = {arm: (name, num, color) for arm, name, num, color in TEAMS}

# Timeline, seconds.
T_GRID = 3.6
T_FIN = 16.0                         # agemu takes the flag
T0 = T_FIN - AGE["total"]            # lights out
T_FF0, T_FF1, T_FF2 = 17.5, 19.5, 20.7   # fast-forward: to the third finisher, then to adb
T_CLASS = 21.3
T_IOS = 24.7
T_PITS = 27.2
T_WEIGH = 30.4
T_CARD = T_WEIGH + 3.084             # the weigh-in lasts one bar of the music it was cut to
T_END = T_CARD + 4.1
R_MID = 30.6                         # race time at T_FF1 (the field but adb is home)


def clamp(x, a=0.0, b=1.0):
    return a if x < a else b if x > b else x


def prog(t, t0, dur):
    return clamp((t - t0) / dur)


def ease_out(p):
    return 1 - (1 - p) ** 3


def ease_io(p):
    return p * p * (3 - 2 * p)


def ease_back(p, s=1.9):
    p -= 1
    return 1 + p * p * ((s + 1) * p + s)


def rgba(c, a=1.0):
    return (c[0], c[1], c[2], int(255 * clamp(a)))


def text(d, xy, s, f, fill, a=1.0, anchor="mm"):
    if a > 0:
        d.text(xy, s, font=f, fill=rgba(fill, a), anchor=anchor)


def tracked(d, cx, y, s, f, fill, gap, a=1.0, align="m"):
    """Letter-spaced text on one line; align l/m/r about cx."""
    ws = [d.textlength(c, font=f) for c in s]
    total = sum(ws) + gap * (len(s) - 1)
    x = cx - total / 2 if align == "m" else cx - total if align == "r" else cx
    for c, w in zip(s, ws):
        d.text((x, y), c, font=f, fill=rgba(fill, a), anchor="lm")
        x += w + gap
    return total


def number(d, right, base, s, f, fill, a=1.0):
    """A number with equal digit cells, right-aligned at `right`, on baseline `base`."""
    cell = max(d.textlength(c, font=f) for c in "0123456789")
    ws = [cell if c.isdigit() else d.textlength(c, font=f) for c in s]
    x = right - sum(ws)
    for c, w in zip(s, ws):
        d.text((x + w / 2, base), c, font=f, fill=rgba(fill, a), anchor="ms")
        x += w
    return sum(ws)


def fit(d, name, s, width, size):
    while size > 10 and d.textlength(s, font=F(name, size)) > width:
        size -= 2
    return F(name, size)


def roundel(d, cx, cy, r, num, color, a=1.0):
    d.ellipse([cx - r, cy - r, cx + r, cy + r], fill=rgba(color, a))
    k = max(3, r * 0.2)
    d.ellipse([cx - r + k, cy - r + k, cx + r - k, cy + r - k], fill=rgba(CREAM, a))
    text(d, (cx, cy + r * 0.04), str(num), F("fut", r * 1.15), INK, a)


def checker(d, x, y, w, h, sq, a=1.0):
    for j in range(int(math.ceil(h / sq))):
        for i in range(int(math.ceil(w / sq))):
            col = INK if (i + j) % 2 == 0 else CREAM
            d.rectangle([x + i * sq, y + j * sq, min(x + w, x + (i + 1) * sq) - 1, min(y + h, y + (j + 1) * sq) - 1],
                        fill=rgba(col, a))


def paper():
    img = Image.new("RGB", (W, H), PAPER)
    # a soft darkening towards the edges, like an old print
    v = Image.new("L", (W // 8, H // 8), 0)
    px = v.load()
    for y in range(v.size[1]):
        for x in range(v.size[0]):
            dx, dy = (x / v.size[0] - 0.5) * 2, (y / v.size[1] - 0.5) * 2
            px[x, y] = int(255 * clamp(1 - 0.10 * (dx * dx + dy * dy) ** 1.2))
    v = v.resize((W, H), Image.BICUBIC)
    return ImageChops.multiply(img, Image.merge("RGB", (v, v, v)))


def grains(n=4):
    out = []
    for _ in range(n):
        g = Image.effect_noise((W // 2, H // 2), 26).resize((W, H), Image.BILINEAR)
        g = g.point(lambda v: int(clamp(0.90 + (v - 128) / 128 * 0.10, 0, 1) * 255))
        out.append(Image.merge("RGB", (g, g, g)))
    return out


BG = paper()
GRAIN = grains()
GRAIN_MODE = "static"   # boil: a new grain every 3 frames (livelier, five times the file size)
STAMP_NOISE = Image.effect_noise((512, 512), 70)


def stamp(img, cx, cy, lines, color, angle, scale=1.0, a=1.0):
    """A rubber stamp: a ruled box with text, turned and a little worn."""
    if a <= 0:
        return
    probe = ImageDraw.Draw(img)
    sized = [(s, F(name, size * scale)) for s, name, size in lines]
    ws = [probe.textlength(s, font=f) for s, f in sized]
    hs = [f.size * 0.86 for _, f in sized]
    pad = 20 * scale
    w, h = int(max(ws) + 2 * pad + 16 * scale), int(sum(hs) + 2 * pad)
    layer = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)
    d.rounded_rectangle([3, 3, w - 4, h - 4], int(12 * scale), outline=rgba(color), width=max(3, int(6 * scale)))
    y = pad
    for (s, f), hh in zip(sized, hs):
        d.text((w / 2, y + hh / 2), s, font=f, fill=rgba(color), anchor="mm")
        y += hh
    al = layer.getchannel("A")
    worn = STAMP_NOISE.resize((w, h)).point(lambda v: 255 if v > 74 else 120)
    layer.putalpha(ImageChops.multiply(al, worn).point(lambda v: int(v * clamp(a))))
    layer = layer.rotate(angle, expand=True, resample=Image.BICUBIC)
    img.paste(layer, (int(cx - layer.size[0] / 2), int(cy - layer.size[1] / 2)), layer)


# ------------------------------------------------------------------------------- the race

def race_clock(t):
    if t <= T_FF0:
        return t - T0
    r0 = T_FF0 - T0
    if t <= T_FF1:
        return r0 + (R_MID - r0) * ease_io(prog(t, T_FF0, T_FF1 - T_FF0))
    if t <= T_FF2:
        return R_MID + (ADB["total"] + 0.02 - R_MID) * ease_io(prog(t, T_FF1, T_FF2 - T_FF1))
    return ADB["total"] + 0.02


def progress(run, r):
    """How far along the 18 steps the run is at race time r, 0..1."""
    if r <= 0:
        return 0.0
    if r >= run["total"]:
        return 1.0
    i = bisect.bisect_right(run["start_t"], r) - 1
    if i < 0:
        return 0.0
    step = run["events"][i]["step"]
    s0, s1 = run["span"][step]
    within = clamp((r - s0) / (s1 - s0)) if s1 > s0 else 1.0
    return min(0.995, (step - 1 + within) / N)


def step_of(run, r):
    i = bisect.bisect_right(run["start_t"], r) - 1
    return run["events"][i]["step"] if i >= 0 else 1


PH_W, PH_H, PH_GAP = 182, 406, 24
PH_X0 = (W - 5 * PH_W - 4 * PH_GAP) // 2
PH_TOP = 138
PH_MASK = C.rounded_mask((PH_W, PH_H), 17)
LANE_Y0, LANE_DY = 696, 62
TR_X0, TR_X1 = 304, 884


def commentary(r, t):
    if r < 0:
        return "Starting grid. Same app, same 18 steps, same Mac.", INK
    if r < 2.9:
        return "Lights out. adb opens with a 2.6-second screen read.", INK
    if r < 4.35:
        return "agemu is signed in and scrolling the catalog.", INK
    if r < 5.65:
        return "agemu puts a towel in the cart. adb is entering the e-mail.", INK
    if r < 8.15:
        return "agemu adds two lamps and opens the cart. adb is entering the password.", INK
    if r < AGE["total"]:
        return "agemu is checking out. adb is still signing in.", INK
    if t < T_FF0:
        return f"Chequered flag: agemu, {AGE['total']:.1f} s. adb is on step {step_of(ADB, r)} of {N}.", RED
    if r < ADB["total"]:
        return "Fast-forward. The rest of the field comes home.", INK
    return f"adb crosses the line at {ADB['total']:.1f} s.", INK


def scene_race(t):
    img = BG.copy()
    d = ImageDraw.Draw(img, "RGBA")
    r = race_clock(t)
    appear = prog(t, T_GRID, 0.5)

    # header
    text(d, (36, 52), "THE AGENT GRAND PRIX", F("fut", 50), INK, anchor="lm")
    tracked(d, 38, 94, "ANDROID EMULATOR  ·  ONE CHECKOUT  ·  18 STEPS", F("avd", 21), FADED, 2.2, align="l")
    shown = clamp(r, 0, ADB["total"])
    wn = number(d, 1010, 84, f"{shown:.1f}", F("fut", 78), INK)
    text(d, (1016, 84), "s", F("fut", 40), FADED, anchor="ls")
    ff = T_FF0 < t < T_FF2
    if ff and int(t * 6) % 2 == 0:
        speed = (race_clock(t + 0.05) - race_clock(t - 0.05)) / 0.1
        lab = f"FAST FORWARD  ×{max(2, round(speed))}"
        f = F("avh", 24)
        wl = d.textlength(lab, font=f)
        x1 = 1010 - wn - 26
        d.polygon([(x1 - wl - 36, 76), (x1 - 8, 76), (x1, 40), (x1 - wl - 28, 40)], fill=rgba(RED))
        text(d, (x1 - wl / 2 - 18, 58), lab, f, CREAM)
    d.rectangle([36, 114, 1044, 120], fill=rgba(RED))
    d.rectangle([36, 124, 1044, 126], fill=rgba(INK))

    # phones
    for i, run in enumerate(RUNS):
        p = ease_back(prog(t, T_GRID + 0.07 * i, 0.4), 1.2)
        if p <= 0:
            continue
        x = PH_X0 + i * (PH_W + PH_GAP)
        y = PH_TOP - (1 - p) * 520
        d.rounded_rectangle([x - 6, y - 6, x + PH_W + 5, y + PH_H + 5], 22, fill=rgba(TEAL if run is AGE else INK))
        img.paste(C.phone_image(run, r, (PH_W, PH_H)), (int(x), int(y)), PH_MASK)
        roundel(d, x + 6, y + 6, 26, run["num"], run["color"])
        text(d, (x + PH_W / 2, y + PH_H + 30), run["name"], fit(d, "avh", run["name"], PH_W, 30), TEAL if run is AGE else INK)

    # commentary strip
    line, col = commentary(r, t)
    d.rectangle([36, 596, 1044, 652], fill=rgba(col, appear))
    live = "REPLAY" if ff else "LIVE"
    f = F("avh", 22)
    wl = d.textlength(live, font=f)
    blink = ff or int(t * 2) % 2 == 0
    d.ellipse([54, 617, 68, 631], fill=rgba(CREAM if col == RED else RED, appear if blink else 0.25))
    text(d, (78, 625), live, f, CREAM, appear, anchor="lm")
    flag = col == RED
    text(d, (78 + wl + 22, 624), line, fit(d, "avhi", line, 1044 - 78 - wl - (130 if flag else 40), 33), CREAM, appear, anchor="lm")
    if flag:
        checker(d, 960, 596, 84, 56, 14)

    # lanes
    for i, run in enumerate(RUNS):
        a = prog(t, T_GRID + 0.15 + 0.06 * i, 0.3)
        if a <= 0:
            continue
        y = LANE_Y0 + i * LANE_DY
        roundel(d, 60, y, 21, run["num"], run["color"], a)
        text(d, (92, y), run["name"], F("avh", 29), INK, a, anchor="lm")
        d.rectangle([TR_X0, y - 1, TR_X1, y + 1], fill=rgba(RULE, a))
        for k in range(1, N):
            xk = TR_X0 + (TR_X1 - TR_X0) * k / N
            d.rectangle([xk - 1, y - 6, xk, y + 6], fill=rgba(RULE, a))
        checker(d, TR_X1, y - 20, 20, 40, 10, a)
        pr = progress(run, r)
        xm = TR_X0 + (TR_X1 - TR_X0) * pr
        d.rectangle([TR_X0, y - 7, xm, y + 7], fill=rgba(run["color"], a))
        roundel(d, xm, y, 17, run["num"], run["color"], a)
        if pr >= 1.0:
            col = TEAL if run is AGE else INK
            text(d, (1044, y + 13), "s", F("fut", 24), col, anchor="rs")
            number(d, 1028, y + 13, f"{run['total']:.1f}", F("fut", 38), col)
        elif r > 0:
            text(d, (1044, y), f"step {step_of(run, r)}", F("avd", 24), FADED, a, anchor="rm")
    text(d, (W / 2, 1046), "Scripted run of one plan on one Mac  ·  no model think time  ·  the median run of 5 per tool",
         F("avm", 21), FADED, appear)

    # start lights
    if t < T0 + 0.45:
        up = ease_out(prog(t, T0 + 0.1, 0.35)) * 420
        a = prog(t, T_GRID + 0.2, 0.25)
        cx, cy = W / 2, 330 - up
        d.rounded_rectangle([cx - 250, cy - 62, cx + 250, cy + 62], 26, fill=rgba(INK, a))
        for k in range(5):
            on = (T0 - 1.5 + 0.3 * k) <= t < T0
            x = cx - 184 + k * 92
            d.ellipse([x - 34, cy - 34, x + 34, cy + 34], fill=rgba((58, 26, 22) if not on else RED, a))
            if on:
                d.ellipse([x - 20, cy - 24, x - 6, cy - 10], fill=rgba((255, 170, 150), 0.7 * a))

    # finishing stamps
    for i, run in enumerate(RUNS):
        if r < run["total"]:
            continue
        at = T_FIN if run is AGE else None
        x = PH_X0 + i * (PH_W + PH_GAP) + PH_W / 2
        if at is None:   # video time this run finished at: invert the clock by scanning back
            lo, hi = T_FF0, T_FF2
            for _ in range(18):
                mid = (lo + hi) / 2
                lo, hi = (mid, hi) if race_clock(mid) < run["total"] else (lo, mid)
            at = hi
        p = prog(t, at, 0.22)
        stamp(img, x, PH_TOP + 250, [(ORD[run["pos"]], "fut", 62), (f"{run['total']:.1f} s", "fut", 38)],
              RED if run is AGE else INK, -11 + 4 * (i % 2), scale=1.0 + 0.7 * (1 - ease_out(p)), a=clamp(p * 3))
    return img


# ------------------------------------------------------------------------------- the cards

def scene_title(t):
    img = BG.copy()
    d = ImageDraw.Draw(img, "RGBA")
    tracked(d, W / 2, 66, "ROUND 1  ·  ANDROID EMULATOR  ·  15 NOV 2026", F("avd", 24), INK, 3.2)
    d.rectangle([110, 96, 970, 98], fill=rgba(INK))
    fa = fit(d, "fut", "AGENT", 900, 330)
    fg = fit(d, "fut", "GRAND PRIX", 900, 330)
    for off, col in (((7, 7), RED), ((0, 0), INK)):
        d.text((W / 2 + off[0], 300 + off[1]), "AGENT", font=fa, fill=rgba(col), anchor="mm")
        d.text((W / 2 + off[0], 530 + off[1]), "GRAND PRIX", font=fg, fill=rgba(col), anchor="mm")
    the = Image.new("RGBA", (260, 130), (0, 0, 0, 0))
    ImageDraw.Draw(the).text((130, 65), "The", font=F("sign", 104), fill=rgba(RED), anchor="mm")
    the = the.rotate(7, expand=True, resample=Image.BICUBIC)
    img.paste(the, (84, 92), the)
    a = prog(t, 0.35, 0.3)
    text(d, (W / 2, 690), "Which tool gets a coding agent", F("avd", 50), INK, a)
    text(d, (W / 2, 748), "through a mobile checkout fastest?", F("avd", 50), INK, a)
    for i, (arm, name, num, color) in enumerate(TEAMS):
        p = ease_back(prog(t, 1.0 + 0.3 * i, 0.3))
        if p <= 0:
            continue
        cx = 150 + i * 195
        roundel(d, cx, 862, 44 * p, num, color)
        text(d, (cx, 928), name, F("avh", 29), INK, clamp(p))
    d.rectangle([0, 1000, W, H], fill=rgba(RED))
    tracked(d, W / 2, 1040, "5 TOOLS  ·  18 STEPS  ·  1 MAC  ·  NO MODEL THINK TIME", F("avh", 26), CREAM, 3.0)
    return img


def table(d, platform, t, t0, y0, dy):
    rows = sorted(((MED[platform][arm], arm) for arm in MED[platform] if arm in TEAM))
    best = rows[0][0]
    n = len(rows)
    for k, (v, arm) in enumerate(rows):
        name, num, color = TEAM[arm]
        p = ease_out(prog(t, t0 + 0.16 * (n - 1 - k), 0.3))   # last place first, the winner last
        if p <= 0:
            continue
        y = y0 + k * dy
        x = (1 - p) * 90
        if k == 0:
            d.rectangle([36 + x, y - dy / 2 + 6, 1044 + x, y + dy / 2 - 6], fill=rgba(TEAL, p))
        ink = CREAM if k == 0 else INK
        text(d, (70 + x, y), str(k + 1), F("fut", 62), ink, p, anchor="lm")
        roundel(d, 168 + x, y, 30, num, color, p)
        text(d, (216 + x, y), name, F("avh", 46), ink, p, anchor="lm")
        number(d, 800 + x, y + 21, f"{v:.1f}", F("fut", 62), ink, p)
        text(d, (806 + x, y + 21), "s", F("fut", 34), ink, 0.7 * p, anchor="ls")
        if k:
            text(d, (1028 + x, y + 2), f"+{round(v, 1) - round(best, 1):.1f} s", F("avd", 34), FADED, p, anchor="rm")
        else:
            text(d, (1028 + x, y + 2), "WINNER", F("avh", 30), CREAM, p, anchor="rm")


def scene_class(t):
    img = BG.copy()
    d = ImageDraw.Draw(img, "RGBA")
    ios = t >= T_IOS
    for off, col in (((5, 5), RED), ((0, 0), INK)):
        d.text((40 + off[0], 104 + off[1]), "CLASSIFICATION", font=F("fut", 124), fill=rgba(col), anchor="lm")
    sub = "iOS SIMULATOR  ·  SAME TASK  ·  MEDIAN OF 5 RUNS" if ios else "ANDROID EMULATOR  ·  18 STEPS  ·  MEDIAN OF 5 RUNS"
    tracked(d, 44, 196, sub, F("avd", 25), RED if ios else INK, 2.6, align="l")
    d.rectangle([36, 226, 1044, 232], fill=rgba(RED))
    d.rectangle([36, 236, 1044, 238], fill=rgba(INK))
    if ios:
        table(d, "ios", t, T_IOS + 0.05, 330, 118)
        p = prog(t, T_IOS + 1.0, 0.2)
        stamp(img, 690, 880, [("FASTEST ON iOS TOO", "fut", 58)], RED, -6, scale=1 + 0.5 * (1 - ease_out(p)), a=clamp(p * 3))
    else:
        table(d, "android", t, T_CLASS + 0.2, 322, 112)
        ratio = MED["android"]["classic"] / MED["android"]["agemu"]
        p = prog(t, T_CLASS + 1.5, 0.2)
        stamp(img, 640, 936, [(f"{ratio:.0f}× FASTER THAN adb", "fut", 70)], RED, -6,
              scale=1 + 0.5 * (1 - ease_out(p)), a=clamp(p * 3))
    text(d, (W / 2, 1046), "2026-11-15  ·  method and every run are in the repo", F("avm", 21), FADED)
    return img


def scene_pits(t):
    img = BG.copy()
    d = ImageDraw.Draw(img, "RGBA")
    ft = fit(d, "fut", "SHORTER PIT STOPS", 770, 124)
    for off, col in (((5, 5), RED), ((0, 0), INK)):
        d.text((40 + off[0], 104 + off[1]), "SHORTER PIT STOPS", font=ft, fill=rgba(col), anchor="lm")
    why = Image.new("RGBA", (280, 150), (0, 0, 0, 0))
    ImageDraw.Draw(why).text((140, 75), "why?", font=F("sign", 120), fill=rgba(RED), anchor="mm")
    why = why.rotate(8, expand=True, resample=Image.BICUBIC)
    img.paste(why, (800, 18), why)
    text(d, (44, 204), "Every tool call is a pit stop. agemu's take milliseconds.", F("avd", 38), INK, prog(t, T_PITS + 0.15, 0.3), anchor="lm")
    d.rectangle([36, 240, 1044, 246], fill=rgba(RED))
    d.rectangle([36, 250, 1044, 252], fill=rgba(INK))
    rows = [("SCREENSHOT", "150 ms", "4–5 ms", "adb, simctl"),
            ("READ THE SCREEN", "2.0 s", "2–23 ms", "uiautomator"),
            ("RESET THE DEVICE", "14 s", "2.1 s", "cold boot")]
    for i, (label, old, new, via) in enumerate(rows):
        p = ease_out(prog(t, T_PITS + 0.45 + 0.4 * i, 0.3))
        if p <= 0:
            continue
        y = 352 + i * 208
        x = (1 - p) * 90
        tracked(d, 44 + x, y - 62, label, F("avh", 31), INK, 2.4, p, align="l")
        fo = F("fut", 86)
        wo = d.textlength(old, font=fo)
        text(d, (44 + x, y + 26), old, fo, FADED, p, anchor="lm")
        s = ease_out(prog(t, T_PITS + 0.7 + 0.4 * i, 0.2))
        if s > 0:
            d.line([(36 + x, y + 30), (36 + x + (wo + 16) * s, y + 22)], fill=rgba(RED), width=9)
        text(d, (44 + x, y + 92), via, F("avm", 26), FADED, p, anchor="lm")
        ax = 44 + x + wo + 56
        d.rectangle([ax, y + 21, ax + 70, y + 29], fill=rgba(INK, p))
        d.polygon([(ax + 62, y + 4), (ax + 96, y + 25), (ax + 62, y + 46)], fill=rgba(INK, p))
        text(d, (1040 + x, y + 26), new, F("fut", 132), TEAL, p, anchor="rm")
        if i < 2:
            d.rectangle([36, y + 132, 1044, y + 133], fill=rgba(RULE, p))
    text(d, (W / 2, 1046), "The emulator and the Simulator stay stock  ·  only the control layer is new",
         F("avm", 23), FADED, prog(t, T_PITS + 1.6, 0.3))
    return img


def scene_weigh(t):
    """The other half of the result: what a device costs the Mac while it is tested (README table)."""
    img = BG.copy()
    d = ImageDraw.Draw(img, "RGBA")
    ft = fit(d, "fut", "THE WEIGH-IN", 640, 124)
    for off, col in (((5, 5), RED), ((0, 0), INK)):
        d.text((40 + off[0], 104 + off[1]), "THE WEIGH-IN", font=ft, fill=rgba(col), anchor="lm")
    text(d, (44, 210), "Each device uses less of your Mac. Room for the IDE and the build.", F("avd", 36), INK,
         prog(t, T_WEIGH + 0.15, 0.3), anchor="lm")
    d.rectangle([36, 240, 1044, 246], fill=rgba(RED))
    d.rectangle([36, 250, 1044, 252], fill=rgba(INK))

    # the scale: the needle falls from the standard emulator's weight to agemu's
    cx, cy, R, top = 880, 172, 132, 8.0

    def at(v, r):
        a = math.radians(180 + 180 * v / top)
        return cx + r * math.cos(a), cy + r * math.sin(a)

    d.pieslice([cx - R, cy - R, cx + R, cy + R], 180, 360, fill=rgba(CREAM), outline=rgba(INK), width=5)
    d.arc([cx - R + 12, cy - R + 12, cx + R - 12, cy + R - 12], 180 + 180 * 4.3 / top, 180 + 180 * 5.4 / top,
          fill=rgba(TEAL), width=16)
    for v in range(0, int(top) + 1):
        d.line([at(v, R - 6), at(v, R - (26 if v % 4 == 0 else 16))], fill=rgba(INK), width=4 if v % 4 == 0 else 2)
    d.line([at(7.5, R - 6), at(7.5, R - 30)], fill=rgba(RED), width=7)
    for v in (0, 4, 8):
        text(d, at(v, R - 46), str(v), F("avh", 24), INK)
    text(d, (cx, cy - 34), "GB", F("avh", 22), FADED)
    q = max(0.0, t - (T_WEIGH + 0.55))
    v = 4.85 + 2.65 * math.exp(-3.4 * q) * math.cos(8.5 * q)
    d.line([(cx, cy), at(v, R - 22)], fill=rgba(INK), width=7)
    d.ellipse([cx - 11, cy - 11, cx + 11, cy + 11], fill=rgba(RED))

    rows = [("ANDROID EMULATOR  ·  MEMORY", "7.5 GB", "4.3–5.4 GB", "as Android Studio runs it"),
            ("iOS SIMULATOR  ·  MEMORY DURING THE RACE", "3.8–4.0 GB", "1.9 GB", "the standard Simulator"),
            ("ANDROID EMULATOR  ·  CPU AT IDLE", "86%", "19–24%", "of one core")]
    fo = F("fut", 76)
    size = 132     # one size for the three new values: the largest that fits every row
    for _, old, new, _ in rows:
        room = 1040 - (44 + d.textlength(old, font=fo) + 56 + 96 + 34)
        size = min(size, fit(d, "fut", new, room, 132).size)
    for i, (label, old, new, via) in enumerate(rows):
        p = ease_out(prog(t, T_WEIGH + 0.45 + 0.4 * i, 0.3))
        if p <= 0:
            continue
        y = 352 + i * 208
        x = (1 - p) * 90
        tracked(d, 44 + x, y - 62, label, F("avh", 31), INK, 2.4, p, align="l")
        wo = d.textlength(old, font=fo)
        text(d, (44 + x, y + 26), old, fo, FADED, p, anchor="lm")
        st = ease_out(prog(t, T_WEIGH + 0.7 + 0.4 * i, 0.2))
        if st > 0:
            d.line([(36 + x, y + 30), (36 + x + (wo + 16) * st, y + 22)], fill=rgba(RED), width=9)
        text(d, (44 + x, y + 88), via, F("avm", 26), FADED, p, anchor="lm")
        ax = 44 + x + wo + 56
        d.rectangle([ax, y + 21, ax + 70, y + 29], fill=rgba(INK, p))
        d.polygon([(ax + 62, y + 4), (ax + 96, y + 25), (ax + 62, y + 46)], fill=rgba(INK, p))
        text(d, (1040 + x, y + 26), new, F("fut", size), TEAL, p, anchor="rm")
        if i < 2:
            d.rectangle([36, y + 132, 1044, y + 133], fill=rgba(RULE, p))
    text(d, (W / 2, 1046), "Per device, on an M2 Pro Mac  ·  the idle figure is one session on a busy Mac, so it is rough",
         F("avm", 22), FADED, prog(t, T_WEIGH + 1.5, 0.3))
    return img


def scene_card(t):
    img = BG.copy()
    d = ImageDraw.Draw(img, "RGBA")
    tracked(d, W / 2, 66, "WINNER  ·  ANDROID AND iOS  ·  OPEN SOURCE, MIT", F("avd", 24), INK, 3.2)
    d.rectangle([110, 96, 970, 98], fill=rgba(INK))
    p = ease_back(prog(t, T_CARD, 0.4), 1.4)
    f = F("fut", fit(d, "fut", "agemu", 860, 340).size * (0.8 + 0.2 * p))
    for off, col in (((8, 8), INK), ((0, 0), TEAL)):
        d.text((W / 2 + off[0], 286 + off[1]), "agemu", font=f, fill=rgba(col), anchor="mm")
    if t > T_CARD + 0.3:
        roundel(d, 966, 168, 52 * ease_back(prog(t, T_CARD + 0.3, 0.3)), 5, TEAL)
    a = prog(t, T_CARD + 0.4, 0.3)
    text(d, (W / 2, 536), "The fast control layer for the Android", F("avd", 50), INK, a)
    text(d, (W / 2, 594), "emulator and iOS Simulator, for coding agents", F("avd", 50), INK, a)
    b = ease_out(prog(t, T_CARD + 0.8, 0.3))
    if b > 0:
        y = 676 + (1 - b) * 30
        d.rectangle([36, y, 1044, y + 112], fill=rgba(INK, b))
        cmd = "claude mcp add agemu -- npx -y agentic-emulator@latest mcp"
        fm = fit(d, "typeb", "$ " + cmd, 960, 32)
        text(d, (W / 2, y + 56), "$ " + cmd, fm, CREAM, b)
    c = prog(t, T_CARD + 1.2, 0.3)
    tracked(d, W / 2, 836, "CLAUDE CODE  ·  CODEX  ·  CURSOR  ·  ANY MCP CLIENT", F("avh", 27), INK, 2.6, c)
    d.rectangle([0, 934, W, H], fill=rgba(RED, prog(t, T_CARD + 1.5, 0.25)))
    text(d, (W / 2, 1004), "github.com/piotr-graczyk-dev/agentic-emulator", fit(d, "fut", "github.com/piotr-graczyk-dev/agentic-emulator", 980, 62),
         CREAM, prog(t, T_CARD + 1.6, 0.3))
    return img


SCENES = [(0.0, scene_title), (T_GRID, scene_race), (T_CLASS, scene_class), (T_PITS, scene_pits),
          (T_WEIGH, scene_weigh), (T_CARD, scene_card)]
WIPE = 0.34


def wipe(old, new, q):
    """Racing stripes sweep across: red, cream, ink; the new scene comes in behind them."""
    lead = -400 + (W + 1100) * ease_io(q)
    out = old.copy()
    m = Image.new("L", (W, H), 0)
    ImageDraw.Draw(m).polygon([(-10, -10), (lead - 300, -10), (lead - 520, H + 10), (-10, H + 10)], fill=255)
    out.paste(new, (0, 0), m)
    d = ImageDraw.Draw(out)
    for dx, wid, col in ((-300, 150, RED), (-150, 40, CREAM), (-110, 110, INK)):
        d.polygon([(lead + dx, -10), (lead + dx + wid, -10), (lead + dx + wid - 220, H + 10), (lead + dx - 220, H + 10)], fill=col)
    return out


def frame(t, k=0):
    i = max(j for j, (t0, _) in enumerate(SCENES) if t0 <= t)
    img = SCENES[i][1](t)
    if i > 0 and t < SCENES[i][0] + WIPE / 2:
        img = wipe(SCENES[i - 1][1](SCENES[i][0] - 0.02), img, 0.5 + (t - SCENES[i][0]) / WIPE)
    elif i + 1 < len(SCENES) and t > SCENES[i + 1][0] - WIPE / 2:
        img = wipe(img, SCENES[i + 1][1](SCENES[i + 1][0] + WIPE / 2), 0.5 - (SCENES[i + 1][0] - t) / WIPE)
    # the flag: a bump of the camera
    if 0 <= t - T_FIN < 0.5:
        amp = 16 * math.exp(-9 * (t - T_FIN))
        img = ImageChops.offset(img, int(amp * math.sin((t - T_FIN) * 70)), int(amp * math.cos((t - T_FIN) * 55)))
    if GRAIN_MODE != "none":
        img = ImageChops.multiply(img, GRAIN[(k // 3) % len(GRAIN) if GRAIN_MODE == "boil" else 0])
    if t > T_END - 0.35:
        img = Image.blend(img, Image.new("RGB", (W, H), INK), ease_io((t - (T_END - 0.35)) / 0.35))
    return img


def write_gif(path, t0, t1, size, fps):
    """A looping GIF of t0..t1 without the grain (GIF cannot hold it), two-pass palette."""
    global GRAIN_MODE, BG
    mode, GRAIN_MODE = GRAIN_MODE, "none"
    bg, BG = BG, Image.new("RGB", (W, H), PAPER)   # flat paper: a GIF bands the soft edge shading
    tmp = path + ".tmp.mkv"
    ff = subprocess.Popen(["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24",
                           "-s", f"{size}x{size}", "-r", str(fps), "-i", "-", "-c:v", "libx264rgb", "-crf", "0",
                           "-preset", "ultrafast", tmp], stdin=subprocess.PIPE)
    n = int((t1 - t0) * fps)
    for k in range(n):
        t = t0 + k / fps
        ff.stdin.write(frame(t, int(t * FPS)).resize((size, size), Image.LANCZOS).tobytes())
    ff.stdin.close()
    if ff.wait() != 0:
        sys.exit("ffmpeg failed")
    pal = path + ".palette.png"
    subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", tmp, "-vf",
                    "palettegen=max_colors=96:stats_mode=diff", pal], check=True)
    subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", tmp, "-i", pal, "-lavfi",
                    "paletteuse=dither=none:diff_mode=rectangle", "-loop", "0", path], check=True)
    os.remove(tmp)
    os.remove(pal)
    GRAIN_MODE, BG = mode, bg
    print(f"{path}  {os.path.getsize(path) / 1e6:.1f} MB", file=sys.stderr)


def main():
    global GRAIN_MODE
    ap = argparse.ArgumentParser(description="The agent race as a vintage race broadcast (1080x1080).")
    ap.add_argument("--out", help="output mp4 (no sound)")
    ap.add_argument("--grain", choices=("static", "boil", "none"), default="static")
    ap.add_argument("--crf", type=int, default=20)
    ap.add_argument("--gif", help="also write a GIF")
    ap.add_argument("--gif-range", default=f"0,{T_END}", help="start,end in seconds (default: all of it)")
    ap.add_argument("--gif-size", type=int, default=600)
    ap.add_argument("--gif-fps", type=int, default=10)
    ap.add_argument("--poster", help="also write one frame as a PNG")
    ap.add_argument("--poster-at", type=float, default=2.8)
    ap.add_argument("--sheet", help="write a contact sheet of the frames at --at instead of a video")
    ap.add_argument("--at", default="0.4,2.4,4.6,7,12,16.15,18.6,20.9,22.6,24.3,26.4,29.6,31,33.5")
    ap.add_argument("--cols", type=int, default=4)
    ap.add_argument("--size", type=int, default=540)
    args = ap.parse_args()
    GRAIN_MODE = args.grain
    print(f"lights out {T0:.2f} s, flag {T_FIN:.2f} s, order {[r['name'] for r in ORDER]}", file=sys.stderr)
    if args.sheet:
        ts = [float(x) for x in args.at.split(",")]
        cols = min(args.cols, len(ts))
        s = args.size
        sheet = Image.new("RGB", (cols * s, math.ceil(len(ts) / cols) * s), (0, 0, 0))
        for k, t in enumerate(ts):
            im = frame(t, int(t * FPS)).resize((s, s), Image.LANCZOS)
            dd = ImageDraw.Draw(im)
            dd.rectangle([0, 0, 74, 26], fill=(0, 0, 0))
            dd.text((6, 2), f"{t:g}s", font=C.font(20, "Bold"), fill=(255, 230, 0))
            sheet.paste(im, ((k % cols) * s, (k // cols) * s))
        sheet.save(args.sheet)
        print(args.sheet)
        return
    if args.poster:
        frame(args.poster_at, int(args.poster_at * FPS)).save(args.poster)
        print(args.poster)
    if args.gif:
        a, b = (float(x) for x in args.gif_range.split(","))
        write_gif(args.gif, a, b, args.gif_size, args.gif_fps)
    if not args.out:
        return
    ff = subprocess.Popen(["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24",
                           "-s", f"{W}x{H}", "-r", str(FPS), "-i", "-", "-c:v", "libx264", "-crf", str(args.crf),
                           "-preset", "slow", "-pix_fmt", "yuv420p", "-movflags", "+faststart", args.out],
                          stdin=subprocess.PIPE)
    n = int(T_END * FPS)
    for k in range(n):
        ff.stdin.write(frame(k / FPS, k).tobytes())
        if k % (FPS * 5) == 0:
            print(f"  {k / FPS:5.1f} s / {T_END:g}", file=sys.stderr)
    ff.stdin.close()
    if ff.wait() != 0:
        sys.exit("ffmpeg failed")
    print(args.out)


if __name__ == "__main__":
    main()
