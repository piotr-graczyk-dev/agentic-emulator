# Architecture

agemu is an agent-first runtime for Android emulators and iOS Simulators on
Apple Silicon macOS. It has no UI for humans. Agents drive devices through a
CLI or an MCP server.

## What is new and what is reused

The guest operating systems are the stock ones, because only the real OS
gives real behaviour:

- **Android:** the AOSP arm64 system image from the Android SDK. It runs in
  the SDK's QEMU with Apple Hypervisor.framework (HVF) acceleration, so app
  code runs at near-native CPU speed.
- **iOS:** the iOS Simulator runtime from Xcode, through CoreSimulator. App
  code runs natively on the host CPU.

Everything an agent touches is written for this project. That layer is where
standard tooling is slow:

| Agent operation | Standard tool | Why it is slow | agemu |
|---|---|---|---|
| boot / reset | cold boot, `simctl erase` | full OS boot | warm snapshots and pre-booted state |
| screenshot | `adb exec-out screencap -p`, `simctl io screenshot` | new process per call, PNG encoded in the guest | read the host-side framebuffer directly from a long-lived connection |
| tap / swipe / type | `adb shell input` (starts a new ART runtime per call) | about 200 ms per event | inject events into the virtual touchscreen directly |
| UI tree | `uiautomator dump` (about 1-3 s) | new process, XML written to the device's disk | long-lived on-device or in-process accessibility server, JSON over a socket |

## Components

```
agents ──► agemu CLI ─┐            (one short-lived process per command)
agents ──► agemu mcp ─┤  JSON lines over unix socket ~/.agemu/agemu.sock
                      ▼
               agemud daemon (Go) ── device registry, selectors, waits, per-device locks
               ├── android backend (internal/android)
               │     ├── emulator gRPC (screenshots, touch, keys, snapshots)
               │     ├── on-device agent (android-agent/, Java via app_process): UI tree, launch, ...
               │     └── log capture: one `logcat -B` reader per device (adb exec:), crash records
               └── ios backend (internal/ios)
                     ├── agemu-ios-helper (ios-helper/, Swift, long-lived child process)
                     │     CoreSimulator / SimulatorKit private APIs: framebuffer IOSurface,
                     │     HID touch and keyboard, accessibility tree
                     └── log capture: one `simctl spawn log stream` per simulator, kqueue on
                           launched apps, the Mac's .ips crash reports
```

`logs`, `crashes` and the crash notice run without the device lock: they read what the capture
holds in memory (docs/CAPABILITIES.md, "Logs and crashes").

A device can be parked (`backend.Parker`: iOS SIGSTOPs the simulator's processes, Android
pauses the VM in place). The daemon resumes a parked device under its device lock before any
other call on it, `logs` and `crashes` excepted, so backends never see a call for a stopped
device.

## Conventions

- Device ids carry a platform prefix: `android:<avd-name>` or `ios:<udid>`.
- **Coordinates are screenshot pixels** of the current profile, on both
  platforms. An agent can read a point off a scale=1 screenshot and tap it.
  UI-tree frames use the same pixel space.
- Every daemon response includes `ms`, the server-side handling time, which
  benchmarks use.
- Profiles (`internal/profiles`) describe real device screens:
  - Android profiles are applied at runtime as a display size and density
    override, so any profile applies to any Android device.
  - iOS screens are fixed per simulator device type, so an iOS profile maps
    to a device type.
