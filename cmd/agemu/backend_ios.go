//go:build !noios

package main

import (
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/ios"
)

func init() {
	backendCtors = append(backendCtors, func() backend.Backend { return ios.New() })
}
