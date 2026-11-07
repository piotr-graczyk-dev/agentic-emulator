// Command agemu is the CLI and MCP server of the agent-first Android emulator / iOS
// Simulator runtime for macOS. It is a thin client: every command is one request to the
// agemud daemon, which it starts on demand. Keeping the backends (gRPC, CoreSimulator
// helper) out of this binary keeps its start-up time low, which matters because agents
// run one process per action.
//
//	agemu daemon                 run agemud in the foreground
//	agemu mcp                    serve the MCP protocol on stdio
//	agemu <command> [flags]      talk to the daemon; prints JSON (ui/find print a text outline)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/mcp"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/outline"
)

const usage = `agemu - agent-first Android emulator & iOS Simulator runtime (macOS)

Device commands (-d <device> optional when exactly one device is booted):
  devices                                   list devices
  profiles [--platform android|ios]         list screen profiles
  create --platform android|ios --name N [--profile P]
  boot <device> [--cold]                    boot (warm snapshot by default)
  shutdown | reset                          stop / restore clean baseline
  profile <name> | --width W --height H --density D
  screenshot [-o file.png] [--scale 0.5] [--format png|jpeg]
  ui [--json]                               UI tree (text outline by default)
  find --text T | --contains T | --id I | --role R
  tap <x> <y> | tap --text T | --contains T | --id I [--index N]
  swipe <x1> <y1> <x2> <y2> [--ms 300]
  type <text> [--keys]                      type into the focused field (--keys: per-key events)
  key home|back|enter|delete|tab|escape|lock
  install <path.apk|path.app>  terminate <app-id>
  launch <app-id> [--relaunch] [-- <process args...>]   (args/relaunch: iOS)
  wait --text T|--contains T|--id I [--gone] [--timeout-ms 10000]

Test capabilities (docs/CAPABILITIES.md; reset restores appearance, location, clipboard, permissions):
  open <url>                                open a URL / deep link as a tapped link would
  permission grant|revoke <app-id> <name>...   names: camera microphone photos photos-add
                                            location location-always contacts contacts-limited
                                            calendar reminders notifications motion media-library
                                            siri, or android.permission.*
  permission reset <app-id> [<name>...]     ask again on next use (all permissions without names)
  location set <lat> <lon> | location clear simulated GPS fix
  clipboard [get] | clipboard set <text>    plain text (get prints it; --json for JSON)
  appearance [light|dark]                   read, or switch light/dark mode
  apps                                      installed user apps
  uninstall <app-id>  clear-data <app-id>   clear-data: like a fresh install, app kept
  push <app-id> <json|file.json|->          push notification (iOS)

Actions (tap, swipe, type, key, launch, open, push) also wait for their effect in the same call:
  --wait-text T | --wait-contains T | --wait-id I | --wait-role R  [--wait-gone] [--wait-timeout-ms N]
  --ui prints the screen the action ends on (outline), so one call acts, waits and observes
  e.g. agemu tap --text "Log in" --wait-id home-screen --ui

Exit status: 0 ok, 1 error, 2 usage, 3 not implemented on the device's platform.

Servers:
  daemon                                    run the daemon in the foreground
  mcp                                       MCP server on stdio (for agent harnesses)
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	case "daemon":
		path, err := client.DaemonPath()
		if err != nil {
			log.Fatal(err)
		}
		// Replace this process so signals reach the daemon directly.
		log.Fatal(syscall.Exec(path, []string{path}, os.Environ()))
	case "mcp":
		if err := mcp.Serve(os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runCommand(cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, api.ErrNotImplemented) {
			os.Exit(3) // a script can skip the step on this platform
		}
		os.Exit(1)
	}
}

// parsed command line: positional args + --flags (flags may appear anywhere).
type cli struct {
	pos   []string
	flags map[string]string
}

var boolFlags = map[string]bool{"cold": true, "json": true, "gone": true, "keys": true, "relaunch": true, "wait-gone": true, "ui": true}

func parse(args []string) cli {
	c := cli{flags: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			c.pos = append(c.pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && !isNumber(a) {
			name := strings.TrimLeft(a, "-")
			if k, v, ok := strings.Cut(name, "="); ok {
				c.flags[k] = v
				continue
			}
			if name == "o" {
				name = "output"
			}
			if name == "d" {
				name = "device"
			}
			if boolFlags[name] || i+1 >= len(args) {
				c.flags[name] = "true"
				continue
			}
			c.flags[name] = args[i+1]
			i++
			continue
		}
		c.pos = append(c.pos, a)
	}
	return c
}

func isNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

func (c cli) int(name string) int { v, _ := strconv.Atoi(c.flags[name]); return v }

func (c cli) selector() *api.Selector {
	s := api.Selector{Text: c.flags["text"], Contains: c.flags["contains"], ID: c.flags["id"], Role: c.flags["role"], Index: c.int("index")}
	if s.Empty() {
		return nil
	}
	return &s
}

// waitSelector builds the selector of an action's --wait-* flags (nil without any).
func (c cli) waitSelector() *api.Selector {
	s := api.Selector{Text: c.flags["wait-text"], Contains: c.flags["wait-contains"], ID: c.flags["wait-id"], Role: c.flags["wait-role"]}
	if s.Empty() {
		return nil
	}
	return &s
}

func atoi(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	return int(f), err
}

func runCommand(cmd string, args []string) error {
	c := parse(args)
	method, p, err := request(cmd, c, os.Stdin)
	if err != nil {
		return err
	}
	textOut := c.flags["json"] != "true" // ui/find/clipboard print text unless --json

	cl, err := client.Dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	raw, _, err := cl.CallRaw(method, p)
	if err != nil {
		return err
	}
	if method == "ui" && textOut {
		var t api.UITree
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		fmt.Print(outline.Render(&t))
		return nil
	}
	if p.ReturnUI && textOut {
		// An action with --ui prints the screen it ends on as an outline (after a JSON line
		// with the action's result); --json prints everything as JSON.
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
			fmt.Println(string(b))
			fmt.Print(outline.Render(r.UI))
			return nil
		}
	}
	if (method == "find") && textOut {
		var els []*api.Element
		if err := json.Unmarshal(raw, &els); err != nil {
			return err
		}
		for _, e := range els {
			fmt.Println(outline.Line(e))
		}
		return nil
	}
	if method == "clipboard" && p.Action == "get" && textOut {
		// The text itself, so a shell can use $(agemu clipboard).
		var r struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		fmt.Println(r.Text)
		return nil
	}
	os.Stdout.Write(raw)
	os.Stdout.Write([]byte{'\n'})
	return nil
}

// request turns a command line into a daemon method and its parameters. stdin is read for
// `push <app> -`.
func request(cmd string, c cli, stdin io.Reader) (string, daemon.Params, error) {
	p := daemon.Params{Device: c.flags["device"]}
	if p.Device == "" {
		p.Device = os.Getenv("AGEMU_DEVICE")
	}
	method, err := params(cmd, c, stdin, &p)
	return method, p, err
}

// params fills p for cmd and returns the daemon method.
func params(cmd string, c cli, stdin io.Reader, p *daemon.Params) (string, error) {
	method := cmd
	switch cmd {
	case "ping", "devices":
	case "profiles":
		p.Platform = api.Platform(c.flags["platform"])
	case "create":
		p.Platform = api.Platform(c.flags["platform"])
		p.Name = c.flags["name"]
		p.Profile = c.flags["profile"]
	case "boot", "shutdown", "reset":
		if len(c.pos) > 0 {
			p.Device = c.pos[0]
		}
		p.Cold = c.flags["cold"] == "true"
	case "profile":
		method = "set_profile"
		if len(c.pos) > 0 {
			p.Profile = c.pos[0]
		}
		p.Width, p.Height, p.Density = c.int("width"), c.int("height"), c.int("density")
	case "screenshot":
		p.Path = c.flags["output"]
		if p.Path != "" {
			if abs, err := filepath.Abs(p.Path); err == nil {
				p.Path = abs
			}
		}
		p.Format = c.flags["format"]
		p.Scale, _ = strconv.ParseFloat(c.flags["scale"], 64)
	case "ui":
	case "find", "wait":
		p.Selector = c.selector()
		p.TimeoutMs = c.int("timeout-ms")
		p.Gone = c.flags["gone"] == "true"
	case "tap":
		if len(c.pos) >= 2 {
			x, err1 := atoi(c.pos[0])
			y, err2 := atoi(c.pos[1])
			if err1 != nil || err2 != nil {
				return "", fmt.Errorf("tap: bad coordinates")
			}
			p.X, p.Y = &x, &y
		} else {
			p.Selector = c.selector()
		}
	case "swipe":
		if len(c.pos) < 4 {
			return "", fmt.Errorf("swipe needs x1 y1 x2 y2")
		}
		var v [4]int
		for i := range v {
			n, err := atoi(c.pos[i])
			if err != nil {
				return "", fmt.Errorf("swipe: bad coordinate %q", c.pos[i])
			}
			v[i] = n
		}
		p.X, p.Y, p.X2, p.Y2 = &v[0], &v[1], v[2], v[3]
		p.DurationMs = c.int("ms")
	case "type":
		p.Text = strings.Join(c.pos, " ")
		p.Keys = c.flags["keys"] == "true"
	case "key":
		if len(c.pos) == 0 {
			return "", fmt.Errorf("key needs a key name")
		}
		p.Key = c.pos[0]
	case "install":
		if len(c.pos) == 0 {
			return "", fmt.Errorf("install needs a path")
		}
		abs, err := filepath.Abs(c.pos[0])
		if err != nil {
			return "", err
		}
		p.Path = abs
	case "launch", "terminate", "uninstall", "clear-data":
		if len(c.pos) == 0 {
			return "", fmt.Errorf("%s needs an app id", cmd)
		}
		p.App = c.pos[0]
		if cmd == "launch" {
			p.Args = c.pos[1:] // agemu launch <app> -- <process arguments...>
			p.Relaunch = c.flags["relaunch"] == "true"
		}
		if cmd == "clear-data" {
			method = "clear_app_data"
		}

	// Test capabilities: the daemon checks the values (internal/daemon/capabilities.go); the
	// CLI only maps the command line onto its parameters.
	case "open":
		method = "open_url"
		if len(c.pos) == 0 {
			return "", fmt.Errorf("open needs a url")
		}
		p.URL = c.pos[0]
	case "permission":
		if len(c.pos) < 2 {
			return "", fmt.Errorf("usage: permission grant|revoke|reset <app-id> [<name>...]")
		}
		p.Action, p.App, p.Permissions = c.pos[0], c.pos[1], c.pos[2:]
	case "location":
		pos := c.pos
		if len(pos) > 0 && !isNumber(pos[0]) {
			p.Action, pos = pos[0], pos[1:]
		}
		if len(pos) > 0 {
			if len(pos) != 2 {
				return "", fmt.Errorf("usage: location set <latitude> <longitude> | location clear")
			}
			lat, err1 := strconv.ParseFloat(pos[0], 64)
			lon, err2 := strconv.ParseFloat(pos[1], 64)
			if err1 != nil || err2 != nil || !finite(lat) || !finite(lon) {
				return "", fmt.Errorf("location: bad coordinates %q %q", pos[0], pos[1])
			}
			p.Latitude, p.Longitude = &lat, &lon
		}
	case "clipboard":
		switch {
		case len(c.pos) == 0 || (c.pos[0] == "get" && len(c.pos) == 1):
			p.Action = "get"
		case c.pos[0] == "set":
			p.Action, p.Text = "set", strings.Join(c.pos[1:], " ")
		default:
			return "", fmt.Errorf("usage: clipboard [get] | clipboard set <text>")
		}
	case "appearance":
		pos := c.pos
		if len(pos) > 0 && (pos[0] == "get" || pos[0] == "set") {
			pos = pos[1:]
		}
		if len(pos) > 0 {
			p.Mode = pos[0]
		}
	case "apps":
	case "push":
		if len(c.pos) < 2 {
			return "", fmt.Errorf("usage: push <app-id> <payload JSON | file.json | - for stdin>")
		}
		p.App = c.pos[0]
		payload, err := readPayload(c.pos[1], stdin)
		if err != nil {
			return "", err
		}
		p.Payload = payload
	default:
		return "", fmt.Errorf("unknown command %q (see agemu help)", cmd)
	}

	switch method {
	case "tap", "swipe", "type", "key", "launch", "open_url", "push":
		if ws := c.waitSelector(); ws != nil {
			p.WaitFor, p.WaitGone, p.WaitTimeoutMs = ws, c.flags["wait-gone"] == "true", c.int("wait-timeout-ms")
		}
		p.ReturnUI = c.flags["ui"] == "true"
	}
	return method, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// readPayload reads a push payload given inline ({...}), as a file path, or as - for stdin.
func readPayload(arg string, stdin io.Reader) (json.RawMessage, error) {
	var data []byte
	var err error
	switch s := strings.TrimSpace(arg); {
	case strings.HasPrefix(s, "{"):
		data = []byte(s)
	case s == "-":
		data, err = io.ReadAll(stdin)
	default:
		data, err = os.ReadFile(s)
	}
	if err != nil {
		return nil, fmt.Errorf("push: reading the payload: %w", err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("push: the payload is not valid JSON")
	}
	return json.RawMessage(data), nil
}
