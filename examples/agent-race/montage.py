#!/usr/bin/env python3
"""One shareable highlight video from several agent races, plus an optional README teaser.

    uv run -q --with pillow examples/agent-race/montage.py \
        --race "Android=classic:runs/classic-2,agent-device:runs/ad-1,argent:runs/argent-1,agemu:runs/agemu-2" \
        --race "iOS=agent-device:runs/ios-ad-1,argent:runs/ios-argent-1,agemu:runs/ios-agemu-1" \
        --out montage.mp4 --gif teaser.gif

Title card, then per race a section card and the race drawn with compose.py's panels: real time
until agemu (or the fastest run) finishes plus 1.5 s, then sped up (badge on screen, the timers
keep the true task time) until the slowest run finishes, then its closing card. A summary card
with every race's task times ends the video. --gif (or a .webp path) writes a ~960 px teaser of
one race's speed-up through the summary. Frames are drawn once from the run directories; no
rendered video is re-encoded. Needs Pillow and ffmpeg.
"""
import argparse
import os
import shutil
import subprocess
import sys
import tempfile

from PIL import Image, ImageDraw

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import compose as C  # noqa: E402

W, H = C.W, C.H
TITLE_S, SECTION_S, SUMMARY_S = 3.0, 1.5, 5.0
CARD_HOLD = 3.0      # closing card on screen, after its fade-in
REAL_AFTER = 1.5     # real time after agemu finishes, before speeding up
MAX_FAST_S = 10.0    # the sped-up part takes at most this long
SPEEDS = (2, 4, 6, 8, 10, 12, 16, 20, 24, 32, 48, 64)
FADE = 0.3
GIF_MAX = 8 * 1024 * 1024

F = {
    "hero": C.font(76, "Bold"), "hero_sub": C.font(28), "section": C.font(110, "Bold"),
    "chip": C.font(26, "Semibold"), "chip_sub": C.font(17), "sum_title": C.font(46, "Bold"),
    "sum_sub": C.font(22), "group": C.font(28, "Bold"), "arm": C.font(24, "Semibold"),
    "val": C.font(26, "Bold"), "speed": C.font(22),
}


def parse_race(spec):
    name, sep, rest = spec.partition("=")
    if not sep:
        sys.exit(f"--race wants NAME=ARM:DIR,ARM:DIR,..., got {spec!r}")
    runs = []
    for part in rest.split(","):
        arm, sep, d = part.partition(":")
        if not sep or arm not in C.ARMS:
            sys.exit(f"bad run {part!r} in --race {name}: want ARM:DIR with ARM one of {', '.join(C.ARMS)}")
        runs.append(C.load_run(d, arm))
    return name.strip(), runs


def faded(img, k, n, fps):
    """Fade a card in over its first FADE seconds and out over its last."""
    a = min(1.0, (k + 1) / (FADE * fps), (n - k) / (FADE * fps))
    return C.blend(Image.new("RGB", (W, H), C.BG), img, a) if a < 1 else img


def arm_chips(draw, arms, y, center=True):
    """A row of colored arm names with their subtitles, centred."""
    items = [(C.ARMS[a], max(draw.textlength(C.ARMS[a]["name"], font=F["chip"]) + 26,
                             draw.textlength(C.ARMS[a]["sub"], font=F["chip_sub"]))) for a in arms]
    gap = 56
    total = sum(w for _, w in items) + gap * (len(items) - 1)
    x = (W - total) / 2 if center else 120
    for st, w in items:
        draw.rounded_rectangle([x, y + 8, x + 14, y + 22], 4, fill=st["color"])
        draw.text((x + 26, y), st["name"], font=F["chip"], fill=C.TEXT)
        draw.text((x, y + 40), st["sub"], font=F["chip_sub"], fill=C.MUTED)
        x += w + gap


def centred(draw, y, text, f, fill):
    draw.text(((W - draw.textlength(text, font=f)) / 2, y), text, font=f, fill=fill)


def title_card(races):
    arms = []
    for _, runs in races:
        arms += [r["arm"] for r in runs if r["arm"] not in arms]
    steps = len(races[0][1][0]["summary"]["steps"])
    img = Image.new("RGB", (W, H), C.BG)
    d = ImageDraw.Draw(img)
    centred(d, 330, f"One agent task, {C.NUM_WORDS.get(len(arms), str(len(arms)))} tool stacks", F["hero"], C.TEXT)
    centred(d, 440, f"Agemu Shop: sign in, scroll, search, cart, checkout · {steps} steps · no model think time",
            F["hero_sub"], C.MUTED)
    platforms = " and ".join(name for name, _ in races)
    centred(d, 490, f"Each stack runs the same plan on {platforms}, timed from the first call to the order number",
            F["chip_sub"], C.DIM)
    arm_chips(d, arms, 640)
    return img


def section_card(name, runs):
    img = Image.new("RGB", (W, H), C.BG)
    d = ImageDraw.Draw(img)
    centred(d, 330, name, F["section"], C.TEXT)
    centred(d, 480, f"{len(runs)} tool stacks · real time until agemu finishes, then sped up",
            F["hero_sub"], C.MUTED)
    arm_chips(d, [r["arm"] for r in runs], 600)
    return img


def anchor_time(runs):
    ok = [r for r in runs if r["ok"]]
    agemu = next((r for r in ok if r["arm"] == "agemu"), None)
    if agemu:
        return agemu["total"]
    return min(r["total"] for r in (ok or runs))


def time_map(race, fps):
    """[(task time, badge)] per output frame: real time, sped up, then real time for the card."""
    slow = race.slow
    fast_from = anchor_time(race.runs) + REAL_AFTER
    out, t = [], -0.5
    while t < min(fast_from, slow):
        out.append((t, None))
        t += 1 / fps
    rest = slow - t
    if rest > 3:
        speed = next((s for s in SPEEDS if rest / s <= MAX_FAST_S), SPEEDS[-1])
        while t < slow:
            out.append((t, f"{speed}× speed"))
            t += speed / fps
    end = slow + C.CARD_DELAY + C.CARD_FADE + CARD_HOLD
    while t < end:
        out.append((t, None))
        t += 1 / fps
    return out


def summary_card(races, a=1.0):
    """Bar chart of task time per arm and platform; a in 0..1 grows the bars."""
    img = Image.new("RGB", (W, H), C.BG)
    d = ImageDraw.Draw(img)
    steps = len(races[0][1][0]["summary"]["steps"])
    d.text((120, 70), f"Task time for the same {steps}-step task", font=F["sum_title"], fill=C.TEXT)
    d.text((120, 132), "One run per stack and platform · same plan, same Mac · no model think time · shorter is faster",
           font=F["sum_sub"], fill=C.MUTED)
    nrows = sum(len(runs) for _, runs in races)
    row_h = min(64, (H - 250 - 70 - 64 * len(races)) // max(1, nrows))
    top = max(r["total"] for _, runs in races for r in runs)
    bx, bw = 440, W - 440 - 520
    block = 64 * len(races) + row_h * nrows
    y = 190 + max(0, (H - 190 - 60 - block) // 3)
    ease = 1 - (1 - min(1.0, a)) ** 3
    for name, runs in races:
        y += 14
        d.text((120, y), name, font=F["group"], fill=C.TEXT)
        y += 50
        base = next((r for r in runs if r["arm"] == "agemu" and r["ok"]), None)
        for r in runs:
            st = r["style"]
            d.text((150, y + (row_h - 30) // 2 - 2), st["name"], font=F["arm"], fill=st["color"])
            bh = min(34, row_h - 18)
            by = y + (row_h - bh) // 2
            d.rounded_rectangle([bx, by, bx + bw, by + bh], bh // 2, fill=C.PANEL)
            length = max(bh, int(bw * r["total"] / top * ease))
            col = st["color"] if r["ok"] else tuple(c // 3 for c in st["color"])
            d.rounded_rectangle([bx, by, bx + length, by + bh], bh // 2, fill=col)
            lx = bx + bw + 24
            if not r["ok"]:
                d.text((lx, by + bh // 2 - 15), f"✗ failed after {r['total']:.1f} s", font=F["val"], fill=C.RED)
            else:
                val = f"{r['total'] * ease:.1f} s"
                d.text((lx, by + bh // 2 - 16), val, font=F["val"], fill=C.TEXT)
                if base and r is not base and ease >= 1:
                    ratio = r["total"] / base["total"]
                    txt = f"agemu {ratio:.1f}× faster" if ratio >= 1 else f"{1 / ratio:.1f}× faster than agemu"
                    d.text((lx + 130, by + bh // 2 - 12), txt, font=F["speed"], fill=C.ARMS["agemu"]["color"])
                elif r is base and ease >= 1 and r["total"] <= min(x["total"] for x in runs if x["ok"]):
                    d.text((lx + 130, by + bh // 2 - 12), "fastest", font=F["speed"], fill=C.MUTED)
            y += row_h
    return img


def build(races, fps):
    """Segments: (tag, frame count, draw(k, n) -> Image)."""
    def card(img):
        def draw(k, n):
            return faded(img, k, n, fps)
        return draw

    def race_frames(race, tm):
        def draw(k, n):
            im = race.frame(tm[k][0], badge=tm[k][1])
            return faded(im, k, n, fps) if k >= n - FADE * fps else im
        return draw

    def summary(k, n):  # fades in only: the video ends on the chart
        return faded(summary_card(races, (k + 1) / (0.9 * fps)), k, n + 10 * fps, fps)

    segs = [("title", int(TITLE_S * fps), card(title_card(races)))]
    for i, (name, runs) in enumerate(races):
        segs.append(("section", int(SECTION_S * fps), card(section_card(name, runs))))
        race = C.Race(runs, title=f"{name} · one agent task, {C.NUM_WORDS.get(len(runs), len(runs))} tool stacks")
        tm = time_map(race, fps)
        segs.append((f"race{i}", len(tm), race_frames(race, tm)))
    segs.append(("summary", int(SUMMARY_S * fps), summary))
    return segs


def write_teaser(path, src, fps):
    """GIF (palettegen/paletteuse) or animated WebP from the lossless teaser intermediate."""
    if path.lower().endswith(".webp"):
        return write_webp(path, src, fps)
    pal = os.path.join(os.path.dirname(src), "palette.png")
    for width, colors in ((960, 128), (960, 96), (840, 96), (720, 80)):
        scale = "null" if width == 960 else f"scale={width}:-1:flags=lanczos"
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", src, "-vf",
                        f"{scale},palettegen=max_colors={colors}:stats_mode=diff", pal], check=True)
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", src, "-i", pal, "-lavfi",
                        f"[0:v]{scale}[v];[v][1:v]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle",
                        "-loop", "0", path], check=True)
        size = os.path.getsize(path)
        print(f"  teaser {width} px, {colors} colors: {size / 1e6:.1f} MB", file=sys.stderr)
        if size <= GIF_MAX:
            return
    print("  teaser is still over 8 MB; consider a shorter --gif-race", file=sys.stderr)


def write_webp(path, src, fps):
    from PIL import features
    if not features.check("webp"):
        sys.exit("this Pillow has no WebP support; use a .gif path")
    w, h = 960, 540
    p = subprocess.Popen(["ffmpeg", "-loglevel", "error", "-i", src, "-f", "rawvideo", "-pix_fmt", "rgb24", "-"],
                         stdout=subprocess.PIPE)
    frames = []
    while True:
        buf = p.stdout.read(w * h * 3)
        if len(buf) < w * h * 3:
            break
        frames.append(Image.frombytes("RGB", (w, h), buf))
    p.wait()
    for q in (70, 55, 40):
        frames[0].save(path, save_all=True, append_images=frames[1:], duration=round(1000 / fps), loop=0,
                       quality=q, method=4)
        size = os.path.getsize(path)
        print(f"  teaser webp q{q}: {size / 1e6:.1f} MB", file=sys.stderr)
        if size <= GIF_MAX:
            return


def main():
    ap = argparse.ArgumentParser(description="Highlight video from several agent races.")
    ap.add_argument("--race", action="append", required=True, metavar="NAME=ARM:DIR,...",
                    help="a race: its name and 2-4 runs in column order; repeat per platform")
    ap.add_argument("--out", required=True, help="output mp4")
    ap.add_argument("--gif", help="also write a ~960 px teaser (.gif, or .webp)")
    ap.add_argument("--gif-race", type=int, default=1, help="which race (1-based) the teaser shows (default 1)")
    ap.add_argument("--fps", type=int, default=C.FPS)
    args = ap.parse_args()

    races = [parse_race(s) for s in args.race]
    segs = build(races, args.fps)
    total = sum(n for _, n, _ in segs)
    print(f"  {total / args.fps:.1f} s, {total} frames", file=sys.stderr)

    teaser_tags = {f"race{args.gif_race - 1}", "summary"}
    step = max(1, round(args.fps / 11))
    tmp = tempfile.mkdtemp(prefix="montage-")
    tsrc = os.path.join(tmp, "teaser.mkv")
    tee = None
    if args.gif:
        tee = subprocess.Popen(["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24",
                                "-s", "960x540", "-r", f"{args.fps / step:g}", "-i", "-",
                                "-c:v", "libx264rgb", "-crf", "0", "-preset", "ultrafast", tsrc], stdin=subprocess.PIPE)
    ff = C.ffmpeg_writer(args.out, args.fps, crf=24, tune=None)
    i = 0
    try:
        for tag, n, draw in segs:
            for k in range(n):
                im = draw(k, n)
                ff.stdin.write(im.tobytes())
                if tee and tag in teaser_tags and k % step == 0:
                    tee.stdin.write(im.reduce(2).tobytes())
                i += 1
                if i % (args.fps * 10) == 0:
                    print(f"  {i / args.fps:6.1f} s / {total / args.fps:.1f}", file=sys.stderr)
        ff.stdin.close()
        if ff.wait() != 0:
            sys.exit("ffmpeg failed")
        print(args.out)
        if tee:
            tee.stdin.close()
            if tee.wait() != 0:
                sys.exit("ffmpeg (teaser) failed")
            write_teaser(args.gif, tsrc, args.fps / step)
            print(f"{args.gif} ({os.path.getsize(args.gif) / 1e6:.1f} MB)")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
