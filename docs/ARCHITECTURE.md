# Architecture

agemu is an agent-first runtime for Android emulators and iOS Simulators on
Apple Silicon macOS, and for Android emulators on Linux and Windows. It has no
UI for humans. Agents drive devices through a CLI or an MCP server.

## What is new and what is reused

The guest operating systems are the stock ones, because only the real OS
gives real behaviour:

- **Android:** the AOSP system image from the Android SDK, built for the host's
  CPU (arm64 on Apple Silicon and arm64 Linux, x86_64 on x86-64 Linux and
  Windows). It runs in the SDK's QEMU with the host's hypervisor
  (Hypervisor.framework on macOS, KVM on Linux, WHPX or AEHD on Windows), so
  app code runs at near-native CPU speed.
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
agents ──► agemu mcp ─┤  JSON lines over unix socket ~/.agemu/agemu.sock (also on Windows 10+)
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

The iOS backend is compiled into darwin builds only. What differs between the
host OSes in the daemon, the CLI and the Android backend is in
`internal/osproc` (detached processes, process liveness and tree kill, the
daemon's lock, the frame mapping) and `internal/toolchain` (SDK, Java and tool
paths, the default system image). On Windows, which has no SIGTERM for a
detached process, `agemu daemon stop` asks the daemon to exit over its socket
(the `exit` method).

`logs`, `crashes` and the crash notice run without the device lock: they read what the capture
holds in memory (docs/CAPABILITIES.md, "Logs and crashes").

A device can be parked (`backend.Parker`: iOS SIGSTOPs the simulator's processes, Android
pauses the VM in place). The daemon resumes a parked device under its device lock before any
other call on it, `logs` and `crashes` excepted, so backends never see a call for a stopped
device. With `AGEMU_AUTO_PARK` set, the daemon also counts each call on a device from
its start to its end (`internal/daemon/autopark.go`) and parks a device it has served after that
long without one; it marks the device first, so a call that starts during the park waits for it
and then resumes the device as above. Device ids are made canonical first
(`backend.IDNormalizer`: an iOS UDID upper case, an AVD name as its file is spelled), so every
per-device key in the daemon has one spelling per device.

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
