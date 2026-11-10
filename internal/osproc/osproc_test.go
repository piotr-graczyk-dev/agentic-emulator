package osproc

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestMain doubles as the child process the tests start: OSPROC_CHILD=sleep runs for a minute,
// OSPROC_CHILD=lock takes the exclusive lock on the named file and holds it.
func TestMain(m *testing.M) {
	switch os.Getenv("OSPROC_CHILD") {
	case "sleep":
		if os.Getenv("OSPROC_GRANDCHILD") == "1" {
			c := exec.Command(os.Args[0], "-test.run=^$")
			c.Env = append(os.Environ(), "OSPROC_GRANDCHILD=0")
			_ = c.Start()
			if c.Process != nil {
				_ = os.WriteFile(os.Getenv("OSPROC_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
			}
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "lock":
		f, err := os.OpenFile(os.Getenv("OSPROC_FILE"), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil || TryLock(f, true) != nil {
			os.Exit(3)
		}
		_, _ = f.WriteAt([]byte("held\n"), 0)
		os.Stdout.WriteString("locked\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func child(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(os.Environ(), env...)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDetachedProcessLifecycle(t *testing.T) {
	c := child(t, "OSPROC_CHILD=sleep")
	if err := StartDetached(c); err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Wait() }()
	pid := c.Process.Pid
	if !Alive(pid) || !Exists(pid) || !GroupAlive(pid) {
		t.Fatalf("pid %d: Alive=%v Exists=%v GroupAlive=%v, want all true", pid, Alive(pid), Exists(pid), GroupAlive(pid))
	}
	if err := Kill(pid); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the child to exit", func() bool { return !Alive(pid) })
	if err := Kill(pid); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill of an exited process: %v, want nil or os.ErrProcessDone", err)
	}
}

func TestKillGroupEndsChildren(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild")
	c := child(t, "OSPROC_CHILD=sleep", "OSPROC_GRANDCHILD=1", "OSPROC_PIDFILE="+pidfile)
	if err := StartDetached(c); err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Wait() }()
	var gc int
	waitFor(t, "the grandchild to start", func() bool {
		b, err := os.ReadFile(pidfile)
		if err != nil {
			return false
		}
		gc, _ = strconv.Atoi(string(b))
		return gc > 0 && Alive(gc)
	})
	KillGroup(c.Process.Pid)
	waitFor(t, "the whole group to exit", func() bool { return !GroupAlive(c.Process.Pid) && !Alive(gc) })
}

func TestNoProcess(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if Alive(pid) || Exists(pid) || GroupAlive(pid) {
			t.Errorf("pid %d reported alive", pid)
		}
		if err := Kill(pid); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("Kill(%d) = %v, want os.ErrProcessDone", pid, err)
		}
	}
}

func TestLockAcrossProcessesAndReadableContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agemud.lock")
	c := child(t, "OSPROC_CHILD=lock", "OSPROC_FILE="+path)
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	buf := make([]byte, 7)
	if _, err := out.Read(buf); err != nil || string(buf) != "locked\n" {
		t.Fatalf("child did not take the lock: %q %v", buf, err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := TryLock(f, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("shared lock while another process holds it exclusively: %v, want ErrLocked", err)
	}
	if err := TryLock(f, true); !errors.Is(err, ErrLocked) {
		t.Fatalf("exclusive lock while another process holds it: %v, want ErrLocked", err)
	}
	// The daemon writes its pid into the file it holds locked; others must still read it.
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "held\n" {
		t.Fatalf("reading the locked file: %q %v", b, err)
	}

	_ = c.Process.Kill()
	_ = c.Wait()
	waitFor(t, "the lock to be released", func() bool {
		if TryLock(f, true) != nil {
			return false
		}
		return Unlock(f) == nil
	})
}

func TestMapFileSharesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame")
	const size = 64 << 10
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	m, err := MapFile(f, size)
	f.Close() // the mapping outlives the descriptor, as the Android backend relies on
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != size {
		t.Fatalf("mapping of %d bytes, want %d", len(m), size)
	}
	// A write through the file shows up in the mapping (the emulator writes frames this way).
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("frame-bytes")
	if _, err := w.WriteAt(want, 1000); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if !bytes.Equal(m[1000:1000+len(want)], want) {
		t.Fatalf("mapping does not see the file's write: %q", m[1000:1000+len(want)])
	}
	if err := Unmap(m); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// An unmapped file can be removed again (a live view would keep it open).
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove after Unmap: %v", err)
		}
	}
}
