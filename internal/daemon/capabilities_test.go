package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// capBackend implements every capability interface and records each call as a string.
type capBackend struct {
	fakeBackend
	plat  api.Platform
	mu    sync.Mutex
	calls []string
	// When block is set, OpenURL signals entered and waits for block to close.
	block   chan struct{}
	entered chan struct{}
}

var (
	_ backend.URLOpener         = (*capBackend)(nil)
	_ backend.PermissionSetter  = (*capBackend)(nil)
	_ backend.LocationSetter    = (*capBackend)(nil)
	_ backend.ClipboardAccessor = (*capBackend)(nil)
	_ backend.AppearanceSetter  = (*capBackend)(nil)
	_ backend.AppManager        = (*capBackend)(nil)
	_ backend.PushSender        = (*capBackend)(nil)
)

func (c *capBackend) Platform() api.Platform {
	if c.plat != "" {
		return c.plat
	}
	return api.Android
}

func (c *capBackend) record(format string, args ...any) {
	c.mu.Lock()
	c.calls = append(c.calls, fmt.Sprintf(format, args...))
	c.mu.Unlock()
}

func (c *capBackend) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *capBackend) OpenURL(ctx context.Context, id, url string) (string, error) {
	if c.block != nil {
		c.entered <- struct{}{}
		<-c.block
	}
	c.record("open %s %s", id, url)
	return "com.app", nil
}

func (c *capBackend) SetPermissions(ctx context.Context, id, app string, action api.PermissionAction, perms []api.Permission) error {
	for _, p := range perms {
		if p == api.PermSiri {
			return api.NotImplemented(c.Platform(), `permission "siri"`, "no such permission")
		}
	}
	c.record("perm %s %s %v", app, action, perms)
	return nil
}

func (c *capBackend) SetLocation(ctx context.Context, id string, lat, lon float64) error {
	c.record("loc %v,%v", lat, lon)
	return nil
}

func (c *capBackend) ClearLocation(ctx context.Context, id string) error {
	c.record("clearloc")
	return nil
}

func (c *capBackend) Clipboard(ctx context.Context, id string) (string, error) {
	c.record("getclip %s", id)
	return "copied", nil
}

func (c *capBackend) SetClipboard(ctx context.Context, id, text string) error {
	c.record("setclip %q", text)
	return nil
}

func (c *capBackend) Appearance(ctx context.Context, id string) (api.Appearance, error) {
	c.record("getmode")
	return api.AppearanceLight, nil
}

func (c *capBackend) SetAppearance(ctx context.Context, id string, a api.Appearance) error {
	c.record("setmode %s", a)
	return nil
}

func (c *capBackend) Apps(ctx context.Context, id string) ([]api.App, error) {
	c.record("apps")
	return nil, nil
}

func (c *capBackend) Uninstall(ctx context.Context, id, app string) error {
	c.record("uninstall %s", app)
	return nil
}

func (c *capBackend) ClearAppData(ctx context.Context, id, app string) error {
	c.record("clear %s", app)
	return nil
}

func (c *capBackend) Push(ctx context.Context, id, app string, payload []byte) error {
	c.record("push %s %s", app, payload)
	return nil
}

func call(t *testing.T, s *Service, method, params string) (any, error) {
	t.Helper()
	return s.Call(context.Background(), method, json.RawMessage(params))
}

// withDevice adds "device":dev to a JSON object.
func withDevice(dev, params string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(params), "{"))
	if rest == "}" {
		return `{"device":"` + dev + `"}`
	}
	return `{"device":"` + dev + `",` + rest
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCapabilitiesRouteToTheBackend(t *testing.T) {
	cases := []struct {
		method, params string
		call           string // what the backend got
		result         string // the daemon's result as JSON
	}{
		{"open_url", `{"url":" myapp://item/7 "}`, "open android:x myapp://item/7", `{"app":"com.app","ok":true}`},
		{"permission", `{"app":"com.app","action":"GRANT","permissions":["Camera","microphone, photos","camera"]}`, "perm com.app grant [camera microphone photos]", `{"ok":true}`},
		{"permission", `{"app":"com.app","action":"revoke","permissions":["android.permission.READ_SMS"]}`, "perm com.app revoke [android.permission.READ_SMS]", `{"ok":true}`},
		{"permission", `{"app":"com.app","action":"reset"}`, "perm com.app reset []", `{"ok":true}`},
		{"location", `{"latitude":52.2297,"longitude":21.0122}`, "loc 52.2297,21.0122", `{"ok":true}`},
		{"location", `{"action":"set","latitude":0,"longitude":0}`, "loc 0,0", `{"ok":true}`},
		{"location", `{"action":"clear"}`, "clearloc", `{"ok":true}`},
		{"clipboard", `{}`, "getclip android:x", `{"text":"copied"}`},
		{"clipboard", `{"text":"otp 1234"}`, `setclip "otp 1234"`, `{"ok":true}`},
		{"clipboard", `{"action":"set"}`, `setclip ""`, `{"ok":true}`},
		{"appearance", `{}`, "getmode", `{"appearance":"light"}`},
		{"appearance", `{"mode":"Dark"}`, "setmode dark", `{"appearance":"dark"}`},
		{"apps", `{}`, "apps", `[]`},
		{"uninstall", `{"app":"com.app"}`, "uninstall com.app", `{"ok":true}`},
		{"clear_app_data", `{"app":"com.app"}`, "clear com.app", `{"ok":true}`},
		{"push", `{"app":"com.app","payload":{ "aps": {"alert": "hi"} }}`, `push com.app {"aps":{"alert":"hi"}}`, `{"ok":true}`},
		{"push", `{"app":"com.app","payload":"{\"aps\":{\"badge\":1}}"}`, `push com.app {"aps":{"badge":1}}`, `{"ok":true}`},
	}
	for _, c := range cases {
		cb := &capBackend{}
		svc := NewService(t.TempDir(), cb)
		res, err := call(t, svc, c.method, withDevice("android:x", c.params))
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.params, err)
		}
		if got := cb.recorded(); len(got) != 1 || got[0] != c.call {
			t.Fatalf("%s %s: backend calls %q, want %q", c.method, c.params, got, c.call)
		}
		if got := asJSON(res); got != c.result {
			t.Fatalf("%s %s: result %s, want %s", c.method, c.params, got, c.result)
		}
	}
}

func TestCapabilitiesValidateArgumentsBeforeTheDeviceLock(t *testing.T) {
	big := `{"aps":{"alert":"` + strings.Repeat("x", api.MaxPushPayload) + `"}}`
	cases := []struct{ method, params, want string }{
		{"open_url", `{}`, "needs a url"},
		{"open_url", `{"url":"example.com/path"}`, "has no scheme"},
		{"open_url", `{"url":"http://[::1"}`, "bad url"},
		{"permission", `{"action":"grant","permissions":["camera"]}`, "needs an app"},
		{"permission", `{"app":"a","action":"allow","permissions":["camera"]}`, "grant, revoke or reset"},
		{"permission", `{"app":"a","action":"grant"}`, "needs at least one permission"},
		{"permission", `{"app":"a","action":"revoke","permissions":[" , "]}`, "needs at least one permission"},
		{"permission", `{"app":"a","action":"grant","permissions":["camra"]}`, `unknown permission "camra"`},
		{"permission", `{"app":"a","action":"grant","permissions":["android.permission.X Y"]}`, "unknown permission"},
		{"location", `{}`, "set (with latitude and longitude) or clear"},
		{"location", `{"action":"teleport"}`, "set (with latitude and longitude) or clear"},
		{"location", `{"action":"set","latitude":1}`, "needs latitude and longitude"},
		{"location", `{"latitude":91,"longitude":0}`, "latitude must be between -90 and 90"},
		{"location", `{"latitude":0,"longitude":-180.5}`, "longitude must be between -180 and 180"},
		{"location", `{"action":"clear","latitude":1,"longitude":2}`, "takes no coordinates"},
		{"clipboard", `{"action":"get","text":"x"}`, "takes no text"},
		{"clipboard", `{"action":"copy"}`, "get or set"},
		{"appearance", `{"mode":"sepia"}`, "light or dark"},
		{"uninstall", `{}`, "uninstall needs an app"},
		{"clear_app_data", `{}`, "clear_app_data needs an app"},
		{"push", `{"payload":{"aps":{}}}`, "push needs an app"},
		{"push", `{"app":"a"}`, "needs a payload"},
		{"push", `{"app":"a","payload":null}`, "needs a payload"},
		{"push", `{"app":"a","payload":[1]}`, "must be a JSON object"},
		{"push", `{"app":"a","payload":"not json"}`, "must be a JSON object"},
		{"push", `{"app":"a","payload":{"alert":"x"}}`, `needs an "aps" object`},
		{"push", `{"app":"a","payload":{"aps":"x"}}`, `needs an "aps" object`},
		{"push", `{"app":"a","payload":` + big + `}`, "byte limit"},
	}
	cb := &capBackend{}
	svc := NewService(t.TempDir(), cb)
	// Another agent holds the device: a bad call must still fail at once.
	l := svc.deviceLock("android:x")
	l.Lock()
	defer l.Unlock()
	for _, c := range cases {
		p := withDevice("android:x", c.params)
		done := make(chan error, 1)
		go func() {
			_, err := call(t, svc, c.method, p)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s %s: err = %v, want %q", c.method, c.params, err, c.want)
			}
			if errors.Is(err, api.ErrNotImplemented) {
				t.Fatalf("%s %s: an argument error must not read as not implemented: %v", c.method, c.params, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s %s: blocked on the device lock instead of failing", c.method, c.params)
		}
	}
	if got := cb.recorded(); len(got) != 0 {
		t.Fatalf("the backend was called: %q", got)
	}
}

// validCalls has one valid call per capability method.
var validCalls = map[string]string{
	"open_url":       `{"url":"https://example.com/a"}`,
	"permission":     `{"app":"com.app","action":"grant","permissions":["camera"]}`,
	"location":       `{"latitude":1,"longitude":2}`,
	"clipboard":      `{}`,
	"appearance":     `{"mode":"dark"}`,
	"apps":           `{}`,
	"uninstall":      `{"app":"com.app"}`,
	"clear_app_data": `{"app":"com.app"}`,
	"push":           `{"app":"com.app","payload":{"aps":{"alert":"hi"}}}`,
}

// iosFake is a backend for the ios platform with no capability interfaces.
type iosFake struct{ fakeBackend }

func (iosFake) Platform() api.Platform { return api.IOS }

func TestCapabilitiesAreNotImplementedWithoutTheirInterface(t *testing.T) {
	if len(validCalls) != len(capabilities) {
		t.Fatalf("validCalls covers %d methods, capabilities has %d", len(validCalls), len(capabilities))
	}
	svc := NewService(t.TempDir(), &fakeBackend{}, &iosFake{})
	for method, params := range validCalls {
		for _, dev := range []string{"android:x", "ios:y"} {
			plat, _, _ := strings.Cut(dev, ":")
			_, err := call(t, svc, method, withDevice(dev, params))
			if !errors.Is(err, api.ErrNotImplemented) {
				t.Fatalf("%s on %s: err = %v, want not implemented", method, dev, err)
			}
			want := method + " is not implemented on " + plat
			if !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("%s on %s: err = %q, want it to start with %q", method, dev, err, want)
			}
			if faithless := method == "push" && plat == "android"; faithless != strings.Contains(err.Error(), "no faithful path") {
				t.Fatalf("%s on %s: err = %q; only android push says there is no faithful path", method, dev, err)
			}
		}
	}
}

func TestBackendNotImplementedPassesThrough(t *testing.T) {
	svc := NewService(t.TempDir(), &capBackend{plat: api.IOS})
	_, err := call(t, svc, "permission", `{"device":"ios:y","app":"com.app","action":"grant","permissions":["camera","siri"]}`)
	if !errors.Is(err, api.ErrNotImplemented) || !strings.Contains(err.Error(), `permission "siri" is not implemented on ios`) {
		t.Fatalf("err = %v", err)
	}
}

func TestCapabilitiesHoldTheDeviceLock(t *testing.T) {
	cb := &capBackend{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	svc := NewService(t.TempDir(), cb)
	opened := make(chan error, 1)
	go func() {
		_, err := call(t, svc, "open_url", `{"device":"android:x","url":"myapp://a"}`)
		opened <- err
	}()
	<-cb.entered // open_url holds android:x

	read := make(chan error, 1)
	go func() {
		_, err := call(t, svc, "clipboard", `{"device":"android:x"}`)
		read <- err
	}()
	// Another device is not held up.
	if _, err := call(t, svc, "clipboard", `{"device":"android:other"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		t.Fatalf("a clipboard read ran while open_url held the device (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(cb.block)
	for _, ch := range []chan error{opened, read} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a call did not finish once the device was free")
		}
	}
	want := []string{"getclip android:other", "open android:x myapp://a", "getclip android:x"}
	if got := cb.recorded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestOpenURLWaitsForItsEffectInTheSameCall(t *testing.T) {
	svc := NewService(t.TempDir(), &capBackend{})
	res, err := call(t, svc, "open_url", `{"device":"android:x","url":"myapp://a","waitFor":{"contains":"welcome"},"returnUi":true}`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := res.(map[string]any)
	if el, _ := m["waited"].(*api.Element); el == nil || el.Ref != "e2" || m["ui"] == nil || m["result"] == nil {
		t.Fatalf("result = %#v, want result, waited (e2) and ui", res)
	}
}

func TestNotImplementedErrorsCarryACode(t *testing.T) {
	if errorCode(api.NotImplemented(api.Android, "push", "")) != CodeNotImplemented {
		t.Fatal("not implemented without a code")
	}
	if errorCode(fmt.Errorf("wrapped: %w", api.NotImplemented(api.IOS, "x", ""))) != CodeNotImplemented {
		t.Fatal("wrapped not implemented without a code")
	}
	if errorCode(errors.New("push needs an app")) != "" {
		t.Fatal("an argument error got a code")
	}
}
