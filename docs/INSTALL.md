# Install

The full setup: what each install method does, what a Mac needs, other MCP clients, updates and
troubleshooting. The short version is in the [README](../README.md#install).

```sh
claude mcp add agemu -- npx -y agentic-emulator@latest mcp
```

That is the whole install for Claude Code. `npx` fetches the
[`agentic-emulator`](https://www.npmjs.com/package/agentic-emulator) package on first use: three
native binaries, 13 MB, no Node code and no install script. With `@latest` each new session
runs the newest release, and a session that starts a newer release restarts the shared daemon on it (devices keep
running). Then check this Mac:

```sh
npx -y agentic-emulator doctor
```

`agemu doctor` checks everything below per platform and prints the commands that fix what is
missing. One platform is enough: an iOS-only or Android-only Mac is fine.

```text
$ agemu doctor
agemu
  ✓ Apple Silicon Mac      macOS 26.6 (arm64)
  ✓ agemud                 ~/.local/bin/agemud
  ✓ daemon                 not running (the first command starts it)
  ✓ on PATH                ~/.local/bin/agemu

iOS  ready
  ✓ Xcode                  Xcode 26.6 (/Applications/Xcode.app/Contents/Developer)
  ✓ iOS Simulator runtime  iOS 26.5
  ✓ agemu-ios-helper       ~/.local/bin/agemu-ios-helper
  ✓ private frameworks     CoreSimulator 1051.55, SimulatorKit 955.7 (/Applications/Xcode.app/Contents/Developer/Library/PrivateFrameworks/SimulatorKit.framework)

Android  not ready
  ✓ Android SDK            ~/Library/Android/sdk
  ✓ Java                   /Applications/Android Studio.app/Contents/jbr/Contents/Home
  ✓ command-line tools     ~/Library/Android/sdk/cmdline-tools/latest/bin
  ✓ emulator               ~/Library/Android/sdk/emulator
  ✓ platform-tools (adb)   ~/Library/Android/sdk/platform-tools
  ✗ system image           not installed: system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a

To fix
  1. yes | ~/Library/Android/sdk/cmdline-tools/latest/bin/sdkmanager --licenses > /dev/null
     ~/Library/Android/sdk/cmdline-tools/latest/bin/sdkmanager --install 'system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a'    # the image takes about 8 GB of disk
  Then run `agemu doctor` again.
...
```

## The `agemu` command

Agents that use a shell, and scripts, want `agemu` on `PATH`:

```sh
npm install -g agentic-emulator
# or without Node, into ~/.local/bin (checks the release's SHA256SUMS):
curl -fsSL https://raw.githubusercontent.com/piotr-graczyk-dev/agentic-emulator/main/install.sh | sh
```

If `install.sh` says `~/.local/bin` is not on `PATH`, run the line it prints first. Then the
first device:

```sh
agemu create --platform ios --name iphone && agemu boot agemu-iphone
agemu screenshot -o shot.png
```

Every command prints JSON, and `agemu help` lists them. Through `npx`, a command is
`npx -y agentic-emulator <command>`, which adds Node's start-up to each call; an agent that runs
one command per action should use the installed `agemu`.

## What it needs

| | Needed for | Get it |
|---|---|---|
| Apple Silicon Mac | everything | |
| Xcode with an iOS Simulator runtime | iOS | the App Store, open it once, then `xcodebuild -downloadPlatform iOS` (iOS 26.5: 16 GB on disk) |
| Android SDK: emulator, platform-tools, command-line tools | Android | Android Studio, or `brew install --cask android-commandlinetools` |
| the arm64 system image `system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a` | Android | `sdkmanager --install '<image>'` (8.2 GB on disk); another image: `AGEMU_ANDROID_IMAGE` |
| Java 17+ | creating Android devices (the SDK's `avdmanager`) | Android Studio's bundled runtime is found by itself, or `brew install --cask temurin` |
| Node.js | only for the `npx` and `npm` installs | `brew install node` |

No environment variables are needed for the usual layouts. The Android SDK is found at
`$ANDROID_HOME`, `$ANDROID_SDK_ROOT`, `~/Library/Android/sdk` (Android Studio) or
`/opt/homebrew/share/android-commandlinetools` (Homebrew), in that order; Java at `$JAVA_HOME`,
the JDK `/usr/libexec/java_home` picks, or Android Studio's. A Mac without Android Studio, from
nothing:

```sh
brew install --cask android-commandlinetools temurin
yes | sdkmanager --licenses > /dev/null
sdkmanager --install emulator platform-tools 'system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a'
```

## Connect an agent

`agemu mcp` is an MCP server on stdio.

| Client | |
|---|---|
| Claude Code | `claude mcp add agemu -- npx -y agentic-emulator@latest mcp` (`--scope user` for every project) |
| Codex CLI | `codex mcp add agemu -- npx -y agentic-emulator@latest mcp` |
| Cursor, Claude Desktop, other JSON configs | `{"mcpServers": {"agemu": {"command": "npx", "args": ["-y", "agentic-emulator@latest", "mcp"]}}}` |

Apps started from the Dock do not see your shell's `PATH`. If one cannot find `npx`, put the path
`which npx` prints in `command`, or install `agemu` and use the absolute path `agemu doctor`
prints.

## Update and uninstall

| Installed with | Update | Uninstall |
|---|---|---|
| `npx` | nothing to do: each session runs the latest release | `claude mcp remove agemu` |
| `npm install -g` | `npm install -g agentic-emulator@latest`, then `agemu daemon restart` | `agemu daemon stop && npm uninstall -g agentic-emulator` |
| `install.sh` | run it again | `agemu daemon stop && rm ~/.local/bin/{agemu,agemud,agemu-ios-helper}` |
| source | `git pull && make install` | `make uninstall` |

`install.sh` and `make install` restart a running daemon on the new build; after an npm update,
the next MCP session does it, or `agemu daemon restart` at once. MCP sessions reconnect by
themselves and devices keep running. The automatic restarts (install.sh, `make install`, an MCP
session) wait while the daemon prepares a new Android device's baseline, since a restart cancels
the preparation; `agemu daemon restart` does not wait (it warns), so run it once `agemu daemon
status` lists no preparing device. Uninstalling leaves the devices you created: `agemu-*` AVDs
(`avdmanager delete avd -n agemu-pixel`) and `agemu-*` simulators (`xcrun simctl delete <udid>`),
and the state in `~/.agemu`.

## From source

Building needs Go (the version in `go.mod`) and Swift (Xcode or `xcode-select --install`):

```sh
git clone https://github.com/piotr-graczyk-dev/agentic-emulator.git
cd agentic-emulator
make install     # build, copy to ~/.local/bin (PREFIX=/usr/local for another place), run agemu doctor
```

`agemu` is a thin client: it starts the `agemud` next to it on first use, and all CLI processes
and MCP sessions share that one daemon. State lives in `$AGEMU_HOME` (default `~/.agemu`). The
on-device Android agent's jar is committed; rebuilding it needs a JDK and the Android SDK. More in
[CONTRIBUTING.md](../CONTRIBUTING.md).

## When something does not work

- **Run `agemu doctor`.** It does not start the daemon and took 0.6 s here.
- **A change or an upgrade has no effect.** The daemon keeps running the binary it started from.
  An MCP session restarts a daemon older than itself when it starts; otherwise `agemu version` and
  `agemu doctor` say when it is out of date, and `agemu daemon restart` fixes it without touching
  running devices.
- **A setting is ignored.** The daemon reads `AGEMU_*`, `ANDROID_HOME` and `JAVA_HOME` from the
  process that started it, which may be an MCP client that does not have your shell's
  environment. Set them in the MCP config's `env`, or run `agemu daemon restart` from your shell.
- **`agemu create --platform android` fails.** The error carries `avdmanager`'s last lines; a
  missing image or Java shows up in `agemu doctor` first.
- **The daemon log:** `~/.agemu/daemon.log`; `agemu daemon status` prints its path and the pid.
- **Still stuck:** [open an issue](https://github.com/piotr-graczyk-dev/agentic-emulator/issues/new/choose)
  with `agemu doctor --json`.
