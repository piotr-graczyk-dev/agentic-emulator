//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---- agent-device: its CLI, as its help tells an agent to use it ----
//
// press/fill by selector (label="...") with --settle ("Use --settle on every mutating app
// action"), whose settled diff is the next snapshot: a wait text follows only when that diff does
// not show the expected text yet. scroll --until, snapshot to read, screenshot. Every call goes to
// agent-device's daemon, which holds the session.

type agentDevice struct {
	r    *Recorder
	bin  string
	tmp  string
	sel  []string // device selection flags (--platform, --serial / --udid)
	plat string
}

func (a *agentDevice) ad(display string, args ...string) ([]byte, error) {
	return a.r.run(display, a.bin, append(args, a.sel...)...)
}

func (a *agentDevice) cmd(args ...string) ([]byte, error) {
	return a.ad("agent-device "+quoteArgs(args), args...)
}

// retryable: the target was not found (as opposed to a broken tool or device).
func retryable(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "did not match") || strings.Contains(m, "no element matches") ||
		strings.Contains(m, "not found") || strings.Contains(m, "diagnostics log")
}

var snapRef = regexp.MustCompile(`(@e\d+) \[[^\]]+\] "((?:[^"\\]|\\.)*)"`)

// refFor finds, in a snapshot -i, the ref of the element shown with the selector's text.
func refFor(snapshot, selector string) string {
	want, err := strconv.Unquote(strings.TrimPrefix(selector, "label="))
	if err != nil {
		return ""
	}
	for _, m := range snapRef.FindAllStringSubmatch(snapshot, -1) {
		if s, err := strconv.Unquote(`"` + m[2] + `"`); err == nil && s == want {
			return m[1]
		}
	}
	return ""
}

func label(s string) string { return `label=` + strconv.Quote(s) }

func (a *agentDevice) wait(w Cond) error {
	switch {
	case w.Text != "":
		_, err := a.cmd("wait", "text", w.Text, "20000")
		return err
	case w.Contains != "":
		_, err := a.cmd("wait", "text", w.Contains, "20000")
		return err
	case w.Gone != "":
		_, err := a.cmd("wait", "absent", label(w.Gone), "20000")
		return err
	}
	return nil
}

func (a *agentDevice) Launch(w Cond) error {
	if _, err := a.cmd("open", appID, "--relaunch"); err != nil {
		return err
	}
	return a.wait(w)
}

// act runs a press or fill with --settle; when the selector did not match, it reads the screen
// again (snapshot -i, as agent-device's help says) and retries once. Then it waits for w unless
// the settled diff already shows it.
func (a *agentDevice) act(w Cond, args ...string) error {
	args = append(args, "--settle")
	out, err := a.cmd(args...)
	if err != nil && retryable(err) {
		snap, err := a.cmd("snapshot", "-i")
		if err != nil {
			return err
		}
		// Retry with the ref of the element the snapshot shows under that text, if any.
		if ref := refFor(string(snap), args[1]); ref != "" {
			args[1] = ref
		}
		out, err = a.cmd(args...)
	}
	if err != nil {
		return err
	}
	if t := w.Text + w.Contains; t != "" && strings.Contains(string(out), t) {
		return nil
	}
	return a.wait(w)
}

func (a *agentDevice) Tap(target string, w Cond) error {
	return a.act(w, "press", label(target))
}

func (a *agentDevice) Fill(field, text string, w Cond) error {
	return a.act(w, "fill", label(field), text)
}

func (a *agentDevice) ScrollUntil(target string) error {
	_, err := a.cmd("scroll", "down", "--until", label(target), "--max-passes", "8")
	return err
}

func (a *agentDevice) Read(sub string) (string, error) {
	out, err := a.cmd("snapshot", "--force-full") // Android answers a repeated snapshot with "unchanged"
	if err != nil {
		return "", err
	}
	for _, s := range quotedStrings(string(out)) {
		if strings.Contains(s, sub) {
			return s, nil
		}
	}
	return "", fmt.Errorf("%q not on screen", sub)
}

func (a *agentDevice) Screenshot(name string) error {
	_, err := a.ad("agent-device screenshot "+name+".png", "screenshot", filepath.Join(a.tmp, name+".png"))
	return err
}

// ---- Argent: its CLI (argent run <tool>), as its tool descriptions tell an agent to use it ----
//
// Taps take normalized coordinates, so a tap first reads `describe` to find the element and taps
// the centre of its frame. Text goes in with `fill` into the focused field, waits use
// `await-ui-element`, scrolls `gesture-swipe` without momentum (its advice for scroll-to-element
// loops) followed by a `describe`.

type argent struct {
	r    *Recorder
	bin  string
	udid string
	tmp  string
	plat string
}

func (a *argent) run(tool string, args ...string) ([]byte, error) {
	display := "argent run " + tool
	if len(args) > 0 {
		display += " " + quoteArgs(args)
	}
	return a.r.run(display, a.bin, append([]string{"run", tool, "--udid", a.udid}, args...)...)
}

// describeLine: `  Button "Sign in" [clickable]  (0.047, 0.345, 0.906, 0.050)`.
var describeLine = regexp.MustCompile(`^\s*(\S+)\s+"((?:[^"\\]|\\.)*)"(.*)\(([0-9.]+), ([0-9.]+), ([0-9.]+), ([0-9.]+)\)\s*$`)

type axEl struct {
	role, label, flags string
	x, y, w, h         float64
}

func (a *argent) describe() ([]axEl, error) {
	out, err := a.run("describe", "--json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("describe: %v", err)
	}
	var els []axEl
	for _, line := range strings.Split(res.Description, "\n") {
		m := describeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		l, _ := strconv.Unquote(`"` + m[2] + `"`)
		e := axEl{role: m[1], label: l, flags: m[3]}
		e.x, _ = strconv.ParseFloat(m[4], 64)
		e.y, _ = strconv.ParseFloat(m[5], 64)
		e.w, _ = strconv.ParseFloat(m[6], 64)
		e.h, _ = strconv.ParseFloat(m[7], 64)
		els = append(els, e)
	}
	return els, nil
}

// pick finds the element an agent would tap for a target label. Android's describe shows a
// password field as "[password]", not by its hint.
func pick(els []axEl, target string) (axEl, bool) {
	for _, e := range els {
		if e.label == target || strings.HasPrefix(e.label, target+",") || strings.HasPrefix(e.label, target+" / ") {
			return e, true
		}
	}
	if target == "Password" {
		for _, e := range els {
			if strings.Contains(e.flags, "password") || e.label == "[password]" {
				return e, true
			}
		}
	}
	return axEl{}, false
}

func (a *argent) wait(w Cond) error {
	var cond, text string
	switch {
	case w.Text != "":
		cond, text = "visible", w.Text
	case w.Contains != "":
		cond, text = "visible", w.Contains
	case w.Gone != "":
		cond, text = "hidden", w.Gone
	default:
		return nil
	}
	sel, _ := json.Marshal(map[string]string{"text": text})
	out, err := a.run("await-ui-element", "--condition", cond, "--selector-json", string(sel), "--timeoutMs", "20000")
	if err != nil {
		return err
	}
	var res struct {
		Success bool   `json:"success"`
		Note    string `json:"note"`
	}
	if json.Unmarshal(out, &res) == nil && !res.Success {
		return fmt.Errorf("await-ui-element %s %q: %s", cond, text, res.Note)
	}
	return nil
}

func (a *argent) Launch(w Cond) error {
	if _, err := a.run("launch-app", "--bundleId", appID); err != nil {
		return err
	}
	return a.wait(w)
}

func (a *argent) tap(target string) error {
	els, err := a.describe()
	if err != nil {
		return err
	}
	e, ok := pick(els, target)
	if !ok { // read the screen once more, as with every tool
		if els, err = a.describe(); err != nil {
			return err
		}
		if e, ok = pick(els, target); !ok {
			return fmt.Errorf("%q not in describe", target)
		}
	}
	_, err = a.run("gesture-tap", "--x", fmt.Sprintf("%.4f", e.x+e.w/2), "--y", fmt.Sprintf("%.4f", e.y+e.h/2))
	return err
}

func (a *argent) Tap(target string, w Cond) error {
	if err := a.tap(target); err != nil {
		return err
	}
	return a.wait(w)
}

func (a *argent) Fill(field, text string, w Cond) error {
	if err := a.tap(field); err != nil {
		return err
	}
	if _, err := a.run("fill", "--text", text); err != nil {
		return err
	}
	return a.wait(w)
}

func (a *argent) ScrollUntil(target string) error {
	for i := 0; i < 8; i++ {
		if _, err := a.run("gesture-swipe", "--fromX", "0.5", "--fromY", "0.72", "--toX", "0.5", "--toY", "0.32",
			"--durationMs", "500", "--momentum", "false"); err != nil {
			return err
		}
		els, err := a.describe()
		if err != nil {
			return err
		}
		if e, ok := pick(els, target); ok && e.y+e.h/2 > 0 && e.y+e.h/2 < visibleBand {
			return nil
		}
	}
	return fmt.Errorf("%q not found after scrolling", target)
}

func (a *argent) Read(sub string) (string, error) {
	els, err := a.describe()
	if err != nil {
		return "", err
	}
	for _, e := range els {
		if strings.Contains(e.label, sub) {
			return e.label, nil
		}
	}
	return "", fmt.Errorf("%q not on screen", sub)
}

func (a *argent) Screenshot(name string) error {
	_, err := a.r.run("argent run screenshot --out "+name+".png", a.bin, "run", "screenshot", "--udid", a.udid,
		"--out", filepath.Join(a.tmp, name+".png"))
	return err
}
