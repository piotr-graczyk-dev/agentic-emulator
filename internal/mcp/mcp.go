// Package mcp exposes the agemu daemon as a Model Context Protocol server over
// stdio (newline-delimited JSON-RPC 2.0), so agent harnesses can drive devices
// with tool calls. Every tool call is forwarded to the shared daemon.
package mcp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/outline"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	method      string
	// selector: the text/contains/id/role/index arguments form a selector (find, tap, wait_for).
	// In other tools "text" is plain text (type_text, clipboard).
	selector bool
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

var (
	str    = func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num    = func(d string) map[string]any { return map[string]any{"type": "number", "description": d} }
	boolT  = func(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }
	device = str("Device id or name (optional when exactly one device is booted)")
)

// waitProps are the optional "wait for the next screen in the same call" arguments of actions.
var waitProps = map[string]any{
	"waitFor": map[string]any{"type": "object", "description": "After the action, wait for an element matching this selector (fields: text, contains, id, role) in the same call", "properties": map[string]any{
		"text": str("Exact text or label"), "contains": str("Substring of text or label"), "id": str("Identifier"), "role": str("Role"),
	}},
	"waitGone":      boolT("Wait for waitFor to disappear instead"),
	"waitTimeoutMs": num("Timeout for waitFor, default 10000"),
	"returnUi":      boolT("Also return the UI outline of the screen the action ends on (act, wait and observe in one call)"),
}

func enum(d string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": d}
}

func withWait(props map[string]any) map[string]any {
	for k, v := range waitProps {
		props[k] = v
	}
	return props
}

func selectorProps(extra map[string]any) map[string]any {
	m := map[string]any{
		"device":   device,
		"text":     str("Exact text or accessibility label"),
		"contains": str("Case-insensitive substring of text or label"),
		"id":       str("resource-id (Android) or accessibilityIdentifier (iOS)"),
		"role":     str("Normalized role: button, text, textfield, switch, image, cell..."),
		"index":    num("Pick the n-th match (0-based)"),
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var tools = []tool{
	{Name: "devices", method: "devices", Description: "List Android and iOS devices with state, profile and screen size.", InputSchema: obj(map[string]any{})},
	{Name: "profiles", method: "profiles", Description: "List screen profiles (real device sizes) usable with set_profile.", InputSchema: obj(map[string]any{"platform": str("android or ios")})},
	{Name: "create", method: "create", Description: "Create a new device.", InputSchema: obj(map[string]any{"platform": str("android or ios"), "name": str("Device name"), "profile": str("Profile name")}, "platform", "name")},
	{Name: "boot", method: "boot", Description: "Boot a device to an agent-ready state (warm snapshot, seconds).", InputSchema: obj(map[string]any{"device": device, "cold": boolT("Force a cold boot")})},
	{Name: "shutdown", method: "shutdown", Description: "Shut a device down.", InputSchema: obj(map[string]any{"device": device})},
	{Name: "reset", method: "reset", Description: "Restore a booted device to its clean baseline state (fast).", InputSchema: obj(map[string]any{"device": device})},
	{Name: "set_profile", method: "set_profile", Description: "Switch the screen to a real-device profile (see profiles) or a custom Android size.", InputSchema: obj(map[string]any{"device": device, "profile": str("Profile name"), "width": num("Custom width px (Android)"), "height": num("Custom height px (Android)"), "density": num("Custom density dpi (Android)")})},
	{Name: "screenshot", method: "screenshot", Description: "Capture the screen. Returns the image plus its file path. Coordinates on a scale=1 image are tap coordinates.", InputSchema: obj(map[string]any{"device": device, "scale": num("Downscale factor 0-1 for the returned image (default 0.5)"), "format": str("png or jpeg")})},
	{Name: "ui_tree", method: "ui", Description: "Accessibility/UI hierarchy as a compact outline: [ref] role \"text\" id=... @(x,y wxh). Frames are screenshot pixels.", InputSchema: obj(map[string]any{"device": device, "json": boolT("Return full JSON instead of the outline")})},
	{Name: "find", method: "find", selector: true, Description: "Find elements by text/label/id/role.", InputSchema: obj(selectorProps(nil))},
	{Name: "tap", method: "tap", selector: true, Description: "Tap at x,y (screenshot pixels) or at the center of the element matching a selector. With waitFor it also waits for the next screen.", InputSchema: obj(withWait(selectorProps(map[string]any{"x": num("X in screenshot pixels"), "y": num("Y in screenshot pixels")})))},
	{Name: "swipe", method: "swipe", Description: "Swipe from (x,y) to (x2,y2) in screenshot pixels.", InputSchema: obj(withWait(map[string]any{"device": device, "x": num("start x"), "y": num("start y"), "x2": num("end x"), "y2": num("end y"), "durationMs": num("duration, default 300")}), "x", "y", "x2", "y2")},
	{Name: "type_text", method: "type", Description: "Type text into the focused field.", InputSchema: obj(withWait(map[string]any{"device": device, "text": str("Text to type"), "keys": boolT("Send one key event per character (slower) instead of setting the field's text (Android) or pasting it (iOS)")}), "text")},
	{Name: "press_key", method: "key", Description: "Press a key: home, back, enter, delete, tab, escape, lock, volumeup, volumedown.", InputSchema: obj(withWait(map[string]any{"device": device, "key": str("Key name")}), "key")},
	{Name: "install", method: "install", Description: "Install an app (.apk for Android, .app for iOS Simulator).", InputSchema: obj(map[string]any{"device": device, "path": str("Absolute path")}, "path")},
	{Name: "launch", method: "launch", Description: "Launch an app by package name / bundle id. iOS also takes process arguments (e.g. an Expo dev client's --initialUrl), environment and relaunch.", InputSchema: obj(withWait(map[string]any{
		"device":   device,
		"app":      str("Package name or bundle id"),
		"args":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Process arguments (iOS)"},
		"env":      map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Process environment (iOS)"},
		"relaunch": boolT("Terminate a running instance first (iOS)"),
	}), "app")},
	{Name: "terminate", method: "terminate", Description: "Force-stop an app.", InputSchema: obj(map[string]any{"device": device, "app": str("Package name or bundle id")}, "app")},
	{Name: "wait_for", method: "wait", selector: true, Description: "Wait until an element matching the selector appears (or disappears with gone=true).", InputSchema: obj(selectorProps(map[string]any{"gone": boolT("Wait for disappearance"), "timeoutMs": num("Timeout, default 10000")}))},

	// Test capabilities (docs/CAPABILITIES.md). A capability a platform lacks answers an error
	// saying "... is not implemented on <platform>".
	{Name: "open_url", method: "open_url", Description: "Open a URL or deep link as a link tapped in another app would (Android: VIEW intent with CATEGORY_BROWSABLE). App/universal links open the app only if their domain is verified. With waitFor it also waits for the next screen.", InputSchema: obj(withWait(map[string]any{
		"device": device,
		"url":    str("URL with a scheme: https://..., myapp://path, exp+app://..."),
	}), "url")},
	{Name: "permission", method: "permission", Description: "Grant, revoke or reset an app's privacy permissions without the system dialog. revoke = denied without asking again; reset = ask again on next use. Android kills the app when one is revoked or reset. A grant the app could not get on a device (not in its manifest / no Info.plist usage description) is refused.", InputSchema: obj(map[string]any{
		"device":      device,
		"app":         str("Package name or bundle id"),
		"action":      enum("What to do", "grant", "revoke", "reset"),
		"permissions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Names: " + permissionNames() + "; or a raw Android permission (android.permission.READ_SMS). Not every name exists on both platforms. Empty with reset: all of the app's permissions."},
	}, "app", "action")},
	{Name: "location", method: "location", Description: "Set the simulated GPS location, or clear it (back to the device's baseline).", InputSchema: obj(map[string]any{
		"device":    device,
		"action":    enum("set (default when coordinates are given) or clear", "set", "clear"),
		"latitude":  num("Degrees, -90..90"),
		"longitude": num("Degrees, -180..180"),
	})},
	{Name: "clipboard", method: "clipboard", Description: "Read the device clipboard (plain text), or set it.", InputSchema: obj(map[string]any{
		"device": device,
		"action": enum("get (default) or set (default when text is given)", "get", "set"),
		"text":   str("Text to put on the clipboard (action set; empty clears it)"),
	})},
	{Name: "appearance", method: "appearance", Description: "Read the system appearance, or switch light/dark mode.", InputSchema: obj(map[string]any{
		"device": device,
		"mode":   enum("Omit to read the current appearance", "light", "dark"),
	})},
	{Name: "apps", method: "apps", Description: "List installed user apps (not the system image's).", InputSchema: obj(map[string]any{"device": device})},
	{Name: "uninstall", method: "uninstall", Description: "Uninstall a user app with its data.", InputSchema: obj(map[string]any{"device": device, "app": str("Package name or bundle id")}, "app")},
	{Name: "clear_app_data", method: "clear_app_data", Description: "Clear one app's data so it starts like a fresh install (stopped, data and permissions gone; iOS keeps keychain items, as a reinstall does on a device).", InputSchema: obj(map[string]any{"device": device, "app": str("Package name or bundle id")}, "app")},
	{Name: "push", method: "push", Description: "Send a push notification to an app (iOS; Android has no faithful path). It shows only if the app may post notifications. With waitFor it also waits for the banner or screen.", InputSchema: obj(withWait(map[string]any{
		"device":  device,
		"app":     str("Bundle id"),
		"payload": map[string]any{"type": "object", "description": "APNs payload with an \"aps\" object, at most 4096 bytes, e.g. {\"aps\":{\"alert\":{\"title\":\"Hi\",\"body\":\"Hello\"}}}"},
	}), "app", "payload")},
}

// permissionNames lists the platform-neutral permission names for tool descriptions.
func permissionNames() string {
	names := make([]string, len(api.Permissions))
	for i, p := range api.Permissions {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

// Serve runs the MCP server until in is closed.
func Serve(in io.Reader, out io.Writer) error {
	var cl *client.Client
	var clErr error
	var once sync.Once
	getClient := func() (*client.Client, error) {
		once.Do(func() { cl, clErr = client.Dial() })
		return cl, clErr
	}
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriter(out)
	write := func(resp rpcResp) {
		resp.JSONRPC = "2.0"
		b, _ := json.Marshal(resp)
		w.Write(b)
		w.WriteByte('\n')
		w.Flush()
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var req rpcReq
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				write(rpcResp{ID: json.RawMessage("null"), Error: &rpcErr{Code: -32700, Message: jerr.Error()}})
			} else if len(req.ID) > 0 { // notifications (no id) get no response
				res, rerr := handle(req, getClient)
				if rerr != nil {
					write(rpcResp{ID: req.ID, Error: rerr})
				} else {
					write(rpcResp{ID: req.ID, Result: res})
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func handle(req rpcReq, getClient func() (*client.Client, error)) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agemu", "version": "0.1.0"},
			"instructions":    "Drive Android emulators and iOS Simulators. Typical loop: devices -> boot -> launch -> ui_tree -> tap/type_text -> wait_for -> screenshot. Coordinates are screenshot pixels. Test setup: permission, location, appearance, clipboard, open_url, push, clear_app_data; reset restores appearance, location, clipboard and permissions.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{Code: -32602, Message: err.Error()}
		}
		cl, err := getClient()
		if err != nil {
			return toolError(err), nil
		}
		return callTool(cl, p.Name, p.Arguments), nil
	}
	return nil, &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
}

func toolError(err error) map[string]any {
	return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}
}

func text(s string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}

func callTool(cl *client.Client, name string, args map[string]json.RawMessage) map[string]any {
	var t *tool
	for i := range tools {
		if tools[i].Name == name {
			t = &tools[i]
		}
	}
	if t == nil {
		return toolError(fmt.Errorf("unknown tool %q", name))
	}
	// Tool arguments use the daemon's field names; selector fields are folded into a selector.
	b, _ := json.Marshal(args)
	var p daemon.Params
	if err := json.Unmarshal(b, &p); err != nil {
		return toolError(err)
	}
	if t.selector {
		var sel api.Selector
		_ = json.Unmarshal(b, &sel)
		if !sel.Empty() {
			p.Selector = &sel
			p.Text = "" // "text" was a selector field, not text to type
		}
	}
	wantJSON := false
	if v, ok := args["json"]; ok {
		_ = json.Unmarshal(v, &wantJSON)
	}
	scale := p.Scale
	if name == "screenshot" {
		// Default to half resolution to keep image tokens low; the text part says how to map back.
		if scale <= 0 || scale > 1 {
			scale = 0.5
		}
		p.Scale = scale
	}

	raw, ms, err := cl.CallRaw(t.method, p)
	if err != nil {
		return toolError(err)
	}
	switch name {
	case "ui_tree":
		if wantJSON {
			return text(string(raw))
		}
		var tr api.UITree
		if err := json.Unmarshal(raw, &tr); err != nil {
			return toolError(err)
		}
		return text(outline.Render(&tr))
	case "find":
		var els []*api.Element
		_ = json.Unmarshal(raw, &els)
		var sb strings.Builder
		for _, e := range els {
			sb.WriteString(outline.Line(e))
			sb.WriteByte('\n')
		}
		if len(els) == 0 {
			sb.WriteString("no match")
		}
		return text(sb.String())
	case "screenshot":
		var s api.Screenshot
		if err := json.Unmarshal(raw, &s); err != nil {
			return toolError(err)
		}
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return toolError(err)
		}
		mime := "image/png"
		if s.Format == "jpeg" {
			mime = "image/jpeg"
		}
		return map[string]any{"content": []map[string]any{
			{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": mime},
			{"type": "text", "text": fmt.Sprintf("%s %dx%d (scale %.2f: multiply image coordinates by %.2f to get tap coordinates) in %.0fms", s.Path, s.Width, s.Height, scale, 1/scale, ms)},
		}}
	}
	if p.ReturnUI {
		var r struct {
			Result json.RawMessage `json:"result"`
			Waited json.RawMessage `json:"waited,omitempty"`
			UI     *api.UITree     `json:"ui"`
		}
		if err := json.Unmarshal(raw, &r); err == nil && r.UI != nil {
			head := map[string]json.RawMessage{"result": r.Result}
			if len(r.Waited) > 0 {
				head["waited"] = r.Waited
			}
			b, _ := json.Marshal(head)
			return text(fmt.Sprintf("%s (%.0fms)\n%s", b, ms, outline.Render(r.UI)))
		}
	}
	return text(fmt.Sprintf("%s (%.0fms)", string(raw), ms))
}
