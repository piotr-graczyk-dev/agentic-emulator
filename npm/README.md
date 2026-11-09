# agentic-emulator (`agemu`)

An agent-first runtime for the Android emulator and the iOS Simulator on Apple Silicon Macs. LLM
agents drive headless devices through a CLI or an MCP server: screenshots, the UI tree, taps,
typing, apps, test capabilities, logs and crashes, in milliseconds instead of the hundreds of
milliseconds to seconds `adb`, `simctl` and Maestro take.

```sh
claude mcp add agemu -- npx -y agentic-emulator@latest mcp    # MCP server for Claude Code, no install
npm install -g agentic-emulator                               # or the `agemu` CLI on PATH
agemu doctor                                                  # checks Xcode and the Android SDK
```

It needs an Apple Silicon Mac, and Xcode with an iOS Simulator runtime or the Android SDK with
the emulator. `agemu doctor` says what is missing and prints the commands that install it.

The package holds three native binaries (`agemu`, the `agemud` daemon it starts, and the iOS
helper); there is no Node code and no install script.

Documentation, measurements and source:
[github.com/piotr-graczyk-dev/agentic-emulator](https://github.com/piotr-graczyk-dev/agentic-emulator).
MIT License.
