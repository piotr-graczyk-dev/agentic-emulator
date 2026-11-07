package daemon

// Test capabilities: open a URL, permissions, location, clipboard, appearance, app data and
// push (docs/CAPABILITIES.md). Each daemon method has its arguments checked and normalized by
// checkCapability before the device lock is taken, then runs under the device lock (reads
// too: a clipboard read must not see the pasteboard while iOS types through it) and calls an
// optional backend interface. A backend without that interface gets api.NotImplemented.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

type capability func(ctx context.Context, b backend.Backend, id string, p Params) (any, error)

// capabilities maps each capability method to its handler. The method names are also the MCP
// tool names.
var capabilities = map[string]capability{
	"open_url":       openURL,
	"permission":     permission,
	"location":       location,
	"clipboard":      clipboard,
	"appearance":     appearance,
	"apps":           apps,
	"uninstall":      uninstall,
	"clear_app_data": clearAppData,
	"push":           push,
}

// noFaithfulPath gives the reason a capability stays unimplemented on a platform by design, so
// the error tells the agent not to wait for it.
var noFaithfulPath = map[string]map[api.Platform]string{
	"push": {api.Android: "no faithful path (FCM delivery needs the app's server credentials, and the shell may not send the c2dm RECEIVE broadcast)"},
}

// missing is the error for a backend without the capability's interface.
func missing(b backend.Backend, method string) error {
	return api.NotImplemented(b.Platform(), method, noFaithfulPath[method][b.Platform()])
}

// checkCapability validates and normalizes the arguments of a capability method in place. It
// returns nil for other methods.
func checkCapability(method string, p *Params) error {
	switch method {
	case "open_url":
		p.URL = strings.TrimSpace(p.URL)
		if p.URL == "" {
			return errors.New("open_url needs a url")
		}
		u, err := url.Parse(p.URL)
		if err != nil {
			return fmt.Errorf("open_url: bad url: %v", err)
		}
		if u.Scheme == "" {
			return fmt.Errorf("open_url: %q has no scheme (e.g. https://example.com/path or myapp://screen)", p.URL)
		}
	case "permission":
		if p.App == "" {
			return errors.New("permission needs an app (package name or bundle id)")
		}
		act := api.PermissionAction(strings.ToLower(strings.TrimSpace(p.Action)))
		switch act {
		case api.PermissionGrant, api.PermissionRevoke, api.PermissionReset:
		default:
			return fmt.Errorf("permission action must be grant, revoke or reset, got %q", p.Action)
		}
		p.Action = string(act)
		names, err := permissionNames(p.Permissions)
		if err != nil {
			return err
		}
		if len(names) == 0 && act != api.PermissionReset {
			return fmt.Errorf("permission %s needs at least one permission: %s", act, knownPermissions())
		}
		p.Permissions = names
	case "location":
		act := strings.ToLower(strings.TrimSpace(p.Action))
		if act == "" && (p.Latitude != nil || p.Longitude != nil) {
			act = "set"
		}
		switch act {
		case "set":
			if p.Latitude == nil || p.Longitude == nil {
				return errors.New("location set needs latitude and longitude")
			}
			if lat := *p.Latitude; math.IsNaN(lat) || lat < -90 || lat > 90 {
				return fmt.Errorf("latitude must be between -90 and 90, got %v", lat)
			}
			if lon := *p.Longitude; math.IsNaN(lon) || lon < -180 || lon > 180 {
				return fmt.Errorf("longitude must be between -180 and 180, got %v", lon)
			}
		case "clear":
			if p.Latitude != nil || p.Longitude != nil {
				return errors.New("location clear takes no coordinates")
			}
		default:
			return fmt.Errorf("location action must be set (with latitude and longitude) or clear, got %q", p.Action)
		}
		p.Action = act
	case "clipboard":
		act := strings.ToLower(strings.TrimSpace(p.Action))
		if act == "" {
			act = "get"
			if p.Text != "" {
				act = "set"
			}
		}
		switch act {
		case "get":
			if p.Text != "" {
				return errors.New("clipboard get takes no text; use action set to write it")
			}
		case "set": // "" empties the clipboard
		default:
			return fmt.Errorf("clipboard action must be get or set, got %q", p.Action)
		}
		p.Action = act
	case "appearance":
		mode := api.Appearance(strings.ToLower(strings.TrimSpace(p.Mode)))
		switch mode {
		case "", api.AppearanceLight, api.AppearanceDark:
		default:
			return fmt.Errorf("appearance mode must be light or dark (or empty to read it), got %q", p.Mode)
		}
		p.Mode = string(mode)
	case "uninstall", "clear_app_data":
		if p.App == "" {
			return fmt.Errorf("%s needs an app (package name or bundle id)", method)
		}
	case "push":
		if p.App == "" {
			return errors.New("push needs an app (bundle id)")
		}
		payload, err := pushPayload(p.Payload)
		if err != nil {
			return err
		}
		p.Payload = payload
	}
	return nil
}

// permissionNames splits comma-separated names, lowercases neutral names, checks every name
// and drops duplicates, keeping the order.
func permissionNames(in []string) ([]string, error) {
	var out []string
	seen := map[api.Permission]bool{}
	for _, item := range in {
		for _, name := range strings.Split(item, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			perm := api.Permission(name)
			if !perm.Raw() {
				perm = api.Permission(strings.ToLower(name))
			}
			if !perm.Known() {
				return nil, fmt.Errorf("unknown permission %q: %s", name, knownPermissions())
			}
			if !seen[perm] {
				seen[perm] = true
				out = append(out, string(perm))
			}
		}
	}
	return out, nil
}

func knownPermissions() string {
	names := make([]string, len(api.Permissions))
	for i, p := range api.Permissions {
		names[i] = string(p)
	}
	return "use " + strings.Join(names, ", ") + ", or a raw Android permission such as android.permission.READ_SMS"
}

// pushPayload checks a push payload and returns it as compact JSON. A JSON string holding the
// object is accepted too (agents sometimes quote JSON arguments).
func pushPayload(raw json.RawMessage) (json.RawMessage, error) {
	example := `e.g. {"aps":{"alert":{"title":"Hi","body":"Hello"}}}`
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("push needs a payload: a JSON object with an \"aps\" object, " + example)
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("push payload: %v", err)
		}
		raw = bytes.TrimSpace([]byte(s))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("push payload must be a JSON object (%v), %s", err, example)
	}
	var aps map[string]json.RawMessage
	if err := json.Unmarshal(obj["aps"], &aps); err != nil || aps == nil {
		return nil, errors.New("push payload needs an \"aps\" object, " + example)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("push payload: %v", err)
	}
	if buf.Len() > api.MaxPushPayload {
		return nil, fmt.Errorf("push payload is %d bytes, more than the %d-byte limit", buf.Len(), api.MaxPushPayload)
	}
	return buf.Bytes(), nil
}

func openURL(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	o, has := b.(backend.URLOpener)
	if !has {
		return nil, missing(b, "open_url")
	}
	app, err := o.OpenURL(ctx, id, p.URL)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"ok": true}
	if app != "" {
		res["app"] = app
	}
	return res, nil
}

func permission(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	ps, has := b.(backend.PermissionSetter)
	if !has {
		return nil, missing(b, "permission")
	}
	perms := make([]api.Permission, len(p.Permissions))
	for i, n := range p.Permissions {
		perms[i] = api.Permission(n)
	}
	return ok(ps.SetPermissions(ctx, id, p.App, api.PermissionAction(p.Action), perms))
}

func location(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	ls, has := b.(backend.LocationSetter)
	if !has {
		return nil, missing(b, "location")
	}
	if p.Action == "clear" {
		return ok(ls.ClearLocation(ctx, id))
	}
	return ok(ls.SetLocation(ctx, id, *p.Latitude, *p.Longitude))
}

func clipboard(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	ca, has := b.(backend.ClipboardAccessor)
	if !has {
		return nil, missing(b, "clipboard")
	}
	if p.Action == "set" {
		return ok(ca.SetClipboard(ctx, id, p.Text))
	}
	text, err := ca.Clipboard(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": text}, nil
}

func appearance(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	as, has := b.(backend.AppearanceSetter)
	if !has {
		return nil, missing(b, "appearance")
	}
	if p.Mode != "" {
		if err := as.SetAppearance(ctx, id, api.Appearance(p.Mode)); err != nil {
			return nil, err
		}
		return map[string]any{"appearance": p.Mode}, nil
	}
	a, err := as.Appearance(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"appearance": a}, nil
}

func apps(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	am, has := b.(backend.AppManager)
	if !has {
		return nil, missing(b, "apps")
	}
	list, err := am.Apps(ctx, id)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []api.App{}
	}
	return list, nil
}

func uninstall(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	am, has := b.(backend.AppManager)
	if !has {
		return nil, missing(b, "uninstall")
	}
	return ok(am.Uninstall(ctx, id, p.App))
}

func clearAppData(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	am, has := b.(backend.AppManager)
	if !has {
		return nil, missing(b, "clear_app_data")
	}
	return ok(am.ClearAppData(ctx, id, p.App))
}

func push(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	ps, has := b.(backend.PushSender)
	if !has {
		return nil, missing(b, "push")
	}
	return ok(ps.Push(ctx, id, p.App, p.Payload))
}
