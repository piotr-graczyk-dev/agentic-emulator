package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

func TestParse(t *testing.T) {
	c := parse([]string{"-d", "android:x", "--text", "Log in", "--index=2", "100", "-5", "--cold"})
	if c.flags["device"] != "android:x" || c.flags["text"] != "Log in" || c.flags["index"] != "2" || c.flags["cold"] != "true" {
		t.Fatalf("flags: %#v", c.flags)
	}
	if len(c.pos) != 2 || c.pos[0] != "100" || c.pos[1] != "-5" {
		t.Fatalf("pos: %#v", c.pos)
	}
	s := c.selector()
	if s == nil || s.Text != "Log in" || s.Index != 2 {
		t.Fatalf("selector: %#v", s)
	}
}

func TestParseLaunchArgs(t *testing.T) {
	c := parse([]string{"pl.app", "--relaunch", "-d", "ios:x", "--", "--initialUrl", "http://localhost:8081", "-EXDevMenuShowsAtLaunch", "NO"})
	if c.flags["relaunch"] != "true" || c.flags["device"] != "ios:x" {
		t.Fatalf("flags: %#v", c.flags)
	}
	want := []string{"pl.app", "--initialUrl", "http://localhost:8081", "-EXDevMenuShowsAtLaunch", "NO"}
	if len(c.pos) != len(want) {
		t.Fatalf("pos: %#v", c.pos)
	}
	for i := range want {
		if c.pos[i] != want[i] {
			t.Fatalf("pos: %#v", c.pos)
		}
	}
}

func req(t *testing.T, stdin string, args ...string) (string, daemon.Params, error) {
	t.Helper()
	t.Setenv("AGEMU_DEVICE", "")
	return request(args[0], parse(args[1:]), strings.NewReader(stdin))
}

func TestCapabilityCommands(t *testing.T) {
	payloadFile := filepath.Join(t.TempDir(), "push.json")
	if err := os.WriteFile(payloadFile, []byte(`{"aps":{"badge":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args   []string
		stdin  string
		method string
		want   string // the parameters as JSON
	}{
		{[]string{"open", "myapp://item/7", "--wait-text", "Item 7", "--ui"}, "", "open_url", `{"waitFor":{"text":"Item 7"},"returnUi":true,"url":"myapp://item/7"}`},
		{[]string{"permission", "grant", "com.app", "camera", "microphone,photos"}, "", "permission", `{"app":"com.app","action":"grant","permissions":["camera","microphone,photos"]}`},
		{[]string{"permission", "reset", "com.app", "-d", "ios:y"}, "", "permission", `{"device":"ios:y","app":"com.app","action":"reset"}`},
		{[]string{"location", "set", "-33.8688", "151.2093"}, "", "location", `{"action":"set","latitude":-33.8688,"longitude":151.2093}`},
		{[]string{"location", "52.2297", "21.0122"}, "", "location", `{"latitude":52.2297,"longitude":21.0122}`},
		{[]string{"location", "clear"}, "", "location", `{"action":"clear"}`},
		{[]string{"clipboard"}, "", "clipboard", `{"action":"get"}`},
		{[]string{"clipboard", "get"}, "", "clipboard", `{"action":"get"}`},
		{[]string{"clipboard", "set", "otp", "1234"}, "", "clipboard", `{"text":"otp 1234","action":"set"}`},
		{[]string{"clipboard", "set", "--", "--not-a-flag"}, "", "clipboard", `{"text":"--not-a-flag","action":"set"}`},
		{[]string{"clipboard", "set"}, "", "clipboard", `{"action":"set"}`},
		{[]string{"appearance"}, "", "appearance", `{}`},
		{[]string{"appearance", "dark"}, "", "appearance", `{"mode":"dark"}`},
		{[]string{"appearance", "set", "light"}, "", "appearance", `{"mode":"light"}`},
		{[]string{"apps"}, "", "apps", `{}`},
		{[]string{"uninstall", "com.app"}, "", "uninstall", `{"app":"com.app"}`},
		{[]string{"clear-data", "com.app"}, "", "clear_app_data", `{"app":"com.app"}`},
		{[]string{"push", "com.app", `{"aps":{"alert":"hi"}}`, "--wait-contains", "hi"}, "", "push", `{"app":"com.app","waitFor":{"contains":"hi"},"payload":{"aps":{"alert":"hi"}}}`},
		{[]string{"push", "com.app", "-"}, `{"aps":{"sound":"default"}}`, "push", `{"app":"com.app","payload":{"aps":{"sound":"default"}}}`},
		{[]string{"push", "com.app", payloadFile}, "", "push", `{"app":"com.app","payload":{"aps":{"badge":3}}}`},
		{[]string{"profile", "pixel-9"}, "", "set_profile", `{"profile":"pixel-9"}`},
	}
	for _, c := range cases {
		method, p, err := req(t, c.stdin, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		b, _ := json.Marshal(p)
		if method != c.method || string(b) != c.want {
			t.Fatalf("%v:\n got %s %s\nwant %s %s", c.args, method, b, c.method, c.want)
		}
	}
}

func TestCapabilityCommandErrors(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"open"}, "open needs a url"},
		{[]string{"permission", "grant"}, "usage: permission"},
		{[]string{"location", "set", "1"}, "usage: location"},
		{[]string{"location", "set", "NaN", "1"}, "bad coordinates"},
		{[]string{"location", "set", "north", "1"}, "bad coordinates"},
		{[]string{"clipboard", "copy"}, "usage: clipboard"},
		{[]string{"clipboard", "get", "x"}, "usage: clipboard"},
		{[]string{"uninstall"}, "uninstall needs an app id"},
		{[]string{"clear-data"}, "clear-data needs an app id"},
		{[]string{"push", "com.app"}, "usage: push"},
		{[]string{"push", "com.app", `{"aps":`}, "not valid JSON"},
		{[]string{"push", "com.app", "/nonexistent/agemu-push.json"}, "reading the payload"},
		{[]string{"teleport"}, "unknown command"},
	}
	for _, c := range cases {
		_, _, err := req(t, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestLogsAndCrashesCommands(t *testing.T) {
	cases := []struct {
		args   []string
		method string
		want   string
	}{
		{[]string{"logs"}, "logs", `{}`},
		{[]string{"logs", "com.app", "--since", "launch", "--level", "warn"}, "logs", `{"app":"com.app","since":"launch","level":"warn"}`},
		{[]string{"logs", "--app", "pl.app", "--contains", "TypeError", "--regex", "fail(ed)?", "--limit", "20", "--max-bytes", "4096", "-d", "ios:y"}, "logs", `{"device":"ios:y","app":"pl.app","contains":"TypeError","regex":"fail(ed)?","limit":20,"maxBytes":4096}`},
		{[]string{"logs", "--since", "c1730970000123456", "--json"}, "logs", `{"since":"c1730970000123456"}`},
		{[]string{"crashes"}, "crashes", `{}`},
		{[]string{"crashes", "com.app", "--since", "5m", "--limit", "3"}, "crashes", `{"app":"com.app","since":"5m","limit":3}`},
	}
	for _, c := range cases {
		method, p, err := req(t, "", c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		b, _ := json.Marshal(p)
		if method != c.method || string(b) != c.want {
			t.Fatalf("%v:\n got %s %s\nwant %s %s", c.args, method, b, c.method, c.want)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"logs", "a", "b"}, "usage: logs"},
		{[]string{"logs", "a", "--app", "b"}, "usage: logs"},
		{[]string{"logs", "--limit", "many"}, `--limit must be a whole number, got "many"`},
		{[]string{"logs", "--max-bytes", "1.5"}, "--max-bytes must be a whole number"},
		{[]string{"crashes", "--limit", "x"}, "--limit must be a whole number"},
		{[]string{"crashes", "--app", "com.app", "--level", "error"}, "crashes does not take --level"}, // a logs-only flag
	} {
		if _, _, err := req(t, "", c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

// A flag the command does not take is a usage error (exit 2) before the daemon is asked: parse
// takes every --name, and `wait --contains X --wait-gone` used to wait for X to appear.
func TestFlagsTheCommandDoesNotTake(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"wait", "--contains", "X", "--wait-gone"}, []string{"wait does not take --wait-gone (did you mean --gone?)", "--gone", "--timeout-ms"}},
		{[]string{"wait", "--wait-contains", "X", "--gone"}, []string{"wait does not take --wait-contains (did you mean --contains?)"}},
		{[]string{"wait", "--id", "a", "--wait-timeout-ms", "500"}, []string{"did you mean --timeout-ms?"}},
		{[]string{"find", "--text", "X", "--wait-text", "Y"}, []string{"find does not take --wait-text (did you mean --text?)"}},
		{[]string{"find", "--text", "X", "--gone"}, []string{"find does not take --gone; it takes -d/--device --json --text"}},
		{[]string{"tap", "--text", "X", "--wait-text", "Y", "--gone"}, []string{"tap does not take --gone (did you mean --wait-gone?)"}},
		{[]string{"tap", "--text", "X", "--wait-id", "Y", "--timeout-ms", "9"}, []string{"did you mean --wait-timeout-ms?"}},
		{[]string{"tap", "--txt", "X"}, []string{"tap does not take --txt;", "--wait-gone --wait-timeout-ms --ui"}},
		{[]string{"tap", "--text", "X", "-o", "out.png"}, []string{"tap does not take -o;"}},
		{[]string{"screenshot", "--out=shot.png"}, []string{"screenshot does not take --out;", "-o/--output"}},
		{[]string{"reset", "--cold"}, []string{"reset does not take --cold"}},
		{[]string{"devices", "--platform", "ios"}, []string{"devices does not take --platform; it takes -d/--device --json ("}},
	}
	for _, c := range cases {
		_, _, err := req(t, "", c.args...)
		if err == nil {
			t.Fatalf("%v: no error", c.args)
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("%v: err = %q, want %q", c.args, err, w)
			}
		}
		if code := exitCode(err); code != 2 {
			t.Fatalf("%v: exit %d, want 2 (usage)", c.args, code)
		}
	}
	// An unknown command is still reported as one.
	if _, _, err := req(t, "", "teleport", "--fast"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("teleport: %v", err)
	}
}

// Every command line of scripts/e2e-*.sh, examples/agent-race/drivers.go, README.md and docs/ still
// parses (the flag check must not refuse a valid one).
func TestValidCommandLinesStillParse(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--platform", "ios", "--name", "agemu-iphone-17-pro", "--profile", "iphone-17-pro"},
		{"create", "--platform", "android", "--name", "pixel"},
		{"profiles", "--platform", "ios"},
		{"boot", "agemu-iphone", "--cold", "--no-wait"},
		{"devices", "--json"},
		{"ui", "-d", "android:x"},
		{"ui", "--json"},
		{"wait", "--id", "About", "--gone", "--timeout-ms", "10000"},
		{"wait", "--role", "textfield", "--text", "Wallpaper", "--timeout-ms", "5000"},
		{"wait", "--contains", "pid:", "--timeout-ms", "8000", "-d", "android:x"},
		{"find", "--role", "textfield", "--json"},
		{"find", "--id", "breadcrumb"},
		{"screenshot", "-o", "shot.png", "-d", "android:x"},
		{"screenshot", "--scale", "0.5", "--format", "jpeg"},
		{"tap", "--text", "Sign in", "--wait-text", "Welcome", "--ui"},
		{"tap", "--id", "crash-exception", "--wait-id", "never-there", "--wait-timeout-ms", "10000"},
		{"tap", "--text", "Done", "--wait-text", "Loading", "--wait-gone"},
		{"tap", "--id", "log-levels", "--wait-contains", "levels tok"},
		{"swipe", "10", "20", "10", "80", "--ms", "500", "--ui"},
		{"type", "--keys", "wifi"},
		{"type", "hello", "--wait-contains", "hello"},
		{"key", "back", "--wait-text", "Settings"},
		{"profile", "--width", "900", "--height", "1800", "--density", "360"},
		{"launch", "dev.agemu.testapp", "--relaunch", "--", "-logs", "--initialUrl", "http://127.0.0.1:8081"}, // after --: arguments, not flags
		{"launch", "pl.app", "--wait-text", "Sign in"},
		{"open", "myapp://orders/7", "--wait-id", "order-7", "--ui"},
		{"push", "com.app", `{"aps":{"alert":"Hi"}}`, "--wait-contains", "Hi"},
		{"logs", "dev.agemu.testapp", "--since", "launch", "--limit", "300", "--max-bytes", "200000"},
		{"logs", "com.example.app", "--since", "launch", "--level", "warn"},
		{"logs", "pl.app", "--since", "launch", "--contains", "level ", "--json"},
		{"crashes", "dev.agemu.testapp", "--since", "launch", "--limit", "1"},
		{"clipboard", "--json"},
	} {
		if _, _, err := req(t, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for cmd := range commandFlags { // every command in the table is one params knows
		if _, _, err := req(t, "", cmd); err != nil && strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("commandFlags has %q, which params does not know", cmd)
		}
	}
}

func TestExitCodes(t *testing.T) {
	crashed := &api.AppCrashedError{Err: errors.New("no element matches")}
	cases := []struct {
		err  error
		want int
	}{
		{errors.New("boom"), 1},
		{api.NotImplemented(api.Android, "logs", ""), 3},
		{crashed, 4},
		{fmt.Errorf("tap done, but: %w", crashed), 4},
		{&client.Error{Message: "x", Code: daemon.CodeAppCrashed}, 4},
		{&client.Error{Message: "x", Code: daemon.CodeNotImplemented}, 3},
		{&api.AppCrashedError{Err: api.NotImplemented(api.IOS, "crashes", "")}, 3},
		{&api.PreparingError{Device: "android:x", Phase: "settling"}, 5},
		{&client.Error{Message: "x", Code: daemon.CodePreparing}, 5},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Fatalf("%v: exit %d, want %d", c.err, got, c.want)
		}
	}
}

// fakeDaemon answers devices with the states in turn, and boot with booted.
func fakeDaemon(t *testing.T, conn net.Conn, states []api.Device) {
	r := bufio.NewReader(conn)
	i := 0
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req daemon.Request
		_ = json.Unmarshal(line, &req)
		var res any
		switch req.Method {
		case "devices":
			res = []api.Device{states[i]}
			if i < len(states)-1 {
				i++
			}
		case "boot":
			res = api.Device{ID: "android:x", State: "booted"}
		}
		b, _ := json.Marshal(res)
		out, _ := json.Marshal(daemon.Response{ID: req.ID, Result: b})
		conn.Write(append(out, '\n'))
	}
}

func TestBootWaitsForThePreparationWithProgress(t *testing.T) {
	old1, old2 := prepPoll, prepReport
	prepPoll, prepReport = time.Millisecond, time.Hour
	defer func() { prepPoll, prepReport = old1, old2 }()
	a, b := net.Pipe()
	defer a.Close()
	prep := func(phase string) api.Device {
		return api.Device{ID: "android:x", State: api.StatePreparing, Preparing: &api.Preparing{Phase: phase, ElapsedS: 10, MaxS: 870}}
	}
	go fakeDaemon(t, b, []api.Device{prep("cold boot"), prep("settling"), prep("settling"), {ID: "android:x", State: "booted"}})
	cl := client.New(a)
	first, _ := json.Marshal(prep("cold boot"))
	var progress bytes.Buffer
	raw, err := waitPrepared(cl, daemon.Params{Device: "android:x", NoWait: true}, first, &progress)
	if err != nil {
		t.Fatal(err)
	}
	var dev api.Device
	_ = json.Unmarshal(raw, &dev)
	if dev.State != "booted" {
		t.Fatalf("final %s", raw)
	}
	out := progress.String()
	for _, want := range []string{"cold boot", "settling", "at most ~15 min", "baseline ready"} {
		if !strings.Contains(out, want) {
			t.Fatalf("progress %q lacks %q", out, want)
		}
	}
	if strings.Count(out, "settling") != 1 {
		t.Fatalf("a phase is printed once (until prepReport): %q", out)
	}
}

func TestBootReportsAFailedPreparation(t *testing.T) {
	old := prepPoll
	prepPoll = time.Millisecond
	defer func() { prepPoll = old }()
	a, b := net.Pipe()
	defer a.Close()
	go fakeDaemon(t, b, []api.Device{{ID: "android:x", State: "shutdown", PrepareError: "cancelled by shutdown"}})
	first, _ := json.Marshal(api.Device{ID: "android:x", State: api.StatePreparing, Preparing: &api.Preparing{Phase: "settling"}})
	_, err := waitPrepared(client.New(a), daemon.Params{Device: "android:x"}, first, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cancelled by shutdown") {
		t.Fatalf("got %v", err)
	}
}

func TestBootOfABootedDeviceDoesNotPoll(t *testing.T) {
	raw, _ := json.Marshal(api.Device{ID: "android:x", State: "booted"})
	got, err := waitPrepared(nil, daemon.Params{}, raw, io.Discard)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("%s %v", got, err)
	}
}

// `boot --cold` of a device that prepared its baseline: the preparation was the cold boot, so the
// final call that reads the booted device must not ask for another one.
func TestBootColdDoesNotColdBootAgainAfterThePreparation(t *testing.T) {
	old := prepPoll
	prepPoll = time.Millisecond
	defer func() { prepPoll = old }()
	a, b := net.Pipe()
	defer a.Close()
	boots := make(chan daemon.Params, 4)
	go func() {
		r := bufio.NewReader(b)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req daemon.Request
			_ = json.Unmarshal(line, &req)
			var res any = []api.Device{{ID: "android:x", State: "booted"}}
			if req.Method == "boot" {
				var p daemon.Params
				_ = json.Unmarshal(req.Params, &p)
				boots <- p
				res = api.Device{ID: "android:x", State: "booted"}
			}
			bs, _ := json.Marshal(res)
			out, _ := json.Marshal(daemon.Response{ID: req.ID, Result: bs})
			b.Write(append(out, '\n'))
		}
	}()
	first, _ := json.Marshal(api.Device{ID: "android:x", State: api.StatePreparing, Preparing: &api.Preparing{Phase: "cold boot"}})
	if _, err := waitPrepared(client.New(a), daemon.Params{Device: "android:x", Cold: true, NoWait: true}, first, io.Discard); err != nil {
		t.Fatal(err)
	}
	if p := <-boots; p.Cold || p.NoWait {
		t.Fatalf("the final boot asked for cold=%v noWait=%v", p.Cold, p.NoWait)
	}
}
