package main

import "github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"

// backendCtors is filled by backend_<platform>.go. Each file has a build tag
// (noandroid / noios) so a platform can be compiled out, e.g. `go build -tags noios`.
var backendCtors []func() backend.Backend

func backends() []backend.Backend {
	var bs []backend.Backend
	for _, c := range backendCtors {
		bs = append(bs, c())
	}
	return bs
}
