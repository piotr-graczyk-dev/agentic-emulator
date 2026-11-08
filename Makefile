# bin/agemu      thin CLI + MCP server (one process per agent action, so it stays small)
# bin/agemud     device daemon with both backends, started on demand by agemu
# bin/agemu-ios-helper, bin/agemu-bench
# Platform-specific build steps live in mk/<platform>.mk and hook into build-extra.
#
#   make install     build, copy agemu, agemud and agemu-ios-helper to $(PREFIX)/bin,
#                    restart a running daemon on the new build, run `agemu doctor`
#   make uninstall   remove them (devices and ~/.agemu stay)
.PHONY: build test vet build-extra install uninstall
BIN := bin
PREFIX ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon.Version=$(VERSION)
INSTALL_BINS := agemu agemud agemu-ios-helper

build: build-extra
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/agemu ./cmd/agemu
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/agemud ./cmd/agemud
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/agemu-bench ./cmd/agemu-bench

test:
	go vet ./...
	go test ./...

# The three binaries must stay together: agemu starts the agemud next to it, and agemud the
# helper next to itself. Replace, never overwrite in place: macOS SIGKILLs a signed binary
# rewritten in place.
install: build
	@mkdir -p "$(PREFIX)/bin"
	@for b in $(INSTALL_BINS); do rm -f "$(PREFIX)/bin/$$b"; cp "$(BIN)/$$b" "$(PREFIX)/bin/$$b"; done
	@echo "installed $(INSTALL_BINS) $(VERSION) to $(PREFIX)/bin"
	@"$(PREFIX)/bin/agemu" daemon restart --if-running
	@echo
	@"$(PREFIX)/bin/agemu" doctor || true

uninstall:
	@-"$(PREFIX)/bin/agemu" daemon stop 2>/dev/null
	@for b in $(INSTALL_BINS); do rm -f "$(PREFIX)/bin/$$b"; done
	@echo "removed $(INSTALL_BINS) from $(PREFIX)/bin (devices and ~/.agemu are kept)"

-include mk/*.mk
