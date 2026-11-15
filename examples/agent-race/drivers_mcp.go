//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---- mobile-mcp: its MCP server over stdio, as its instructions tell an agent to use it ----
//
// mobile-mcp has no CLI for an agent, so this arm keeps one server process and sends it
// tools/call requests, as an MCP client does; each request is one tool call. Its instructions:
// "Read the screen with mobile_list_elements_on_screen", tap by the ref that list gives, and
// "Group known sequences (tap, type, tap) into mobile_batch_commands". So every action is one
// mobile_batch_commands call with listElementsAtEnd: the action and the next read of the screen
// are one round trip, and a fill is a tap and a type in the same call. mobile-mcp has no wait
// tool: a wait reads the screen again until the expected element is there.

// mcpClient is a minimal MCP client on a server's stdio: newline-delimited JSON-RPC.
type mcpClient struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	msgs    chan mcpMsg
	id      int
	version string
}

type mcpMsg struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

const mcpTimeout = 90 * time.Second

func startMCP(bin string, env ...string) (*mcpClient, error) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &mcpClient{cmd: cmd, in: in, msgs: make(chan mcpMsg, 16)}
	go func() {
		defer close(c.msgs)
		rd := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := rd.ReadBytes('\n')
			var m mcpMsg
			if json.Unmarshal(line, &m) == nil && m.ID != nil {
				c.msgs <- m
			}
			if err != nil {
				return
			}
		}
	}()
	var init struct {
		ServerInfo struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err = c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "agent-race", "version": "1"},
	}, &init)
	if err == nil {
		err = c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	}
	if err != nil {
		c.close()
		return nil, err
	}
	c.version = init.ServerInfo.Version
	return c, nil
}

func (c *mcpClient) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(b, '\n'))
	return err
}

func (c *mcpClient) request(method string, params, result any) error {
	c.id++
	id := c.id
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	timeout := time.After(mcpTimeout)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return fmt.Errorf("%s: the MCP server exited", method)
			}
			if *m.ID != id {
				continue
			}
			if m.Error != nil {
				return fmt.Errorf("%s: %s", method, m.Error.Message)
			}
			if result == nil {
				return nil
			}
			return json.Unmarshal(m.Result, result)
		case <-timeout:
			return fmt.Errorf("%s: no answer in %s", method, mcpTimeout)
		}
	}
}

// tool calls one tool and returns its text; a tool's own failure (isError) is an error.
func (c *mcpClient) tool(name string, args map[string]any) (out string, err error) {
	if os.Getenv("AGENT_RACE_DEBUG") != "" { // every answer on stderr, to see what the tool shows
		defer func() { fmt.Fprintf(os.Stderr, "--- %s %v\n%s\n(err: %v)\n", name, args, out, err) }()
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.request("tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return "", err
	}
	var text []string
	for _, p := range res.Content {
		if p.Type == "text" {
			text = append(text, p.Text)
		}
	}
	out = strings.Join(text, "\n")
	if res.IsError {
		return out, fmt.Errorf("%s: %s", name, lastLine(out))
	}
	return out, nil
}

func (c *mcpClient) close() {
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

type mobileMCP struct {
	r    *Recorder
	c    *mcpClient
	dev  string
	tmp  string
	els  []mmEl // the latest list of elements; its refs are good until the next list
	prev []mmEl // the list before it
	w, h int
	// Android: the list shows a text field without its hint ("@e4 EditText at=..."), so what the
	// plan knows by a hint is found another way: a field to fill is the first empty text field
	// on the screen (the plan fills them top to bottom), and mmStandIn names what is waited for
	// instead. An agent would need a screenshot to tell the fields apart; the arm is not
	// charged for one.
	hintless bool
	// iOS: the list can show a screen that still moves (a screen sliding in has the old screen's
	// elements too, a list scrolls to rest for a second or two), and a ref is an element's place
	// in the list, which changes while the screen does. mobile-mcp has no wait tool, so an
	// element is used once two reads in a row have it under the same ref at the same place
	// (atRest). The list also has the rows of a scrolling list that are under the app's header
	// or below the screen: a row counts as on screen between mmTopBand and visibleBand.
	beyond bool
}

// mmStandIn: the catalog, which the plan waits for by its search field's hint, by its title.
var mmStandIn = map[string]string{"Search products": "Catalog"}

// mmTopBand: the catalog's header (title, search field, count) ends at 0.24 of the height.
const mmTopBand = 0.28

// mmEl is one line of mobile_list_elements_on_screen:
// @e5 Button text="Sign in" label="..." at=96,1240 size=888x132 [focused].
type mmEl struct {
	ref, typ   string
	labels     []string // text, label, name, value
	x, y, w, h int
}

func (e mmEl) emptyField() bool { return strings.HasSuffix(e.typ, "EditText") && len(e.labels) == 0 }

// same: the element is where another read had it. A list that scrolls to rest creeps on by a
// point or two for a while.
func (e mmEl) same(o mmEl) bool {
	near := func(a, b int) bool { return a-b <= 2 && b-a <= 2 }
	return e.ref == o.ref && near(e.x, o.x) && near(e.y, o.y)
}

var (
	mmRef  = regexp.MustCompile(`^(@e\d+) `)
	mmAttr = regexp.MustCompile(`\b(?:text|label|name|value)=("(?:[^"\\]|\\.)*")`)
	mmRect = regexp.MustCompile(` at=(-?[0-9.]+),(-?[0-9.]+) size=([0-9.]+)x([0-9.]+)`)
	mmSize = regexp.MustCompile(`(\d+)x(\d+)`)
)

func parseElements(out string) []mmEl {
	var els []mmEl
	for _, line := range strings.Split(out, "\n") {
		rect := mmRect.FindStringSubmatch(line)
		if rect == nil {
			continue
		}
		var e mmEl
		if m := mmRef.FindStringSubmatch(line); m != nil {
			e.ref = m[1]
		}
		e.typ, _, _ = strings.Cut(strings.TrimPrefix(line, e.ref+" "), " ")
		for _, m := range mmAttr.FindAllStringSubmatch(line, -1) {
			if s, err := strconv.Unquote(m[1]); err == nil && s != "" {
				e.labels = append(e.labels, s)
			}
		}
		v := make([]int, 4)
		for i := range v {
			f, _ := strconv.ParseFloat(rect[i+1], 64)
			v[i] = int(f)
		}
		e.x, e.y, e.w, e.h = v[0], v[1], v[2], v[3]
		els = append(els, e)
	}
	return els
}

func findEl(els []mmEl, match func(string) bool) (mmEl, string, bool) {
	for _, e := range els {
		for _, l := range e.labels {
			if match(l) {
				return e, l, true
			}
		}
	}
	return mmEl{}, "", false
}

func (m *mobileMCP) met(w Cond) bool {
	if w.Text != "" {
		text := w.Text
		if s := mmStandIn[text]; m.hintless && s != "" {
			text = s
		}
		if _, _, ok := findEl(m.els, exact(text)); !ok {
			return false
		}
	}
	if w.Contains != "" {
		if _, _, ok := findEl(m.els, contains(w.Contains)); !ok {
			return false
		}
	}
	if w.Gone != "" {
		if _, _, ok := findEl(m.els, exact(w.Gone)); ok {
			return false
		}
	}
	return true
}

// deviceID finds the device in mobile_list_available_devices' answer by its id or its name:
// mobile-mcp names an Android emulator by its AVD and an iOS simulator by its UDID.
func deviceID(list, device string) (string, error) {
	var res struct {
		Devices []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(list), &res); err != nil {
		return "", fmt.Errorf("mobile_list_available_devices: %v", err)
	}
	for _, d := range res.Devices {
		if d.ID == device || d.Name == device {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("mobile-mcp does not list the device %s: %s", device, lastLine(list))
}

// newMobileMCP starts the server and, untimed like every arm's warm-up, makes its first calls:
// the device list, a launch and a read of the screen.
func newMobileMCP(r *Recorder, bin, device, tmp string, android bool) (*mobileMCP, error) {
	c, err := startMCP(bin, "MOBILEMCP_DISABLE_TELEMETRY=1")
	if err != nil {
		return nil, err
	}
	m := &mobileMCP{r: r, c: c, tmp: tmp, hintless: android, beyond: !android}
	warm := func(name string, args map[string]any) (out string, err error) {
		for try := 0; try < 3; try++ { // the first call can install mobile-mcp's helper on the device
			if out, err = c.tool(name, args); err == nil {
				return out, nil
			}
			time.Sleep(3 * time.Second)
		}
		return "", fmt.Errorf("warm-up: %w", err)
	}
	list, err := warm("mobile_list_available_devices", map[string]any{})
	if err == nil {
		m.dev, err = deviceID(list, device)
	}
	if err == nil {
		_, err = warm("mobile_launch_app", map[string]any{"device": m.dev, "packageName": appID})
	}
	if err == nil {
		_, err = warm("mobile_list_elements_on_screen", map[string]any{"device": m.dev})
	}
	if err != nil {
		c.close()
		return nil, err
	}
	return m, nil
}

// call is one tools/call, logged as one tool call.
func (m *mobileMCP) call(display, name string, args map[string]any) (string, error) {
	args["device"] = m.dev
	out, err := m.r.call(display, func() ([]byte, error) {
		s, err := m.c.tool(name, args)
		return []byte(s), err
	})
	return string(out), err
}

// noHierarchy: between two screens mobile-mcp can find no UI hierarchy ("its screen is most
// likely off or locked ... try again"). The arm reads the screen again, as the error says; the
// failed call counts.
func noHierarchy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No UI hierarchy was returned")
}

func (m *mobileMCP) list() error {
	for try := 0; ; try++ {
		out, err := m.call("mobile_list_elements_on_screen", "mobile_list_elements_on_screen", map[string]any{})
		if noHierarchy(err) && try < 5 {
			continue
		}
		if err != nil {
			return err
		}
		m.prev, m.els = m.els, parseElements(out)
		return nil
	}
}

type mmStep struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// batch runs the steps and reads the screen in the same call.
func (m *mobileMCP) batch(display string, steps ...mmStep) error {
	out, err := m.call("mobile_batch_commands "+display+" + list", "mobile_batch_commands",
		map[string]any{"steps": steps, "listElementsAtEnd": true})
	if noHierarchy(err) {
		// The steps ran: a failed step is a line of the result, only the list at the end makes
		// the whole call fail.
		return m.list()
	}
	if err != nil {
		return err
	}
	m.prev, m.els = m.els, parseElements(out) // the list is there after a failed step too
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Step ") && strings.Contains(line, ") failed: ") {
			return fmt.Errorf("mobile_batch_commands: %s", line)
		}
	}
	return nil
}

// waitFor reads the screen again until the condition holds.
func (m *mobileMCP) waitFor(w Cond) error {
	if w.zero() {
		return nil
	}
	deadline := time.Now().Add(waitTimeout)
	for !m.met(w) {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %+v", w)
		}
		if err := m.list(); err != nil {
			return err
		}
	}
	return nil
}

// locate finds the element in the latest list; when it is not there, it reads the screen, and
// once more, as every arm retries once.
func (m *mobileMCP) locate(target string) (mmEl, error) {
	for try := 0; ; try++ {
		e, ok, err := m.atRest(target)
		if err != nil {
			return mmEl{}, err
		}
		if ok {
			return e, nil
		}
		if try == 2 {
			return mmEl{}, fmt.Errorf("%q not in the element list", target)
		}
		if err := m.list(); err != nil {
			return mmEl{}, err
		}
	}
}

// atRest finds the target in the latest list. Where the list can show a screen that still
// moves, an element that was not at this place in the list before is read again, until two
// reads in a row agree.
func (m *mobileMCP) atRest(target string) (mmEl, bool, error) {
	e, _, ok := findEl(m.els, exact(target))
	if !ok || !m.beyond {
		return e, ok, nil
	}
	if p, _, was := findEl(m.prev, exact(target)); was && e.same(p) {
		return e, true, nil
	}
	for try := 0; try < 10; try++ {
		if err := m.list(); err != nil {
			return mmEl{}, false, err
		}
		n, _, ok := findEl(m.els, exact(target))
		if !ok {
			return mmEl{}, false, nil
		}
		if n.same(e) {
			return n, true, nil
		}
		e = n
	}
	return e, true, nil // it keeps moving: the latest read
}

// clickThen clicks the element find returns and runs more steps in the same call. When the
// click fails (its ref is gone: the screen changed after the list), it finds the element again
// in the list that call returned and tries once more, as every arm retries once.
func (m *mobileMCP) clickThen(find func() (mmEl, error), what string, more ...mmStep) error {
	for try := 0; ; try++ {
		e, err := find()
		if err != nil {
			return err
		}
		display, step := m.click(e)
		err = m.batch(display+what, append([]mmStep{step}, more...)...)
		if err == nil || try == 1 || !strings.Contains(err.Error(), ": Step 1 (") {
			return err
		}
	}
}

func (m *mobileMCP) click(e mmEl) (string, mmStep) {
	if e.ref != "" {
		return "click " + e.ref, mmStep{"mobile_click_on_screen_at_coordinates", map[string]any{"ref": e.ref}}
	}
	x, y := e.x+e.w/2, e.y+e.h/2
	return fmt.Sprintf("click %d,%d", x, y), mmStep{"mobile_click_on_screen_at_coordinates", map[string]any{"x": x, "y": y}}
}

func (m *mobileMCP) Launch(w Cond) error {
	m.els = nil
	if err := m.batch("launch "+appID, mmStep{"mobile_launch_app", map[string]any{"packageName": appID}}); err != nil {
		return err
	}
	return m.waitFor(w)
}

func (m *mobileMCP) Tap(target string, w Cond) error {
	if err := m.clickThen(func() (mmEl, error) { return m.locate(target) }, ""); err != nil {
		return err
	}
	return m.waitFor(w)
}

// field finds a text field by its label, or, where the list has no hints, the first empty one.
func (m *mobileMCP) field(label string) (mmEl, error) {
	if _, _, ok := findEl(m.els, exact(label)); !ok && m.hintless {
		for _, e := range m.els {
			if e.emptyField() {
				return e, nil
			}
		}
	}
	return m.locate(label)
}

func (m *mobileMCP) Fill(field, text string, w Cond) error {
	if err := m.clickThen(func() (mmEl, error) { return m.field(field) }, ", type "+strconv.Quote(text),
		mmStep{"mobile_type_keys", map[string]any{"text": text, "submit": false}}); err != nil {
		return err
	}
	return m.waitFor(w)
}

func (m *mobileMCP) ScrollUntil(target string) error {
	if m.h == 0 {
		out, err := m.call("mobile_get_screen_size", "mobile_get_screen_size", map[string]any{})
		if err != nil {
			return err
		}
		s := mmSize.FindStringSubmatch(out)
		if s == nil {
			return fmt.Errorf("no screen size in %q", out)
		}
		m.w, _ = strconv.Atoi(s[1])
		m.h, _ = strconv.Atoi(s[2])
	}
	// mobile-mcp's swipe has momentum and no speed setting, and one swipe can carry the list past
	// the item; the way back is in shorter steps. A swipe can also be lost (the screen is as it
	// was): only two of those in a row are the end of the list.
	dir, from, dist := "up", m.h*72/100, m.h*40/100
	back := func() { dir, from, dist = "down", m.h*40/100, m.h*20/100 }
	forth := func() { dir, from, dist = "up", m.h*60/100, m.h*20/100 }
	still := 0
	for i := 0; i < 24; i++ {
		before := m.screen()
		if err := m.batch("swipe "+dir, mmStep{"mobile_swipe_on_screen", map[string]any{
			"direction": dir, "x": m.w / 2, "y": from, "distance": dist}}); err != nil {
			return err
		}
		at, err := m.where(target)
		if err != nil {
			return err
		}
		if still++; m.screen() != before {
			still = 0
		}
		switch {
		case at == 0:
			return nil
		case at < 0 && m.beyond:
			back() // under the header
		case at > 0 && m.beyond && dir == "down":
			forth() // below the band
		case still == 2 && dir == "up":
			back() // the end of the list
			still = 0
		case still == 2:
			forth() // its top
			still = 0
		}
	}
	return fmt.Errorf("%q not found after scrolling", target)
}

// where says where the target has come to rest: 0 on screen, -1 above the band that counts as
// on screen, +1 below it or not in the list.
func (m *mobileMCP) where(target string) (int, error) {
	e, ok, err := m.atRest(target)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 1, nil
	}
	top := 0.0
	if m.beyond {
		top = mmTopBand * float64(m.h)
	}
	switch c := float64(e.y + e.h/2); {
	case c <= top:
		return -1, nil
	case c < visibleBand*float64(m.h):
		return 0, nil
	}
	return 1, nil
}

// screen is the latest list as one string: equal before and after a swipe, nothing moved.
func (m *mobileMCP) screen() string {
	var b strings.Builder
	for _, e := range m.els {
		fmt.Fprintf(&b, "%s %d,%d|", strings.Join(e.labels, "/"), e.x, e.y)
	}
	return b.String()
}

func (m *mobileMCP) Read(sub string) (string, error) {
	for try := 0; ; try++ {
		if _, l, ok := findEl(m.els, contains(sub)); ok {
			return l, nil
		}
		if try == 1 {
			return "", fmt.Errorf("%q not on screen", sub)
		}
		if err := m.list(); err != nil {
			return "", err
		}
	}
}

func (m *mobileMCP) Screenshot(name string) error {
	_, err := m.call("mobile_save_screenshot "+name+".png", "mobile_save_screenshot",
		map[string]any{"saveTo": filepath.Join(m.tmp, name+".png")})
	return err
}
