package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

func TestToolList(t *testing.T) {
	want := []string{
		"devices", "profiles", "create", "boot", "shutdown", "reset", "set_profile", "screenshot",
		"ui_tree", "find", "tap", "swipe", "type_text", "press_key", "install", "launch",
		"terminate", "wait_for",
		"open_url", "permission", "location", "clipboard", "appearance", "apps", "uninstall",
		"clear_app_data", "push",
		"logs", "crashes",
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if tl.method == "" || tl.Description == "" {
			t.Fatalf("%s: no method or description", tl.Name)
		}
		if tl.selector != (tl.Name == "find" || tl.Name == "tap" || tl.Name == "wait_for") {
			t.Fatalf("%s: selector = %v", tl.Name, tl.selector)
		}
		props, _ := tl.InputSchema["properties"].(map[string]any)
		req, _ := tl.InputSchema["required"].([]string)
		for _, r := range req {
			if _, ok := props[r]; !ok {
				t.Fatalf("%s: required %q is not a property", tl.Name, r)
			}
		}
	}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("tools = %v\nwant    %v (the list grows only on purpose)", names, want)
	}
	if _, err := json.Marshal(map[string]any{"tools": tools}); err != nil {
		t.Fatal(err)
	}
}

// recorder is an iOS backend with some capabilities (no AppearanceSetter); it records calls.
type recorder struct {
	backend.Backend
	calls []string
}

func (r *recorder) Platform() api.Platform { return api.IOS }
func (r *recorder) add(f string, a ...any) { r.calls = append(r.calls, fmt.Sprintf(f, a...)) }
func (r *recorder) UITree(ctx context.Context, id string) (*api.UITree, error) {
	return &api.UITree{Width: 100, Height: 100, Root: &api.Element{Role: "window", Frame: api.Rect{W: 100, H: 100}, Children: []*api.Element{
		{Role: "button", Text: "Log in", Frame: api.Rect{X: 10, Y: 20, W: 30, H: 10}, Enabled: true},
	}}}, nil
}
func (r *recorder) Tap(ctx context.Context, id string, x, y int) error {
	r.add("tap %d,%d", x, y)
	return nil
}
func (r *recorder) TypeText(ctx context.Context, id, text string) error {
	r.add("type %q", text)
	return nil
}
func (r *recorder) OpenURL(ctx context.Context, id, url string) (string, error) {
	r.add("open %s", url)
	return "", nil
}
func (r *recorder) SetPermissions(ctx context.Context, id, app string, action api.PermissionAction, perms []api.Permission) error {
	r.add("perm %s %s %v", app, action, perms)
	return nil
}
func (r *recorder) SetLocation(ctx context.Context, id string, lat, lon float64) error {
	r.add("loc %v,%v", lat, lon)
	return nil
}
func (r *recorder) ClearLocation(ctx context.Context, id string) error { r.add("clearloc"); return nil }
func (r *recorder) Clipboard(ctx context.Context, id string) (string, error) {
	return "copied", nil
}
func (r *recorder) SetClipboard(ctx context.Context, id, text string) error {
	r.add("setclip %q", text)
	return nil
}
func (r *recorder) Push(ctx context.Context, id, app string, payload []byte) error {
	r.add("push %s %s", app, payload)
	return nil
}
func (r *recorder) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	r.add("logs app=%s level=%s contains=%s regex=%s after=%d", q.App, q.Level, q.Contains, q.Regex, q.After)
	at := time.Now().Add(-time.Second)
	return api.LogBatch{Cursor: 99, Entries: []api.LogEntry{
		{Time: at, Level: api.LogInfo, Pid: 7, Process: "AgemuTestApp", Tag: "com.example:net", Message: "GET /items"},
		{Time: at, Level: api.LogError, Pid: 7, Process: "AgemuTestApp", Tag: "com.example:net", Message: "request failed"},
	}}, nil
}

func connect(t *testing.T, b backend.Backend) *client.Client {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c1, c2 := net.Pipe()
	go daemon.ServeConn(ctx, daemon.NewService(t.TempDir(), b), c2)
	cl := client.New(c1)
	t.Cleanup(func() { cl.Close() })
	return cl
}

func toolArgs(t *testing.T, s string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func resultText(res map[string]any) string {
	var sb strings.Builder
	for _, c := range res["content"].([]map[string]any) {
		if s, ok := c["text"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func TestToolArgumentsReachTheBackend(t *testing.T) {
	cases := []struct{ tool, args, call string }{
		{"type_text", `{"text":"hello"}`, `type "hello"`},
		{"tap", `{"text":"Log in"}`, "tap 25,25"},
		{"clipboard", `{"text":"otp 1234"}`, `setclip "otp 1234"`},
		{"clipboard", `{"action":"set","text":""}`, `setclip ""`},
		{"open_url", `{"url":"myapp://item/7"}`, "open myapp://item/7"},
		{"permission", `{"app":"com.app","action":"grant","permissions":["camera","location-always"]}`, "perm com.app grant [camera location-always]"},
		{"location", `{"latitude":52.5,"longitude":-0.12}`, "loc 52.5,-0.12"},
		{"location", `{"action":"clear"}`, "clearloc"},
		{"push", `{"app":"com.app","payload":{"aps":{"alert":{"title":"Hi"}}}}`, `push com.app {"aps":{"alert":{"title":"Hi"}}}`},
		{"logs", `{"app":"pl.app","level":"error","contains":"Failed","regex":"fail(ed)?","since":"c5"}`, "logs app=pl.app level=error contains=failed regex=fail(ed)? after=5"},
		{"logs", `{}`, "logs app= level= contains= regex= after=0"},
		{"tap", `{"contains":"Log"}`, "tap 25,25"}, // contains is the selector's, not a logs filter
	}
	for _, c := range cases {
		r := &recorder{}
		cl := connect(t, r)
		args := toolArgs(t, c.args)
		args["device"] = json.RawMessage(`"ios:y"`)
		res := callTool(cl, c.tool, args)
		if res["isError"] == true {
			t.Fatalf("%s %s: %s", c.tool, c.args, resultText(res))
		}
		if len(r.calls) != 1 || r.calls[0] != c.call {
			t.Fatalf("%s %s: backend calls %q, want %q", c.tool, c.args, r.calls, c.call)
		}
	}
}

func TestToolResults(t *testing.T) {
	cl := connect(t, &recorder{})
	res := callTool(cl, "clipboard", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`)})
	if res["isError"] == true || !strings.Contains(resultText(res), `"text":"copied"`) {
		t.Fatalf("clipboard read: %v", res)
	}
	res = callTool(cl, "appearance", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`), "mode": json.RawMessage(`"dark"`)})
	if res["isError"] != true || !strings.Contains(resultText(res), "appearance is not implemented on ios") {
		t.Fatalf("appearance on a backend without it: %v", res)
	}
	res = callTool(cl, "permission", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`), "app": json.RawMessage(`"com.app"`), "action": json.RawMessage(`"grant"`)})
	if res["isError"] != true || !strings.Contains(resultText(res), "needs at least one permission") {
		t.Fatalf("permission without names: %v", res)
	}

	// logs: text lines by default, with the cursor at the end; JSON on request.
	res = callTool(cl, "logs", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`), "level": json.RawMessage(`"warn"`)})
	out := resultText(res)
	if res["isError"] == true || !strings.Contains(out, " E AgemuTestApp[7] com.example:net: request failed\n") || strings.Contains(out, "GET /items") || !strings.Contains(out, "-- cursor c99") {
		t.Fatalf("logs text:\n%s", out)
	}
	res = callTool(cl, "logs", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`), "json": json.RawMessage(`true`)})
	var l api.Logs
	if err := json.Unmarshal([]byte(resultText(res)), &l); err != nil || len(l.Entries) != 2 || l.Cursor != "c99" {
		t.Fatalf("logs JSON: %v %s", err, resultText(res))
	}
	res = callTool(cl, "logs", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`), "level": json.RawMessage(`"loud"`)})
	if res["isError"] != true || !strings.Contains(resultText(res), `unknown log level "loud"`) {
		t.Fatalf("logs with a bad level: %v", res)
	}
	res = callTool(cl, "crashes", map[string]json.RawMessage{"device": json.RawMessage(`"ios:y"`)})
	if res["isError"] != true || resultText(res) != "crashes is not implemented on ios" {
		t.Fatalf("crashes on a backend without it: %v", res)
	}
}

// crashed is an iOS backend whose launched app crashes when the test says so.
type crashed struct {
	recorder
	crash *api.Crash
}

func (c *crashed) Launch(ctx context.Context, id, app string) error { return nil }
func (c *crashed) AppState(ctx context.Context, id, app string, since time.Time) (api.AppState, error) {
	if c.crash == nil || c.crash.Time.Before(since) {
		return api.AppState{Running: true}, nil
	}
	return api.AppState{Crash: c.crash}, nil
}

func TestCrashNoticeInToolResults(t *testing.T) {
	b := &crashed{}
	cl := connect(t, b)
	dev := json.RawMessage(`"ios:y"`)
	if res := callTool(cl, "launch", map[string]json.RawMessage{"device": dev, "app": json.RawMessage(`"pl.app"`)}); res["isError"] == true || strings.Contains(resultText(res), "NOTE") {
		t.Fatalf("launch: %v", res)
	}
	b.crash = &api.Crash{Time: time.Now(), App: "pl.app", Kind: api.CrashKindCrash, Exception: "EXC_BAD_ACCESS (SIGSEGV)"}
	res := callTool(cl, "ui_tree", map[string]json.RawMessage{"device": dev})
	content := res["content"].([]map[string]any)
	if res["isError"] == true || len(content) != 2 || !strings.HasPrefix(content[0]["text"].(string), "NOTE: pl.app crashed at") || !strings.Contains(content[1]["text"].(string), `button "Log in"`) {
		t.Fatalf("ui_tree after the crash: %v", res)
	}
	res = callTool(cl, "ui_tree", map[string]json.RawMessage{"device": dev})
	if strings.Contains(resultText(res), "NOTE:") {
		t.Fatalf("the notice repeated: %v", res)
	}
	res = callTool(cl, "tap", map[string]json.RawMessage{"device": dev, "text": json.RawMessage(`"Pay"`)})
	if res["isError"] != true || !strings.Contains(resultText(res), "no element matches") || !strings.Contains(resultText(res), "pl.app crashed at") {
		t.Fatalf("a failed tap while the app is down: %v", res)
	}
}
