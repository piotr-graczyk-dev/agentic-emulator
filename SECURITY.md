# Security

## Reporting a vulnerability

Please report vulnerabilities privately, not in a public issue: use
[Report a vulnerability](https://github.com/piotr-graczyk-dev/agentic-emulator/security/advisories/new)
on GitHub. Include what an attacker can do, the steps to reproduce, and the output of
`agemu version`. You should get an answer within a week.

## What agemu trusts

agemu is a local developer tool. Knowing its boundaries helps judge a report:

- **The daemon socket.** `agemud` serves every request on the unix socket
  `$AGEMU_HOME/agemu.sock` (default `~/.agemu/agemu.sock`) without authentication. Anyone who can
  connect to it can drive your devices, install apps on them and read their screens, logs and
  clipboards. It is meant to be reachable by your own user only.
- **Agents.** An agent connected over MCP or the CLI can do everything above, by design.
- **Devices.** agemu only manages devices whose name starts with `agemu-` unless
  `AGEMU_ANDROID_AVDS` says otherwise, so it does not touch emulators and simulators other tools
  use.
- **The iOS helper** uses Apple's private CoreSimulator frameworks inside your user session. It
  needs no extra privileges and asks for none.

A way to reach the socket, the devices or the host from outside your user account, or from an
app running inside a device, is in scope.
