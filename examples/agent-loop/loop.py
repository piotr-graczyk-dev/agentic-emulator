#!/usr/bin/env python3
"""agent-loop: one shopping task in the Agemu Shop app (examples/shop), done by a real coding
agent (Claude Code, headless) that has one device tool at a time.

    loop.py run    --platform ios --arm agemu --device <udid> --out out/ios/runs/agemu-1
    loop.py series --platform ios --std <udid> --agemu <udid> --rounds 5 --out out/ios

Arms: adb (Android only: a shell with adb), mobile-mcp, agent-device, argent, agemu. Every arm
but adb gets that tool's MCP server and nothing else: no shell and no file tools. The prompt is
the same for every arm; only the device line differs.

The agent's output stream is kept with the time each event arrived (stream.jsonl), so a run can
be read again later; summary.json has the totals: wall time, time inside tool calls, time the
model took, turns, tool calls, tokens and list-price cost. A run counts as done when the agent's
last message has the order number, which the app derives from the cart's total, the number of
items and the name on the order.

Python's standard library only. Needs the `claude` CLI, and for the rivals the tools from
examples/agent-race (TOOLS, default ~/.cache/agemu-race-tools/node_modules/.bin).
"""
import argparse
import glob
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
APP_ID = "dev.agemu.shop"
ORDER = "31614"  # Bath Towel $32.00 + 2 x Desk Lamp $29.99, for Ada Lovelace
ARMS = {"android": ["adb", "mobile-mcp", "agent-device", "argent", "agemu"],
        "ios": ["mobile-mcp", "agent-device", "argent", "agemu"]}

TASK = """You are testing a mobile app. Device: {device}. The app "Agemu Shop" ({app}) is \
installed on it and is not running.

Do this in the app:
1. Sign in with the e-mail test@agemu.dev and the password hunter2.
2. Add one Bath Towel to the cart.
3. Add two Desk Lamps to the cart.
4. Open the cart and check that the total is $91.98. If it is not, stop and report what you see.
5. Check out with this shipping address: Ada Lovelace, 12 Analytical St, London, N1 9GU. \
Place the order.

Finish with one line: ORDER <the order number on the confirmation screen>."""


def sh(cmd, timeout=120, env=None):
    """Runs a command; returns (exit status, output)."""
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env=env)
        return p.returncode, (p.stdout + p.stderr).strip()
    except subprocess.TimeoutExpired:
        return 124, "timed out after %ss: %s" % (timeout, " ".join(cmd[:3]))


def loadavg():
    return subprocess.run(["sysctl", "-n", "vm.loadavg"], capture_output=True, text=True).stdout.strip("{} \n")


def quiet(wait, max_load):
    """Holds a run back while the host is loaded, for `wait` seconds at most."""
    end = time.time() + wait
    while time.time() < end and float(loadavg().split()[0]) >= max_load:
        time.sleep(5)


def adb():
    sdk = os.environ.get("ANDROID_HOME") or os.environ.get("ANDROID_SDK_ROOT") or os.path.expanduser("~/Library/Android/sdk")
    return os.path.join(sdk, "platform-tools", "adb")


def serial_of(avd):
    """The adb serial of a running emulator, from its discovery file."""
    for f in glob.glob(os.path.expanduser("~/Library/Caches/TemporaryItems/avd/running/pid_*.ini")):
        kv = dict(l.strip().split("=", 1) for l in open(f) if "=" in l)
        if kv.get("avd.id") == avd:
            return "emulator-" + kv["port.serial"]
    raise SystemExit("no running emulator for " + avd)


class Ctx:
    """What one run needs to know about its device and tools."""

    def __init__(self, a):
        self.platform, self.arm, self.device, self.out = a.platform, a.arm, a.device, os.path.abspath(a.out)
        self.tools = a.tools
        self.agemu = os.path.abspath(a.agemu_bin)
        self.model = a.model
        self.timeout = a.timeout
        self.ios = self.platform == "ios"
        self.serial = None if self.ios else serial_of(self.device)
        self.udid = self.device if self.ios else self.serial  # the id the rivals use
        self.agemu_id = ("ios:" if self.ios else "android:") + self.device
        # The tools' own processes: no telemetry, and agemu's daemon from its own state directory.
        self.env = dict(os.environ, DO_NOT_TRACK="1", MOBILEMCP_DISABLE_TELEMETRY="1")
        if a.agemu_home:
            self.env["AGEMU_HOME"] = a.agemu_home
        self.ad = os.path.join(self.tools, "agent-device")
        self.ar = os.path.join(self.tools, "argent")
        self.mm = os.path.join(self.tools, "mcp-server-mobile")
        self.ad_sel = ["--platform", "ios", "--udid", self.device] if self.ios else ["--platform", "android", "--serial", self.serial]

    def device_line(self):
        if self.ios:
            name = sim_name(self.device)
            return 'the booted iOS Simulator "%s" (iPhone 17 Pro), UDID %s' % (name, self.device)
        return 'the running Android emulator "%s", adb serial %s' % (self.device, self.serial)


def sim_name(udid):
    out = subprocess.run(["xcrun", "simctl", "list", "devices", "-j"], capture_output=True, text=True).stdout
    for devs in json.loads(out)["devices"].values():
        for d in devs:
            if d["udid"] == udid:
                return d["name"]
    return udid


# --- the tools' daemons ---------------------------------------------------------------------

def release(c):
    """Stops the rivals' daemons and on-device helpers, so the next arm starts clean (they hold
    Android's one UiAutomation connection, and the iOS test runners)."""
    sh([c.ad, "close"], 30, c.env)
    sh([c.ad, "daemon", "stop"], 30, c.env)
    sh([c.ar, "server", "stop"], 30, c.env)
    subprocess.run(["pkill", "-f", os.path.join(os.path.dirname(os.path.dirname(c.tools)), "node_modules", "@mobilenext") + "/"],
                   capture_output=True)
    if c.serial:
        for p in ["com.callstack.agentdevice.snapshothelper", "com.callstack.agentdevice.imehelper",
                  "com.argent.androiddevtools", "com.mobilenext.devicekit"]:
            sh([adb(), "-s", c.serial, "shell", "am", "force-stop", p], 10)
        sh([adb(), "-s", c.serial, "shell", "pkill", "-f", "com.mobilenext.mobilecli"], 10)
    time.sleep(2)


class MCP:
    """A minimal MCP client over stdio, for mobile-mcp's warm-up."""

    def __init__(self, argv, env):
        self.p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, env=env)
        self.n = 0
        self.req("initialize", {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "agent-loop", "version": "1"}})
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def send(self, m):
        self.p.stdin.write(json.dumps(m) + "\n")
        self.p.stdin.flush()

    def req(self, method, params):
        self.n += 1
        self.send({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params})
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError("the MCP server closed its output")
            m = json.loads(line)
            if m.get("id") == self.n:
                if "error" in m:
                    raise RuntimeError(m["error"].get("message", "error"))
                return m["result"]

    def tool(self, name, args):
        r = self.req("tools/call", {"name": name, "arguments": args})
        text = "".join(b.get("text", "") for b in r.get("content", []))
        if r.get("isError"):
            raise RuntimeError(text[:300])
        return text

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(10)
        except Exception:
            self.p.kill()


def retry(what, f, tries=3):
    err = None
    for _ in range(tries):  # a daemon's first start can time out
        try:
            return f()
        except Exception as e:  # noqa: BLE001
            err = e
            time.sleep(3)
    raise RuntimeError("warm-up %s: %s" % (what, err))


def must(cmd, c, timeout=180):
    rc, out = sh(cmd, timeout, c.env)
    if rc != 0:
        raise RuntimeError(out[-300:])
    return out


def warm(c):
    """Untimed, as in examples/agent-race: the tool's daemon and on-device helper are started
    before the task, so no arm pays for a cold start inside the timed run."""
    if c.arm == "adb":
        retry("adb", lambda: must([adb(), "-s", c.serial, "shell", "true"], c))
    elif c.arm == "agemu":
        retry("agemu", lambda: must([c.agemu, "ui", "-d", c.agemu_id], c))
    elif c.arm == "agent-device":
        retry("agent-device open", lambda: must([c.ad, "open", APP_ID, "--relaunch"] + c.ad_sel, c))
        retry("agent-device snapshot", lambda: must([c.ad, "snapshot"] + c.ad_sel, c))
        sh([c.ad, "close"], 30, c.env)  # the agent opens its own session
    elif c.arm == "argent":
        subprocess.Popen([c.ar, "server", "start"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=c.env)
        for _ in range(180):
            if "ok" in sh([c.ar, "server", "status"], 20, c.env)[1].split("health:")[-1][:8]:
                break
            time.sleep(1)
        retry("argent launch-app", lambda: must([c.ar, "run", "launch-app", "--udid", c.udid, "--bundleId", APP_ID], c))
        retry("argent describe", lambda: must([c.ar, "run", "describe", "--udid", c.udid], c))
    elif c.arm == "mobile-mcp":
        m = MCP([c.mm], c.env)
        try:
            listed = retry("mobile-mcp devices", lambda: m.tool("mobile_list_available_devices", {}))
            if c.udid not in listed:
                raise RuntimeError("mobile-mcp does not list the device %s" % c.udid)
            retry("mobile-mcp launch", lambda: m.tool("mobile_launch_app", {"device": c.udid, "packageName": APP_ID}))
            retry("mobile-mcp list", lambda: m.tool("mobile_list_elements_on_screen", {"device": c.udid}))
        finally:
            m.close()


def reset_app(c):
    """Untimed: one launch (the first one after a device resumes can take seconds), then the app
    stopped and the home screen in front, so the agent's launch is a cold start, signed out."""
    if c.ios:
        sh(["xcrun", "simctl", "launch", c.device, APP_ID], 60)
        time.sleep(1)
        sh(["xcrun", "simctl", "terminate", c.device, APP_ID], 30)
        time.sleep(2)
    else:
        sh([adb(), "-s", c.serial, "shell", "am", "start", "-W", "-n", APP_ID + "/.LoginActivity"], 60)
        time.sleep(1)
        sh([adb(), "-s", c.serial, "shell", "am", "force-stop", APP_ID], 30)
        sh([adb(), "-s", c.serial, "shell", "input", "keyevent", "KEYCODE_HOME"], 30)
        time.sleep(1.5)


# --- the agent ------------------------------------------------------------------------------

def workspace(c):
    """An empty project for the agent, in an empty home directory outside this user's: the
    arm's MCP server, and the rules file a tool ships for agents. So the agent has no user
    memory (a CLAUDE.md that names a tool would steer it), skills, hooks or other servers. The
    tools themselves keep the real home, where their daemons and keys are."""
    home = tempfile.mkdtemp(prefix="agent-loop-")
    ws, cfg = os.path.join(home, "project"), os.path.join(home, ".claude")
    os.makedirs(ws)
    os.makedirs(cfg)
    tool_env = {k: c.env[k] for k in ("DO_NOT_TRACK", "MOBILEMCP_DISABLE_TELEMETRY", "AGEMU_HOME") if k in c.env}
    tool_env["HOME"] = os.path.expanduser("~")
    servers = {
        "adb": {},
        "agemu": {"agemu": {"command": c.agemu, "args": ["mcp"], "env": tool_env}},
        "mobile-mcp": {"mobile-mcp": {"command": c.mm, "args": [], "env": tool_env}},
        "agent-device": {"agent-device": {"command": c.ad, "args": ["mcp"], "env": tool_env}},
        "argent": {"argent": {"command": c.ar, "args": ["mcp"], "env": tool_env}},
    }[c.arm]
    mcp = os.path.join(ws, "mcp.json")
    json.dump({"mcpServers": servers}, open(mcp, "w"), indent=1)
    if c.arm == "argent":  # `argent init` installs this always-on rules file into a project
        rules = os.path.join(os.path.dirname(c.tools), "@swmansion", "argent", "rules", "argent.md")
        os.makedirs(os.path.join(ws, ".claude", "rules"))
        shutil.copy(rules, os.path.join(ws, ".claude", "rules", "argent.md"))
    return home, ws, cfg, mcp


def agent_cmd(c, mcp):
    cmd = ["claude", "-p", TASK.format(device=c.device_line(), app=APP_ID), "--model", c.model,
           "--output-format", "stream-json", "--verbose", "--strict-mcp-config", "--mcp-config", mcp,
           "--setting-sources", "project", "--no-session-persistence", "--max-budget-usd", "8"]
    if c.arm == "adb":
        return cmd + ["--tools", "Bash", "--allowedTools", "Bash"]
    return cmd + ["--tools", "", "--allowedTools", "mcp__" + c.arm]


def agent_env(c, cfg):
    """Only what the agent needs: the model endpoint, a clean profile, and for adb its device."""
    keep = ("USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
            "ANTHROPIC_API_KEY", "ANDROID_HOME", "ANDROID_SDK_ROOT", "JAVA_HOME")
    env = {k: os.environ[k] for k in keep if k in os.environ}
    env["HOME"] = os.path.dirname(cfg)
    env["ADB_VENDOR_KEYS"] = os.path.expanduser("~/.android")  # adb's keys stay the real ones
    env["PATH"] = os.pathsep.join([os.path.dirname(adb()), os.path.dirname(shutil.which("claude") or "/usr/local/bin"),
                                   "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"])
    env["CLAUDE_CONFIG_DIR"] = cfg
    env["DO_NOT_TRACK"] = "1"
    env["DISABLE_AUTOUPDATER"] = "1"
    if c.serial:
        env["ANDROID_SERIAL"] = c.serial
    return env


def run_agent(c):
    home, ws, cfg, mcp = workspace(c)
    shutil.copy(mcp, os.path.join(c.out, "mcp.json"))
    rows, done = [], threading.Event()
    t0 = time.time()
    p = subprocess.Popen(agent_cmd(c, mcp), cwd=ws, env=agent_env(c, cfg), stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                         stderr=open(os.path.join(c.out, "stderr.log"), "w"), text=True, start_new_session=True)

    def read():
        for line in p.stdout:
            t = time.time() - t0
            try:
                rows.append({"t": round(t, 3), "ev": json.loads(line)})
            except ValueError:
                rows.append({"t": round(t, 3), "raw": line.rstrip()[:500]})
        done.set()

    threading.Thread(target=read, daemon=True).start()
    timed_out = not done.wait(c.timeout)
    if timed_out:
        os.killpg(p.pid, signal.SIGTERM)
        time.sleep(3)
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    p.wait()
    with open(os.path.join(c.out, "stream.jsonl"), "w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    shutil.rmtree(home, ignore_errors=True)
    return rows, timed_out


def summarize(rows, timed_out=False):
    """Totals of one run from its stream. Time inside tool calls: from the assistant message that
    asks for them to the last of their results; the model's time is the rest after start-up."""
    s = {"wall_s": 0.0, "startup_s": 0.0, "tool_s": 0.0, "tool_calls": 0, "messages": 0, "tools": {}, "tool_errors": 0,
         "answer": "", "turns": 0, "cost_usd": 0.0, "api_s": 0.0, "tokens": {}, "servers": {}, "tool_count": 0, "error": ""}
    pending, asked_at, last_t, seen_msgs, calls = set(), None, 0.0, set(), []
    for r in rows:
        ev, t = r.get("ev"), r["t"]
        if not ev:
            continue
        last_t = t
        kind = ev.get("type")
        if kind == "system" and ev.get("subtype") == "init":
            s["startup_s"] = t
            s["servers"] = {m["name"]: m["status"] for m in ev.get("mcp_servers", [])}
            s["tool_count"] = len(ev.get("tools", []))
            s["claude_code"] = ev.get("claude_code_version", "")
        elif kind == "assistant":
            msg = ev["message"]
            seen_msgs.add(msg.get("id"))
            for b in msg.get("content", []):
                if b.get("type") == "tool_use":
                    name = b["name"].split("__")[-1]
                    s["tools"][name] = s["tools"].get(name, 0) + 1
                    s["tool_calls"] += 1
                    pending.add(b["id"])
                    asked_at = t
                    calls.append({"name": name, "start": t, "end": None})
        elif kind == "user":
            for b in ev["message"].get("content", []) if isinstance(ev["message"].get("content"), list) else []:
                if b.get("type") == "tool_result" and b.get("tool_use_id") in pending:
                    pending.discard(b["tool_use_id"])
                    if b.get("is_error"):
                        s["tool_errors"] += 1
                    for call in calls:
                        if call["end"] is None:
                            call["end"] = t
                            break
                    if not pending and asked_at is not None:
                        s["tool_s"] += t - asked_at
                        asked_at = None
        elif kind == "result":
            s["wall_s"] = t
            s["answer"] = (ev.get("result") or "").strip()
            s["turns"] = ev.get("num_turns", 0)
            s["cost_usd"] = ev.get("total_cost_usd", 0.0)
            s["api_s"] = ev.get("duration_api_ms", 0) / 1000
            u = ev.get("usage", {})
            s["tokens"] = {"input": u.get("input_tokens", 0), "cache_read": u.get("cache_read_input_tokens", 0),
                           "cache_write": u.get("cache_creation_input_tokens", 0), "output": u.get("output_tokens", 0)}
            if ev.get("is_error"):
                s["error"] = "the agent ended with an error: %s" % ev.get("subtype", "")
    s["messages"] = len(seen_msgs)
    if not s["wall_s"]:
        s["wall_s"] = last_t
        s["error"] = "timed out" if timed_out else "the agent ended without a result"
    s["model_s"] = round(s["wall_s"] - s["startup_s"] - s["tool_s"], 3)
    durs = sorted(c["end"] - c["start"] for c in calls if c["end"] is not None)
    s["median_call_s"] = round(durs[(len(durs) - 1) // 2], 3) if durs else 0.0
    for k in ("wall_s", "startup_s", "tool_s"):
        s[k] = round(s[k], 3)
    # iOS shows the number with the simulator's thousands separator ("31.614", "31,614").
    m = re.search(r"ORDER\D{0,12}(\d[\d.,   ]{3,7})", s["answer"])
    s["order"] = re.sub(r"\D", "", m.group(1)) if m else ""
    if not s["error"] and s["order"] != ORDER:
        s["error"] = "the answer has no order number" if not s["order"] else "wrong order number %s (want %s)" % (s["order"], ORDER)
    s["ok"] = not s["error"]
    return s


def versions(c):
    def v(cmd):
        return (sh(cmd, 20, c.env)[1].splitlines() or [""])[-1].strip()
    out = {"claude-code": v(["claude", "--version"])}
    if c.arm == "agemu":
        out["agemu"] = (sh([c.agemu, "version"], 20, c.env)[1].splitlines() or ["? ?"])[0].split()[1]
    elif c.arm == "agent-device":
        out["agent-device"] = v([c.ad, "--version"])
    elif c.arm == "argent":
        out["argent"] = v([c.ar, "--version"])
    elif c.arm == "mobile-mcp":
        pkg = os.path.join(os.path.dirname(c.tools), "@mobilenext", "mobile-mcp", "package.json")
        out["mobile-mcp"] = json.load(open(pkg))["version"]
    return out


def run(a):
    c = Ctx(a)
    os.makedirs(c.out, exist_ok=True)
    try:
        warm(c)
        reset_app(c)
    except Exception as e:  # noqa: BLE001
        s = {"ok": False, "error": "setup: %s" % e, "wall_s": 0.0}
        rows = []
    else:
        before = loadavg()
        rows, timed_out = run_agent(c)
        s = summarize(rows, timed_out)
        s["loadavg_before"], s["loadavg"] = before, loadavg()
    s.update(platform=c.platform, arm=c.arm, device=c.device, model=c.model, date=time.strftime("%Y-%m-%dT%H:%M:%S%z"),
             versions=versions(c))
    json.dump(s, open(os.path.join(c.out, "summary.json"), "w"), indent=1)
    print("%s %s: %s  %.1f s (tools %.1f, model %.1f), %d turns, %d tool calls, $%.2f  %s" % (
        c.platform, c.arm, "ok" if s["ok"] else "FAILED", s["wall_s"], s.get("tool_s", 0), s.get("model_s", 0), s.get("turns", 0),
        s.get("tool_calls", 0), s.get("cost_usd", 0), s.get("error", "")), flush=True)
    return s


def busy(s, limit):
    """Whether the host's 1-minute load average after a run was at or over the limit."""
    try:
        return float(s.get("loadavg", "0").split()[0]) >= limit
    except (ValueError, IndexError):
        return False


def series(a):
    """ROUNDS runs per arm, the arms' order rotated every round. While a rival runs, the agemu
    device is parked; while agemu runs on Android, the standard emulator is paused. A run
    waits for a quiet host before it starts, and one the host was busy during is made again."""
    arms = a.arms.split(",") if a.arms else ARMS[a.platform]
    agemu_env = dict(os.environ, **({"AGEMU_HOME": a.agemu_home} if a.agemu_home else {}))
    agemu_id = ("ios:" if a.platform == "ios" else "android:") + a.agemu
    for i in range(a.first, a.rounds + 1):
        for k in range(len(arms)):
            arm = arms[(k + i - 1) % len(arms)]
            out = os.path.join(a.out, "runs", "%s-%d" % (arm, i))
            if os.path.exists(os.path.join(out, "summary.json")):
                continue
            ra = argparse.Namespace(**vars(a))
            ra.arm, ra.out = arm, out
            ra.device = a.agemu if arm == "agemu" else a.std
            c = Ctx(ra) if (a.platform == "ios" or arm != "agemu") else None
            if arm == "agemu":
                if a.platform == "android":
                    sh([adb(), "-s", serial_of(a.std), "emu", "avd", "stop"], 20)
                sh([a.agemu_bin, "resume", "-d", agemu_id], 120, agemu_env)
            else:
                sh([a.agemu_bin, "park", "-d", agemu_id], 120, agemu_env)
                if a.platform == "android":
                    sh([adb(), "-s", serial_of(a.std), "emu", "avd", "start"], 20)
                release(c)
            time.sleep(15)
            quiet(a.quiet_wait, a.quiet_load)
            s = run(ra)
            if busy(s, a.busy_load):
                # Someone else's work loaded the host during the run: it is kept as
                # <run>.busy, out of the summary, and the run is made once more.
                print("  host load %s after the run: kept as %s.busy, running it again" % (s["loadavg"], os.path.basename(out)), flush=True)
                shutil.rmtree(out + ".busy", ignore_errors=True)
                os.rename(out, out + ".busy")
                if arm != "agemu":
                    release(Ctx(ra))
                time.sleep(15)
                quiet(a.quiet_wait, a.quiet_load)
                run(ra)
            if arm != "agemu":
                release(Ctx(ra))
    if a.platform == "android":
        sh([adb(), "-s", serial_of(a.std), "emu", "avd", "start"], 20)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("run", "series"):
        p = sub.add_parser(name)
        p.add_argument("--platform", choices=["android", "ios"], required=True)
        p.add_argument("--out", required=True)
        p.add_argument("--model", default="claude-sonnet-5-5")
        p.add_argument("--timeout", type=int, default=900, help="seconds one agent run may take")
        p.add_argument("--agemu-bin", default=os.path.join(ROOT, "bin", "agemu"))
        p.add_argument("--agemu-home", default=os.environ.get("AGEMU_HOME", ""), help="the state directory of agemu's daemon")
        p.add_argument("--tools", default=os.environ.get("TOOLS", os.path.expanduser("~/.cache/agemu-race-tools/node_modules/.bin")))
    r = sub.choices["run"]
    r.add_argument("--arm", required=True)
    r.add_argument("--device", required=True, help="Android: the running emulator's AVD name; iOS: the simulator's UDID")
    s = sub.choices["series"]
    s.add_argument("--std", required=True, help="the standard device, for the rivals")
    s.add_argument("--agemu", required=True, help="agemu's device")
    s.add_argument("--rounds", type=int, default=5)
    s.add_argument("--first", type=int, default=1, help="to continue a series; finished runs are skipped")
    s.add_argument("--arms", default="", help="comma-separated; default: every arm of the platform")
    s.add_argument("--quiet-wait", type=int, default=900, help="seconds a run may wait for a quiet host")
    s.add_argument("--quiet-load", type=float, default=5.0, help="quiet is a 1-minute load average below this")
    s.add_argument("--busy-load", type=float, default=15.0,
                   help="a run that ends with the 1-minute load average at or over this is kept as <run>.busy and made again, once")
    a = ap.parse_args()
    if a.cmd == "run":
        if a.arm not in ARMS[a.platform]:
            ap.error("no arm %s on %s" % (a.arm, a.platform))
        sys.exit(0 if run(a)["ok"] else 1)
    series(a)


if __name__ == "__main__":
    main()
