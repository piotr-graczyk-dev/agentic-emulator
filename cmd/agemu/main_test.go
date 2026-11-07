package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{[]string{"crashes", "--app", "com.app", "--level", "error"}, "crashes", `{"app":"com.app"}`}, // logs-only flags are not sent
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
	} {
		if _, _, err := req(t, "", c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: err = %v, want %q", c.args, err, c.want)
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
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Fatalf("%v: exit %d, want %d", c.err, got, c.want)
		}
	}
}
