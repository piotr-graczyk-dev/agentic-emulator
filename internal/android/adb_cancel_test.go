package android

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// hungADB is an adb server that answers the handshake (if okay is set) and then never sends
// anything, like a wedged adbd behind a live adb server.
func hungADB(t *testing.T, okay bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("ANDROID_ADB_SERVER_PORT", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					var n [4]byte
					if _, err := io.ReadFull(c, n[:]); err != nil {
						return
					}
					l, _ := strconv.ParseUint(string(n[:]), 16, 32)
					if _, err := io.ReadFull(c, buf[:l]); err != nil {
						return
					}
					if okay {
						c.Write([]byte("OKAY"))
					}
				}
			}(c)
		}
	}()
}

func TestADBShellEndsWhenCancelled(t *testing.T) {
	for _, okay := range []bool{true, false} { // stuck after the handshake / in it
		hungADB(t, okay)
		ctx, cancel := context.WithCancel(context.Background()) // no deadline, as a boot's context
		done := make(chan error, 1)
		go func() {
			_, err := adbShellOK(ctx, "emulator-5580", "settings put secure stylus_handwriting_enabled 0")
			done <- err
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("okay=%v: got %v, want context.Canceled", okay, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("okay=%v: adbShell ignored the cancellation", okay)
		}
	}
}

func TestADBShellHonoursItsDeadline(t *testing.T) {
	hungADB(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := adbShellOK(ctx, "emulator-5580", "true"); err == nil {
		t.Fatal("want an error")
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
}

// A cancellation that comes after stop must not expire a connection that is reused (the agent's
// cached stream): a late deadline would fail the next call's read after its request was sent.
func TestUnblockOnCancelStopEndsTheWatch(t *testing.T) {
	for i := 0; i < 2000; i++ {
		a, b := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		stop := unblockOnCancel(ctx, a)
		stop()
		cancel()
		a.SetDeadline(time.Time{})
		go func() { b.Write([]byte("x")); b.Close() }()
		buf := make([]byte, 1)
		if _, err := a.Read(buf); err != nil {
			t.Fatalf("iteration %d: read after stop and a later cancel: %v", i, err)
		}
		a.Close()
	}
	// Before stop, a cancellation still unblocks a read at once.
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop := unblockOnCancel(ctx, a)
	defer stop()
	cancel()
	done := make(chan error, 1)
	go func() { _, err := a.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read succeeded with nothing written")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled context did not unblock the read")
	}
}
