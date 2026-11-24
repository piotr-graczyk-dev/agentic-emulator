# Agent loop: a real agent does one task with each tool

The [agent race](../agent-race) times the tools with no model: the next call starts when the
previous one returns. This example adds the model. Claude Code, run headless (`claude -p`, Sonnet
5.5), gets one task in the [Agemu Shop](../shop) app in plain words and one device tool at a time:
agemu, mobile-mcp, agent-device or Argent. It measures what a developer waits for: the whole run,
the time inside tool calls, the model's time, turns, tokens and cost.

The task, the same words for every tool:

> Sign in with the e-mail test@agemu.dev and the password hunter2. Add one Bath Towel to the
> cart. Add two Desk Lamps to the cart. Open the cart and check that the total is $91.98. Check
> out with this shipping address: Ada Lovelace, 12 Analytical St, London, N1 9GU. Place the
> order. Finish with one line: ORDER and the order number.

A run counts as done when the agent's last message has the right order number. The app derives
it from the total, the number of items and the name on the order, so a wrong cart or a mistyped
name gives another number.

## Results

iOS 26.5, 2026-11-24, one Apple M2 Pro Mac (16 GB) that other sessions used at the same time.
Five runs per tool, the tools' order rotated every round; before a run the script waited until
the host's 1-minute load average was under 5. Every run is in `out/ios/<series>/results.md`, and
each run's totals and tool calls are in `out/ios/<series>/runs/`.

### agemu v0.2.2, as released

| | mobile-mcp 1.0.17 | agent-device 0.21.37 | Argent 0.33.0 | agemu v0.2.2 |
|---|---|---|---|---|
| task done | 5 of 5 | 5 of 5 | 3 of 5 | 5 of 5 |
| whole run, median | 107 s | **72 s** | 157 s | 109 s |
| whole run, range | 68-138 s | 67-78 s | 153-160 s | 90-121 s |
| of it in tool calls, median | 36.0 s | 29.5 s | 47.8 s | **20.1 s** |
| of it the model, median | 70 s | 42 s | 109 s | 77 s |
| tool calls | 19-67 | 21-26 | 28-38 | 59-85 |
| input tokens per run, median | 611,000 | 1,252,000 | 2,130,000 | 565,000 |
| cost per run at list price, median | $0.21 | $0.31 | $0.62 | $0.19 |

agemu's calls were the fastest, and its runs were not: the agent made three times as many calls
as with agent-device, and every call is a model turn. Two things caused most of them:

- **A field's text could not be replaced.** `type_text` inserts at the cursor. To search for the
  second product the agent deleted the first one key press per call: 21 `press_key` calls per run
  on average.
- **The screen came back too early.** `returnUi` returned the screen as it was right after the
  action, often in the middle of a transition, and the agent read it again: 9 `ui_tree` calls
  per run.

### agemu with `type --into`, `type --clear` and a settled screen (`608ef7d`)

Those two were fixed ([#18](https://github.com/piotr-graczyk-dev/agentic-emulator/pull/18)), and
the whole series ran again with agemu built from that pull request's first commit.

| | mobile-mcp 1.0.17 | agent-device 0.21.37 | Argent 0.33.0 | agemu `608ef7d` |
|---|---|---|---|---|
| task done | 5 of 5 | 5 of 5 | 4 of 5 | 5 of 5 |
| whole run, median | 113 s | 67 s | 161 s | **54 s** |
| whole run, range | 74-120 s | 65-72 s | 158-168 s | 52-103 s |
| of it in tool calls, median | 38.6 s | 28.6 s | 45.8 s | **21.1 s** |
| of it the model, median | 66 s | 38 s | 113 s | 34 s |
| tool calls | 18-59 | 21-26 | 31-39 | 22-35 |
| input tokens per run, median | 561,000 | 1,121,000 | 2,466,000 | 261,000 |
| cost per run at list price, median | $0.18 | $0.30 | $0.57 | $0.09 |

What the numbers show:

- **The model's time is most of every run**: 34 of 54 s with agemu, 38 of 67 s with agent-device,
  113 of 161 s with Argent. A tool is fast for an agent when it needs few calls and sends little
  back, and only then when its calls are fast.
- **Fewer calls did more than faster calls.** agemu's time in tool calls stayed at 20-21 s. Its
  whole run went from 109 s to 54 s because the calls went from 59-85 to 22-35. A call now
  takes longer (a median of 365-637 ms, from 35-173 ms), because it waits for the screen to stop
  changing instead of leaving that to a second call.
- **Tokens follow what a tool sends back.** agemu offers 31 tools and answers with a compact
  outline. agent-device offers 62 tools, Argent 71, and Argent returns a screenshot and an element
  list after every action. An agemu run read 261,000 input tokens, an agent-device run 1.1
  million, an Argent run 2.5 million.
- **Argent did not finish 3 of its 10 runs.** In each, the text its `keyboard` tool typed
  arrived without capital letters ("ada lovelace"), so the app gave another order number. All
  three agents said so in their report, and two of them that the "@" of the e-mail had first
  come out as "2".
- **A wrong guess costs 10 s.** In every agemu run after the fix the agent lost 10 s after
  "Sign in": it waited for a product name it had guessed ("Towel"), which is not on the catalog's
  first screen, and the wait ran to its timeout. That is a fifth of the run, and it is open (see
  the checks below).
- **agemu's two slow runs.** One (81 s, 35 calls) hit a flaw in the new `into`: the agent wrote
  `into: {"label": "Search products"}`, a selector has no `label` field, and the call typed
  without tapping the field and answered ok. That is fixed since (an unknown field is refused,
  and `label` is another name for `text`). The other (103 s, 22 calls) ran while another
  session's work took the host's load average to 98; its tool calls took 66 s. Both are counted.

The pull request's later commits were checked with agemu alone, five runs each, after the
series. They were not run against the other tools.

| agemu at | what changed | whole run, median (range) | tool calls | done |
|---|---|---|---|---|
| `9e14da7` | review fixes; an unknown selector field is refused, `label` is another name for `text` | 51 s (43-57) | 22-27 | 5 of 5 |
| `d7ddc99` | the MCP instructions say not to guess what to wait for | 56 s (52-68) | 23-25 | 5 of 5 |

The second change did not do what it was for. The agent stopped asking the tap to wait for a
guessed text. The tap then returned the app's "Signing in…" state, which had stopped changing
and so counted as the screen the action ends on, and the agent guessed again in a separate
`wait_for` call: the 10 s timeout was still in all five runs. A loading state should not count
as a settled screen; how to tell one is not decided.

### What these results do not show

- **agemu was changed after its first series on this very task.** That is what the measurement
  was for, but it means agemu's tools now fit this task, and the other tools were used as they
  ship. Their makers could do the same. A second task that no tool was changed against is the
  next step.
- **One task, one app, one model, one platform, five runs.** Runs of the same tool differ a lot
  (mobile-mcp: 74-120 s). Android was not measured: the host had no disk space for a second
  emulator.
- **The released agemu is v0.2.2**, the first table. The second table is a pull request.
- **Each tool got its MCP server and nothing else**, apart from Argent's always-on rules file,
  which `argent init` installs. agent-device also ships skills for agents; they were not
  installed, and neither was agemu's plugin.
- **Cost depends on the prompt cache.** A run that starts while the previous run's prompt is
  still cached costs less (agent-device: $0.16 once, $0.29-0.32 otherwise).

## How it was run

- **The agent**: `claude -p` with `--model claude-sonnet-5-5`, Claude Code 2.1.329, at most 900 s
  per run. Its only tools are the arm's MCP server (`--strict-mcp-config`, `--tools ""`,
  `--allowedTools mcp__<arm>`): no shell and no file tools. On Android the `adb` arm gets a shell
  instead.
- **A clean profile.** The agent runs with an empty home directory and an empty project, so no
  user memory, skill, hook or other server reaches it. This matters: with the host user's own
  `CLAUDE.md`, which named a tool to use, the agent refused to work with another one. The tools'
  own processes keep the real home directory, where their daemons live.
- **Devices**: mobile-mcp, agent-device and Argent on a standard iPhone 17 Pro Simulator, agemu on
  a lean iPhone 17 Pro it created, as in the agent race. While another tool ran, agemu's simulator
  was parked; a standard Simulator cannot be paused and stayed idle during agemu's runs.
- **Untimed setup per run**, as in the race: the tool's daemon warmed up, the app launched once
  and stopped, the home screen in front. Between tools the other tools' daemons and on-device
  helpers were stopped.
- **The times** come from the agent's output stream, stamped as each event arrives. Time in tool
  calls runs from the message that asks for calls to the last of their results; the model's time
  is the rest after start-up. Tokens and cost are what Claude Code reports (`usage`,
  `total_cost_usd`); input tokens include cache reads.
- **A busy host.** `series` waits for a quiet host before a run. Since the run at load 98 it also
  makes a run again, once, when the 1-minute load average is 15 or more after it; the first
  attempt is kept as `<run>.busy`, listed in `results.md` and left out of the summary. The two
  series above ran before that rule and count every run.
- agemu v0.2.2 was built from `main` at `31b5481`, which has the same code as the v0.2.2 tag
  (the commits after the tag change the README and the race only).

## Run it

```sh
make build
npm install --prefix ~/.cache/agemu-race-tools @swmansion/argent@0.33.0 agent-device@0.21.37 \
  @mobilenext/mobile-mcp@1.0.17
app=$(examples/shop/ios/build.sh)

xcrun simctl create bench-std com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro \
  com.apple.CoreSimulator.SimRuntime.iOS-26-5        # prints the standard simulator's UDID
xcrun simctl boot <std-udid> && xcrun simctl install <std-udid> "$app"
agemu create --platform ios --name bench && agemu boot agemu-bench   # agemu's; `agemu devices` has its UDID
agemu install "$app" -d agemu-bench

examples/agent-loop/loop.py series --platform ios --std <std-udid> --agemu <agemu-udid> \
  --rounds 5 --out /tmp/agent-loop/ios
examples/agent-loop/stats.py /tmp/agent-loop/ios                  # results.md and summary.md
examples/agent-loop/calls.py /tmp/agent-loop/ios/runs/agemu-1     # one run, call by call
```

One run on its own: `loop.py run --platform ios --arm argent --device <udid> --out /tmp/agent-loop/try`.
The scripts need Python 3.9 or newer and the `claude` CLI, signed in. A series of 20 runs took
about 45 minutes and $6 at list prices.

`loop.py` keeps each run's whole stream (`stream.jsonl`: every message, tool call and result).
The committed runs have the totals (`summary.json`) and the calls (`calls.jsonl`, from
`calls.py --jsonl`); the streams are not committed.
