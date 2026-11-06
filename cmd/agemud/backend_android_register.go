//go:build !noandroid

package main

import (
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

func init() {
	backendCtors = append(backendCtors, func() backend.Backend { return android.New() })
}
