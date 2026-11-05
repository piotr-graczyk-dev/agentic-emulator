# Platform-specific build steps live in mk/<platform>.mk and hook into build-extra.
.PHONY: build test vet build-extra
BIN := bin

build: build-extra
	go build -o $(BIN)/agemu ./cmd/agemu
	go build -o $(BIN)/agemu-bench ./cmd/agemu-bench

test:
	go vet ./...
	go test ./...

-include mk/*.mk
