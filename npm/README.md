# agentic-emulator (`agemu`)

An agent-first runtime for the Android emulator (macOS, Linux, Windows) and the iOS Simulator
(Apple Silicon Macs). LLM
agents drive headless devices through a CLI or an MCP server: screenshots, the UI tree, taps,
typing, apps, test capabilities, logs and crashes, in milliseconds instead of the hundreds of
milliseconds to seconds `adb`, `simctl` and Maestro take.

```sh
claude mcp add agemu -- npx -y agentic-emulator@latest mcp    # MCP server for Claude Code, no install
npm install -g agentic-emulator                               # or the `agemu` CLI on PATH
agemu doctor                                                  # checks Xcode and the Android SDK
```

iOS needs an Apple Silicon Mac with Xcode and an iOS Simulator runtime. Android needs the
Android SDK with the emulator, on an Apple Silicon Mac, on Linux (x86-64, arm64) with KVM or on
Windows (x86-64) with WHPX. `agemu doctor` says what is missing and prints the commands that
install it.

The package holds native binaries for each of those platforms (`agemu`, the `agemud` daemon it
starts, and on macOS the iOS helper) and a small Node launcher that runs the ones for this
computer. There is no install script.

Documentation, measurements and source:
[github.com/piotr-graczyk-dev/agentic-emulator](https://github.com/piotr-graczyk-dev/agentic-emulator).
MIT License.
