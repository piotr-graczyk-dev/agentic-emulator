package android

import (
	"errors"
	"fmt"
	"testing"
)

func TestRendererMismatch(t *testing.T) {
	// The emulator's gRPC error for a baseline taken with another -gpu mode (seen with -gpu host
	// on a baseline from -gpu auto).
	got := fmt.Errorf("load snapshot agemu-baseline: %w", errors.New("Operation failed due to: Error -1 from the snapshot callback\n\nSnapshot load failure: different renderer configured\n"))
	if !rendererMismatch(got) {
		t.Fatal("renderer mismatch not recognised")
	}
	if rendererMismatch(errors.New("load snapshot agemu-baseline: deadline exceeded")) || rendererMismatch(nil) {
		t.Fatal("other errors are not a renderer mismatch")
	}
}
