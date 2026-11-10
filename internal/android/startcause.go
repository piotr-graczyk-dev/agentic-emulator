package android

// Why an emulator start or boot failed, beyond the generic "exited during startup" or timeout:
// agemu reads the tail of the emulator's log for lines that name a cause, and probes whether
// agemud itself runs inside a sandbox, which the emulator it starts inherits. Agent sandboxes
// (the macOS Seatbelt profiles of Codex and Claude Code) break the emulator in ways its log
// does not name. Measured on emulator 37.1.11 (2026-11-10), with a throwaway AVD:
//   - Codex's profile (`codex sandbox -P :workspace`) lets processes read only listed sysctls,
//     not hw.cachelinesize; qemu read it, was denied, and died of SIGILL right after
//     "Found systemPath", with nothing more in its log. Claude Code's list leaves it out too.
//   - The same profile denies network by default: agemu could not listen on any port and
//     failed with "no free emulator ports".
//   - A profile that restricts mach-lookup made the emulator's crash handler fail its port
//     check-in ("bootstrap_check_in ...: Permission denied (1100)") while the guest booted.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// startLogTail is how much of the end of the emulator's log is read.
const startLogTail = 64 << 10

// logSignatures are emulator log lines that show the emulator runs in a sandbox (every
// substring must be in the line, compared lowercased), with what they mean.
var logSignatures = []struct {
	all  []string
	what string
}{
	{[]string{"bootstrap_check_in", "permission denied"}, "the emulator was denied a Mach port"},
	// From a user report, not reproduced here: the emulator could not make memory executable.
	{[]string{"mprotect", "not permitted"}, "the emulator could not map executable memory"},
	{[]string{"mprotect", "permission denied"}, "the emulator could not map executable memory"},
}

// sandboxProbe reports what agemud itself is denied that the emulator needs (empty outside a
// sandbox). A variable for tests.
var sandboxProbe = probeSandbox

func probeSandbox() []string {
	var why []string
	if sysctlDenied() { // sandbox_darwin.go
		why = append(why, "it may not read sysctl hw.cachelinesize, without which the emulator dies of SIGILL at start")
	}
	if l, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			why = append(why, "it may not listen on localhost (network access denied)")
		}
	} else {
		l.Close()
	}
	return why
}

// startCause explains a failed start from the emulator's log tail and the sandbox probe's
// findings, or returns "" when neither shows anything.
func startCause(logTail []byte, probe []string) string {
	evidence := append([]string(nil), probe...)
	var lines []string
	seen := map[string]bool{}
	for _, l := range strings.Split(string(logTail), "\n") {
		l = strings.TrimSpace(l)
		low := strings.ToLower(l)
		for _, s := range logSignatures {
			match := true
			for _, sub := range s.all {
				if !strings.Contains(low, sub) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if !seen[s.what] {
				seen[s.what] = true
				evidence = append(evidence, s.what)
			}
			if len(lines) < 3 {
				lines = append(lines, l)
			}
			break
		}
	}
	if len(evidence) == 0 {
		return ""
	}
	msg := "agemud runs inside a sandbox, which the emulator inherits (" + strings.Join(evidence, "; ") +
		"); start the daemon outside it: run `agemu daemon restart` from an unsandboxed terminal, then boot again"
	if len(lines) > 0 {
		msg += "; emulator log: " + strings.Join(lines, " | ")
	}
	return msg
}

// explainStart adds the cause startCause finds to err, a failed boot of d that started at
// start. The log is read only when this boot wrote it (it is rewritten at each emulator start).
func (d *device) explainStart(err error, start time.Time) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var tail []byte
	path := filepath.Join(logDir(), d.name+".log")
	if fi, serr := os.Stat(path); serr == nil && !fi.ModTime().Before(start.Add(-time.Second)) {
		tail = readTail(path, startLogTail)
	}
	if c := startCause(tail, sandboxProbe()); c != "" {
		return fmt.Errorf("%w; cause: %s", err, c)
	}
	return err
}

// readTail returns up to n bytes from the end of the file at path (nil if it cannot be read).
func readTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	cut := false
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return nil
		}
		cut = true
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if i := bytes.IndexByte(b, '\n'); cut && i >= 0 {
		b = b[i+1:] // drop the partial first line
	}
	return b
}
