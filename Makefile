# bin/agemu      thin CLI + MCP server (one process per agent action, so it stays small)
# bin/agemud     device daemon with both backends, started on demand by agemu
# bin/agemu-ios-helper, bin/agemu-bench
# Platform-specific build steps live in mk/<platform>.mk and hook into build-extra.
.PHONY: build test vet build-extra
BIN := bin

build: build-extra
	go build -o $(BIN)/agemu ./cmd/agemu
	go build -o $(BIN)/agemud ./cmd/agemud
	go build -o $(BIN)/agemu-bench ./cmd/agemu-bench

test:
	go vet ./...
	go test ./...

-include mk/*.mk
