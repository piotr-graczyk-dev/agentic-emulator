// Command agemu is an agent-first Android emulator / iOS Simulator runtime for macOS.
//
//	agemu daemon                 run the device daemon (auto-started by other commands)
//	agemu mcp                    serve the MCP protocol on stdio
//	agemu <command> [flags]      talk to the daemon; prints JSON (ui/find print a text outline)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/ios"
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
  type <text>                               type into the focused field
  key home|back|enter|delete|tab|escape|lock
  install <path.apk|path.app>  launch <app-id>  terminate <app-id>
  wait --text T|--contains T|--id I [--gone] [--timeout-ms 10000]

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
		runDaemon()
		return
	case "mcp":
		if err := mcp.Serve(os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runCommand(cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runDaemon() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tmp := filepath.Join(daemon.Home(), "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	svc := daemon.NewService(tmp, android.New(), ios.New())
	defer svc.Close()
	if err := daemon.Serve(ctx, svc); err != nil {
		log.Fatal(err)
	}
}

// parsed command line: positional args + --flags (flags may appear anywhere).
type cli struct {
	pos   []string
	flags map[string]string
}

var boolFlags = map[string]bool{"cold": true, "json": true, "gone": true}

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

func atoi(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	return int(f), err
}

func runCommand(cmd string, args []string) error {
	c := parse(args)
	p := daemon.Params{Device: c.flags["device"]}
	if p.Device == "" {
		p.Device = os.Getenv("AGEMU_DEVICE")
	}
	textOut := c.flags["json"] != "true" // ui/find print a text outline unless --json
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
		cmd = "set_profile"
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
				return fmt.Errorf("tap: bad coordinates")
			}
			p.X, p.Y = &x, &y
		} else {
			p.Selector = c.selector()
		}
	case "swipe":
		if len(c.pos) < 4 {
			return fmt.Errorf("swipe needs x1 y1 x2 y2")
		}
		var v [4]int
		for i := range v {
			n, err := atoi(c.pos[i])
			if err != nil {
				return fmt.Errorf("swipe: bad coordinate %q", c.pos[i])
			}
			v[i] = n
		}
		p.X, p.Y, p.X2, p.Y2 = &v[0], &v[1], v[2], v[3]
		p.DurationMs = c.int("ms")
	case "type":
		p.Text = strings.Join(c.pos, " ")
	case "key":
		if len(c.pos) == 0 {
			return fmt.Errorf("key needs a key name")
		}
		p.Key = c.pos[0]
	case "install":
		if len(c.pos) == 0 {
			return fmt.Errorf("install needs a path")
		}
		abs, err := filepath.Abs(c.pos[0])
		if err != nil {
			return err
		}
		p.Path = abs
	case "launch", "terminate":
		if len(c.pos) == 0 {
			return fmt.Errorf("%s needs an app id", cmd)
		}
		p.App = c.pos[0]
	default:
		return fmt.Errorf("unknown command %q (see agemu help)", cmd)
	}

	cl, err := client.Dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	raw, _, err := cl.CallRaw(cmd, p)
	if err != nil {
		return err
	}
	if cmd == "ui" && textOut {
		var t api.UITree
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		fmt.Print(outline.Render(&t))
		return nil
	}
	if (cmd == "find") && textOut {
		var els []*api.Element
		if err := json.Unmarshal(raw, &els); err != nil {
			return err
		}
		for _, e := range els {
			fmt.Println(outline.Line(e))
		}
		return nil
	}
	os.Stdout.Write(raw)
	os.Stdout.Write([]byte{'\n'})
	return nil
}
