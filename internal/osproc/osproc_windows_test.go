//go:build windows

package osproc

import (
	"os"
	"testing"
	"time"
)

// Windows reuses process ids quickly, so the group functions act only on processes
// StartDetached started: any other pid, even a live one, is not a group of ours.
func TestGroupFunctionsIgnoreUntrackedPIDs(t *testing.T) {
	c := child(t, "OSPROC_CHILD=sleep")
	if err := c.Start(); err != nil { // started without StartDetached
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	if GroupAlive(c.Process.Pid) || GroupAlive(os.Getpid()) {
		t.Fatal("GroupAlive is true for a process StartDetached did not start")
	}
	KillGroup(c.Process.Pid)
	time.Sleep(200 * time.Millisecond)
	if !Alive(c.Process.Pid) {
		t.Fatal("KillGroup ended a process StartDetached did not start")
	}
}

// The handle kept for a started process is released once its group has exited, also when nothing
// calls the group functions (an emulator that shut down by itself).
func TestTrackingEndsWithTheGroup(t *testing.T) {
	defer func(d time.Duration) { groupWatchInterval = d }(groupWatchInterval)
	groupWatchInterval = 20 * time.Millisecond
	c := child(t, "OSPROC_CHILD=sleep")
	if err := StartDetached(c); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	_ = c.Process.Kill()
	_ = c.Wait()
	waitFor(t, "the tracking to end", func() bool {
		trackedMu.Lock()
		defer trackedMu.Unlock()
		_, ok := trackedBy[pid]
		return !ok
	})
}
