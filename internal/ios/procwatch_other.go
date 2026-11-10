//go:build !darwin && !windows

package ios

import (
	"errors"
	"time"
)

// procWatcher has no kqueue outside macOS (where Simulators do not run): AppState then finds
// a launched process gone with kill(pid, 0).
type procWatcher struct{}

func newProcWatcher(func(pid int, at time.Time)) (*procWatcher, error) {
	return nil, errors.New("process exit watching needs macOS")
}

func (w *procWatcher) add(int) error { return errors.New("process exit watching needs macOS") }

func (w *procWatcher) close() {}
