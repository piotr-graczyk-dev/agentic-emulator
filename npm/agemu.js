#!/usr/bin/env node
// Runs the agemu binary built for this computer. The package holds one set of native binaries
// per supported platform under platforms/<os>-<cpu>/; agemu starts the agemud daemon that sits
// next to it. There is no install script: this launcher picks the binary at each run.
"use strict";

const { spawn } = require("child_process");
const fs = require("fs");
const path = require("path");

const key = `${process.platform}-${process.arch}`;
const supported = {
  "darwin-arm64": "macOS on Apple Silicon (iOS and Android)",
  "linux-x64": "Linux on x86-64 (Android)",
  "linux-arm64": "Linux on arm64 (Android)",
  "win32-x64": "Windows on x86-64 (Android)",
};
const exe = path.join(__dirname, "..", "platforms", key, process.platform === "win32" ? "agemu.exe" : "agemu");

if (!supported[key] || !fs.existsSync(exe)) {
  const list = Object.values(supported).map((s) => `  - ${s}`).join("\n");
  process.stderr.write(`agemu: no build for ${key} in this package. agemu runs on:\n${list}\n`);
  process.exit(1);
}

const child = spawn(exe, process.argv.slice(2), { stdio: "inherit", windowsHide: true });

// Pass on the signals a terminal or an MCP client sends, so agemu shuts down cleanly.
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => {
    try {
      child.kill(sig);
    } catch (_) {
      // already gone
    }
  });
}

child.on("error", (err) => {
  process.stderr.write(`agemu: could not run ${exe}: ${err.message}\n`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) {
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code === null ? 1 : code);
});
