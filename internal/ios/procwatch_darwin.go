//go:build darwin

package ios

import (
	"sync"
	"syscall"
	"time"
)

// procWatcher reports process exits with kqueue (EVFILT_PROC, NOTE_EXIT). A Simulator app is a
// Mac process, so the daemon sees it end within milliseconds, without polling and without the
// log stream. Only the end is known: the exit status goes to the parent (launchd_sim).
type procWatcher struct {
	kq     int
	onExit func(pid int, at time.Time)
	done   chan struct{}
	once   sync.Once
}

// wakeIdent is the EVFILT_USER event that stops the loop.
const wakeIdent = 1

func newProcWatcher(onExit func(pid int, at time.Time)) (*procWatcher, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	wake := syscall.Kevent_t{Ident: wakeIdent, Filter: syscall.EVFILT_USER, Flags: syscall.EV_ADD | syscall.EV_CLEAR}
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{wake}, nil, nil); err != nil {
		syscall.Close(kq)
		return nil, err
	}
	w := &procWatcher{kq: kq, onExit: onExit, done: make(chan struct{})}
	go w.loop()
	return w, nil
}

// add watches pid until it exits (once). It fails with ESRCH when pid has already exited.
func (w *procWatcher) add(pid int) error {
	ev := syscall.Kevent_t{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}
	_, err := syscall.Kevent(w.kq, []syscall.Kevent_t{ev}, nil, nil)
	return err
}

func (w *procWatcher) loop() {
	defer close(w.done)
	events := make([]syscall.Kevent_t, 32)
	for {
		n, err := syscall.Kevent(w.kq, nil, events, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return
		}
		now := time.Now()
		for _, ev := range events[:n] {
			switch {
			case ev.Filter == syscall.EVFILT_USER:
				return
			case ev.Filter == syscall.EVFILT_PROC && ev.Fflags&syscall.NOTE_EXIT != 0:
				w.onExit(int(ev.Ident), now)
			}
		}
	}
}

// close stops the loop and releases the kqueue.
func (w *procWatcher) close() {
	w.once.Do(func() {
		wake := syscall.Kevent_t{Ident: wakeIdent, Filter: syscall.EVFILT_USER, Fflags: syscall.NOTE_TRIGGER}
		if _, err := syscall.Kevent(w.kq, []syscall.Kevent_t{wake}, nil, nil); err == nil {
			<-w.done
		}
		syscall.Close(w.kq)
	})
}
