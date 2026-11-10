package android

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// The fixtures are real emulator logs (emulator 37.1.11, 2026-11-10, a throwaway AVD; the home
// directory replaced by /Users/dev):
//   - emulator-seatbelt-sigill.log: the whole log of an emulator started under a Seatbelt
//     profile with Codex's sysctl allowlist (no hw.cachelinesize); it died of SIGILL, logging
//     nothing about why. The same happened under `codex sandbox -P :workspace`.
//   - emulator-seatbelt-machlookup.log: the head of the log of an emulator started under a
//     profile that also restricts mach-lookup, as Claude Code's does.

// A failed boot names the sandbox the log shows, with the log line and the fix.
func TestBootErrorNamesSandboxFromEmulatorLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGEMU_HOME", home)
	fixture, err := os.ReadFile(filepath.Join("testdata", "emulator-seatbelt-machlookup.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "android"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "android", "agemu-test-prep.log")
	if err := os.WriteFile(logPath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	generic := errors.New("emulator did not register its gRPC endpoint in 60s; see " + logPath)
	b := New()
	b.bootHook = func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
		return api.Device{}, generic
	}
	_, err = b.Boot(context.Background(), testID, api.BootOptions{})
	if err == nil {
		t.Fatal("boot succeeded")
	}
	for _, want := range []string{
		generic.Error(),
		"agemud runs inside a sandbox",
		"the emulator was denied a Mach port",
		"bootstrap_check_in org.chromium.crashpad.child_port_handshake.51562.3360111.HLFRSVGZDYJSRSRS: Permission denied (1100)",
		"`agemu daemon restart` from an unsandboxed terminal",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boot error lacks %q:\n%v", want, err)
		}
	}
}

// The SIGILL log names nothing: the cause comes from probing agemud's own sandbox.
func TestStartCauseSigillNeedsProbe(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "emulator-seatbelt-sigill.log"))
	if err != nil {
		t.Fatal(err)
	}
	if c := startCause(fixture, nil); c != "" {
		t.Fatalf("cause from the log alone: %q, want none (the log names no cause)", c)
	}
	c := startCause(fixture, []string{"it may not read sysctl hw.cachelinesize, without which the emulator dies of SIGILL at start"})
	for _, want := range []string{"agemud runs inside a sandbox", "hw.cachelinesize", "agemu daemon restart"} {
		if !strings.Contains(c, want) {
			t.Errorf("cause lacks %q: %q", want, c)
		}
	}
	if strings.Contains(c, "emulator log:") {
		t.Errorf("cause quotes log lines that name nothing: %q", c)
	}
}

// explainStart probes the sandbox and leaves cancellations and stale logs alone.
func TestExplainStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGEMU_HOME", home)
	old := sandboxProbe
	t.Cleanup(func() { sandboxProbe = old })
	probe := []string{"it may not listen on localhost (network access denied)"}
	sandboxProbe = func() []string { return probe }
	d := &device{name: "agemu-test-explain"}
	start := time.Now()

	base := errors.New("no free emulator ports from 5580 / 8580")
	if got := d.explainStart(base, start); !errors.Is(got, base) || !strings.Contains(got.Error(), "may not listen on localhost") {
		t.Fatalf("got %v", got)
	}
	if got := d.explainStart(context.Canceled, start); got != context.Canceled {
		t.Fatalf("a cancellation got a cause: %v", got)
	}
	if got := d.explainStart(nil, start); got != nil {
		t.Fatalf("nil became %v", got)
	}
	probe = nil
	if got := d.explainStart(base, start); got != base {
		t.Fatalf("nothing known, got %v", got)
	}

	// A log from an earlier emulator start is not read.
	if err := os.MkdirAll(filepath.Join(home, "android"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "android", d.name+".log")
	if err := os.WriteFile(logPath, []byte("x: mprotect failed: Operation not permitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := d.explainStart(base, start.Add(-10*time.Second)); !strings.Contains(got.Error(), "could not map executable memory") {
		t.Fatalf("fresh log not read: %v", got)
	}
	if got := d.explainStart(base, start.Add(10*time.Second)); got != base {
		t.Fatalf("stale log read: %v", got)
	}
}

func TestReadTailDropsPartialLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(p, []byte("first line\nsecond\nthird\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := string(readTail(p, 10)); got != "third\n" {
		t.Fatalf("got %q", got)
	}
	if got := string(readTail(p, 1000)); got != "first line\nsecond\nthird\n" {
		t.Fatalf("got %q", got)
	}
}
