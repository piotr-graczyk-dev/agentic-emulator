#!/usr/bin/env bash
# Regenerates the Go gRPC client for the Android emulator from the protos that
# ship with the SDK emulator (copied into ./proto so the build needs no SDK).
# Needs: protoc (brew install protobuf), protoc-gen-go, protoc-gen-go-grpc.
set -euo pipefail
cd "$(dirname "$0")"
SDK_LIB="${ANDROID_HOME:-$HOME/Library/Android/sdk}/emulator/lib"
if [[ "${1:-}" == "--refresh" ]]; then
  cp "$SDK_LIB"/{emulator_controller,snapshot_service,snapshot}.proto proto/
fi
MOD=github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb
M="Memulator_controller.proto=$MOD,Msnapshot_service.proto=$MOD,Msnapshot.proto=$MOD/snapshotpb"
export PATH="$PATH:$(go env GOPATH)/bin"
protoc -I proto \
  --go_out=. --go_opt=paths=source_relative --go_opt="$M" \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative --go-grpc_opt="$M" \
  emulator_controller.proto snapshot_service.proto
protoc -I proto --go_out=snapshotpb --go_opt=paths=source_relative --go_opt="$M" snapshot.proto
