# iOS backend: builds agemu-ios-helper (Swift, ios-helper/) and puts it next to agemu. macOS only:
# elsewhere `make build` skips it (the iOS backend is compiled out off macOS).
# The Go backend looks for the helper beside the agemu binary (or $AGEMU_IOS_HELPER).
.PHONY: ios-helper ios-test ios-e2e

IOS_HELPER_SRC := $(shell find ios-helper/Package.swift ios-helper/Sources -type f 2>/dev/null)

ifeq ($(shell uname -s),Darwin)
build-extra: ios-helper
endif

ios-helper: $(BIN)/agemu-ios-helper

$(BIN)/agemu-ios-helper: $(IOS_HELPER_SRC)
	cd ios-helper && swift build -c release
	mkdir -p $(BIN)
	# Replace, never overwrite in place: macOS SIGKILLs a signed binary rewritten in place.
	rm -f $(BIN)/agemu-ios-helper
	cp ios-helper/.build/release/agemu-ios-helper $(BIN)/agemu-ios-helper

# Unit tests that do not need a simulator.
ios-test:
	go test ./internal/ios/...

# End-to-end proof on a real simulator (creates/boots agemu-* simulators only).
ios-e2e: build
	AGEMU_BIN=$(BIN)/agemu scripts/e2e-ios.sh
