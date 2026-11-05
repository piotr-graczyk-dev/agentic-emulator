//go:build !noios

// Not named backend_ios.go: a _ios.go suffix is an implicit GOOS=ios build constraint,
// which would leave the iOS backend out of darwin builds.

package main

import (
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/ios"
)

func init() {
	backendCtors = append(backendCtors, func() backend.Backend { return ios.New() })
}
