package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const waitTimeout = 20 * time.Second

// ---- classic: adb + uiautomator, as an agent with a shell drives the standard emulator ----

type classic struct {
	r      *Recorder
	serial string
	tmp    string
	w, h   int // screen size, from the first dump
}

type uiNode struct {
	Text   string   `xml:"text,attr"`
	Desc   string   `xml:"content-desc,attr"`
	Hint   string   `xml:"hint,attr"`
	Bounds string   `xml:"bounds,attr"`
	Kids   []uiNode `xml:"node"`
}

var boundsRe = regexp.MustCompile(`\[(\d+),(\d+)\]\[(\d+),(\d+)\]`)

func (n uiNode) labels() []string { return []string{n.Text, n.Desc, n.Hint} }

func (n uiNode) walk(f func(uiNode) bool) bool {
	if f(n) {
		return true
	}
	for _, k := range n.Kids {
		if k.walk(f) {
			return true
		}
	}
	return false
}

func (c *classic) adb(display string, args ...string) ([]byte, error) {
	return c.r.run(display, filepath.Join(androidSDK(), "platform-tools", "adb"), append([]string{"-s", c.serial}, args...)...)
}

// dump reads the screen's UI tree: one uiautomator dump written to stdout.
func (c *classic) dump() (uiNode, error) {
	out, err := c.adb("adb exec-out uiautomator dump /dev/tty", "exec-out", "uiautomator", "dump", "/dev/tty")
	if err != nil {
		return uiNode{}, err
	}
	s := string(out)
	if i := strings.Index(s, "</hierarchy>"); i >= 0 {
		s = s[:i+len("</hierarchy>")]
	}
	var root struct {
		Rotation string   `xml:"rotation,attr"`
		Nodes    []uiNode `xml:"node"`
	}
	if err := xml.Unmarshal([]byte(s), &root); err != nil {
		return uiNode{}, fmt.Errorf("uiautomator dump: %v", err)
	}
	top := uiNode{Kids: root.Nodes}
	if c.w == 0 && len(root.Nodes) > 0 {
		if m := boundsRe.FindStringSubmatch(root.Nodes[0].Bounds); m != nil {
			c.w, _ = strconv.Atoi(m[3])
			c.h, _ = strconv.Atoi(m[4])
		}
	}
	return top, nil
}

func find(root uiNode, match func(string) bool) (uiNode, bool) {
	var hit uiNode
	ok := root.walk(func(n uiNode) bool {
		for _, l := range n.labels() {
			if l != "" && match(l) {
				hit = n
				return true
			}
		}
		return false
	})
	return hit, ok
}

func exact(s string) func(string) bool { return func(l string) bool { return l == s } }
func contains(s string) func(string) bool {
	return func(l string) bool { return strings.Contains(l, s) }
}

func (w Cond) met(root uiNode) bool {
	if w.Text != "" {
		if _, ok := find(root, exact(w.Text)); !ok {
			return false
		}
	}
	if w.Contains != "" {
		if _, ok := find(root, contains(w.Contains)); !ok {
			return false
		}
	}
	if w.Gone != "" {
		if _, ok := find(root, exact(w.Gone)); ok {
			return false
		}
	}
	return true
}

// waitFor polls the UI tree until the condition holds, as an agent re-reads the screen.
func (c *classic) waitFor(w Cond) (uiNode, error) {
	deadline := time.Now().Add(waitTimeout)
	for {
		root, err := c.dump()
		if err == nil && w.met(root) {
			return root, nil
		}
		if time.Now().After(deadline) {
			return root, fmt.Errorf("timed out waiting for %+v", w)
		}
	}
}

func center(n uiNode) (int, int, error) {
	m := boundsRe.FindStringSubmatch(n.Bounds)
	if m == nil {
		return 0, 0, fmt.Errorf("no bounds %q", n.Bounds)
	}
	v := make([]int, 4)
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return (v[0] + v[2]) / 2, (v[1] + v[3]) / 2, nil
}

func (c *classic) Launch(w Cond) error {
	if _, err := c.adb("adb shell am start -W -n "+appID+"/.LoginActivity", "shell", "am", "start", "-W", "-n", appID+"/.LoginActivity"); err != nil {
		return err
	}
	_, err := c.waitFor(w)
	return err
}

func (c *classic) Tap(target string, w Cond) error {
	root, err := c.waitFor(Cond{Text: target})
	if err != nil {
		return err
	}
	n, _ := find(root, exact(target))
	x, y, err := center(n)
	if err != nil {
		return err
	}
	xs, ys := strconv.Itoa(x), strconv.Itoa(y)
	if _, err := c.adb("adb shell input tap "+xs+" "+ys, "shell", "input", "tap", xs, ys); err != nil {
		return err
	}
	if w.zero() {
		return nil
	}
	_, err = c.waitFor(w)
	return err
}

// Type sends text with input text, which takes %s for a space.
func (c *classic) Type(text string, w Cond) error {
	arg := strings.ReplaceAll(text, " ", "%s")
	if _, err := c.adb("adb shell input text "+arg, "shell", "input", "text", arg); err != nil {
		return err
	}
	if w.zero() {
		return nil
	}
	_, err := c.waitFor(w)
	return err
}

func (c *classic) ScrollUntil(target string) error {
	if c.w == 0 {
		if _, err := c.dump(); err != nil {
			return err
		}
	}
	x := strconv.Itoa(c.w / 2)
	y1, y2 := strconv.Itoa(c.h*72/100), strconv.Itoa(c.h*32/100)
	for i := 0; i < 8; i++ {
		if _, err := c.adb("adb shell input swipe "+x+" "+y1+" "+x+" "+y2+" 500", "shell", "input", "swipe", x, y1, x, y2, "500"); err != nil {
			return err
		}
		root, err := c.dump()
		if err != nil {
			return err
		}
		if _, ok := find(root, exact(target)); ok {
			return nil
		}
	}
	return fmt.Errorf("%q not found after scrolling", target)
}

func (c *classic) Read(sub string) (string, error) {
	root, err := c.waitFor(Cond{Contains: sub})
	if err != nil {
		return "", err
	}
	n, _ := find(root, contains(sub))
	for _, l := range n.labels() {
		if strings.Contains(l, sub) {
			return l, nil
		}
	}
	return "", fmt.Errorf("%q not on screen", sub)
}

func (c *classic) Screenshot(name string) error {
	out, err := c.adb("adb exec-out screencap -p > "+name+".png", "exec-out", "screencap", "-p")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.tmp, name+".png"), out, 0o644)
}

// ---- agemu: the agemu CLI, one process per call ----

type agemuDriver struct {
	r   *Recorder
	bin string
	dev string
	tmp string
	w   int
	h   int
}

func (a *agemuDriver) agemu(args ...string) ([]byte, error) {
	return a.agemuShown("agemu "+quoteArgs(args), args...)
}

func (a *agemuDriver) agemuShown(display string, args ...string) ([]byte, error) {
	return a.r.run(display, a.bin, append(args, "-d", a.dev)...)
}

func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, s := range args {
		if s == "" || strings.ContainsAny(s, " \"'$#&‹") {
			q[i] = strconv.Quote(s)
		} else {
			q[i] = s
		}
	}
	return strings.Join(q, " ")
}

func waitArgs(w Cond) []string {
	switch {
	case w.Text != "":
		return []string{"--wait-text", w.Text}
	case w.Contains != "":
		return []string{"--wait-contains", w.Contains}
	case w.Gone != "":
		return []string{"--wait-text", w.Gone, "--wait-gone"}
	}
	return nil
}

func (a *agemuDriver) Launch(w Cond) error {
	_, err := a.agemu(append([]string{"launch", appID}, waitArgs(w)...)...)
	return err
}

func (a *agemuDriver) Tap(target string, w Cond) error {
	_, err := a.agemu(append([]string{"tap", "--text", target}, waitArgs(w)...)...)
	return err
}

func (a *agemuDriver) Type(text string, w Cond) error {
	_, err := a.agemu(append([]string{"type", text}, waitArgs(w)...)...)
	return err
}

var screenRe = regexp.MustCompile(`screen (\d+)x(\d+)`)

func (a *agemuDriver) ScrollUntil(target string) error {
	if a.w == 0 {
		out, err := a.agemu("ui")
		if err != nil {
			return err
		}
		m := screenRe.FindStringSubmatch(string(out))
		if m == nil {
			return fmt.Errorf("no screen size in the outline")
		}
		a.w, _ = strconv.Atoi(m[1])
		a.h, _ = strconv.Atoi(m[2])
	}
	x := strconv.Itoa(a.w / 2)
	y1, y2 := strconv.Itoa(a.h*72/100), strconv.Itoa(a.h*32/100)
	for i := 0; i < 8; i++ {
		out, err := a.agemu("swipe", x, y1, x, y2, "--ms", "500", "--ui")
		if err != nil {
			return err
		}
		if strings.Contains(string(out), strconv.Quote(target)) {
			return nil
		}
	}
	return fmt.Errorf("%q not found after scrolling", target)
}

var quoted = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

func (a *agemuDriver) Read(sub string) (string, error) {
	out, err := a.agemu("ui")
	if err != nil {
		return "", err
	}
	for _, m := range quoted.FindAllStringSubmatch(string(out), -1) {
		if s, err := strconv.Unquote(`"` + m[1] + `"`); err == nil && strings.Contains(s, sub) {
			return s, nil
		}
	}
	return "", fmt.Errorf("%q not on screen", sub)
}

func (a *agemuDriver) Screenshot(name string) error {
	_, err := a.agemuShown("agemu screenshot -o "+name+".png", "screenshot", "-o", filepath.Join(a.tmp, name+".png"))
	return err
}
