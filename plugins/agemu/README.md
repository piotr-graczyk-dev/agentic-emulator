# agemu for Claude Code

A Claude Code plugin for [agemu](https://github.com/piotr-graczyk-dev/agentic-emulator), the fast
control layer for the Android emulator and the iOS Simulator.

```sh
claude plugin marketplace add piotr-graczyk-dev/agentic-emulator
claude plugin install agemu@agemu
npx -y agentic-emulator doctor      # checks this computer, prints fixes
```

Inside a session the same two steps are `/plugin marketplace add piotr-graczyk-dev/agentic-emulator`
and `/plugin install agemu@agemu`.

It adds:

- **The agemu MCP server** (`npx -y agentic-emulator@latest mcp`), 31 tools: devices, screenshots,
  the UI tree, taps and typing that wait for their effect, logs, crash reports and test setup
  (permissions, location, deep links, dark mode, push).
- **The `check-on-device` skill**: how to build an app, run it on a device and check a change in
  few turns. Claude loads it when a change should be seen working on a simulator or emulator.

If you already added the MCP server by hand (`claude mcp add agemu ...`), remove that entry so
the tools are not listed twice: `claude mcp remove agemu`.

What agemu needs (an Apple Silicon Mac for iOS; the Android SDK for Android, also on Linux and
Windows) is in [docs/INSTALL.md](../../docs/INSTALL.md).
