// Package osproc holds the few operating-system calls agemu makes that differ between macOS and
// Linux on one side and Windows on the other: starting a process that outlives its parent,
// checking and stopping processes and their children, an advisory file lock, and a shared
// file mapping. Everything else in agemu is portable Go.
//
// A "group" is the process a caller started with StartDetached plus every process it started:
// on Unix the session's process group (its id is the leader's pid), on Windows the process tree
// under that pid.
package osproc

import "errors"

// ErrLocked is returned by TryLock when another process holds a conflicting lock.
var ErrLocked = errors.New("file is locked by another process")
