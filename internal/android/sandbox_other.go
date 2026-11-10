//go:build !darwin

package android

// sysctlDenied: the hw.cachelinesize check is specific to macOS sandboxes (sandbox_darwin.go).
func sysctlDenied() bool { return false }
