#!/usr/bin/env bash
# Writes THIRD_PARTY_NOTICES.md: the licenses of the code agemu's binaries contain besides its
# own. Go modules come from `go list` (only those linked into a command), the rest is listed by
# hand below. Run it after changing go.mod: scripts/third-party-notices.sh > THIRD_PARTY_NOTICES.md
set -euo pipefail
cd "$(dirname "$0")/.."

license_file() {
	for f in LICENSE LICENSE.md LICENSE.txt COPYING; do
		[ -f "$1/$f" ] && { echo "$1/$f"; return; }
	done
}

cat <<'HEAD'
# Third-party notices

agemu is MIT-licensed ([LICENSE](LICENSE)). Its binaries also contain the code below, under the
licenses that follow.

## Android emulator gRPC protocol

`internal/android/emupb/proto/*.proto` and the Go code generated from them
(`internal/android/emupb`) come from the Android Open Source Project's emulator, Copyright (C)
2018 The Android Open Source Project, licensed under the Apache License, Version 2.0 (its text is
under google.golang.org/grpc below). The files keep their original headers.

## facebook/idb

The iOS helper's private-API byte layouts (Indigo HID messages) follow facebook/idb's
FBSimulatorControl, MIT License, Copyright (c) Meta Platforms, Inc. and affiliates. The places are
marked in `ios-helper/Sources/AgemuSim`.

## Go modules
HEAD

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/... |
	sort -u | while read -r path version dir; do
		lf=$(license_file "$dir")
		printf '\n### %s %s\n\n' "$path" "$version"
		if [ -n "$lf" ]; then
			printf '```text\n'
			cat "$lf"
			printf '```\n'
		else
			printf 'No license file found in the module.\n'
		fi
		for nf in "$dir"/NOTICE "$dir"/NOTICE.txt "$dir"/NOTICE.md; do
			[ -f "$nf" ] || continue
			printf '\nNOTICE:\n\n```text\n'
			cat "$nf"
			printf '```\n'
		done
	done
