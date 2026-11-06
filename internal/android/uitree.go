package android

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Raw tree as produced by android-agent (Agent.tree). Bounds are logical display pixels,
// which are also agemu screenshot pixels (screenshots are rendered at the logical size).
type rawTree struct {
	W       int         `json:"w"`
	H       int         `json:"h"`
	Density int         `json:"density"`
	App     string      `json:"app"`
	Windows []rawWindow `json:"windows"`
}

type rawWindow struct {
	Type    int      `json:"type"`
	Layer   int      `json:"layer"`
	Active  bool     `json:"active"`
	Focused bool     `json:"focused"`
	Title   string   `json:"title"`
	B       [4]int   `json:"b"`
	Pkg     string   `json:"pkg"`
	Root    *rawNode `json:"root"`
}

type rawNode struct {
	Cls   string     `json:"cls"`
	Text  string     `json:"text"`
	Desc  string     `json:"desc"`
	ID    string     `json:"id"`
	Hint  string     `json:"hint"`
	State string     `json:"state"`
	B     [4]int     `json:"b"` // left, top, right, bottom
	F     int        `json:"f"`
	C     []*rawNode `json:"c"`
}

// Flag bits, mirrored from Agent.java.
const (
	fEnabled = 1 << iota
	fFocused
	fSelected
	fCheckable
	fChecked
	fClickable
	fLongClickable
	fScrollable
	fPassword
	fEditable
	fFocusable
	fShowingHint
)

// AccessibilityWindowInfo types.
var windowTypes = map[int]string{1: "application", 2: "input_method", 3: "system", 4: "accessibility_overlay", 5: "split_screen_divider", 6: "magnification_overlay"}

func parseTree(b []byte) (*api.UITree, error) {
	var rt rawTree
	if err := json.Unmarshal(b, &rt); err != nil {
		return nil, err
	}
	return mapTree(&rt), nil
}

// mapTree converts the raw agent tree to api.UITree. The root is a synthetic "window"
// element covering the screen; its children are the on-screen windows, top-most first.
func mapTree(rt *rawTree) *api.UITree {
	t := &api.UITree{App: rt.App, Width: rt.W, Height: rt.H}
	n := 0
	ref := func() string { s := "e" + strconv.Itoa(n); n++; return s }
	root := &api.Element{Ref: ref(), Role: "window", Type: "screen", Frame: api.Rect{W: rt.W, H: rt.H}, Enabled: true}
	for _, w := range rt.Windows {
		we := &api.Element{
			Ref: ref(), Role: "window", Type: "window:" + windowTypes[w.Type], Label: w.Title,
			Frame: rect(w.B), Enabled: true, Focused: w.Focused || w.Active, Value: w.Pkg,
		}
		if w.Root != nil {
			we.Children = []*api.Element{mapNode(w.Root, ref)}
		}
		root.Children = append(root.Children, we)
	}
	t.Root = root
	return t
}

func rect(b [4]int) api.Rect {
	w, h := b[2]-b[0], b[3]-b[1]
	if w < 0 || h < 0 {
		w, h = 0, 0
	}
	return api.Rect{X: b[0], Y: b[1], W: w, H: h}
}

func mapNode(r *rawNode, ref func() string) *api.Element {
	e := &api.Element{
		Ref:       ref(),
		Type:      r.Cls,
		Label:     r.Desc,
		ID:        r.ID,
		Frame:     rect(r.B),
		Enabled:   r.F&fEnabled != 0,
		Focused:   r.F&fFocused != 0,
		Selected:  r.F&fSelected != 0,
		Clickable: r.F&(fClickable|fLongClickable) != 0,
	}
	if r.F&fShowingHint == 0 {
		e.Text = r.Text
	}
	if e.Label == "" && r.Hint != "" {
		e.Label = r.Hint // what TalkBack reads for an empty field
	}
	if r.F&fCheckable != 0 {
		c := r.F&fChecked != 0
		e.Checked = &c
	}
	e.Role = role(r)
	if e.Role == "textfield" {
		e.Value = e.Text
	} else if r.State != "" {
		e.Value = r.State
	}
	for _, c := range r.C {
		e.Children = append(e.Children, mapNode(c, ref))
	}
	return e
}

// role normalizes the Android class name (and editable / checkable flags) into the
// platform-neutral roles shared with iOS.
func role(r *rawNode) string {
	cls := r.Cls
	short := cls[strings.LastIndexByte(cls, '.')+1:]
	switch {
	case r.F&fEditable != 0 || strings.Contains(short, "EditText") || strings.HasSuffix(short, "TextField"):
		return "textfield"
	case strings.Contains(short, "Switch") || short == "ToggleButton":
		return "switch"
	case strings.Contains(short, "CheckBox") || strings.HasSuffix(short, "CheckedTextView"):
		return "checkbox"
	case strings.Contains(short, "RadioButton"):
		return "radio"
	case strings.Contains(short, "SeekBar") || strings.Contains(short, "Slider"):
		return "slider"
	case strings.Contains(short, "ProgressBar"):
		return "progress"
	case strings.Contains(short, "Button"): // Button, ImageButton, MaterialButton, FloatingActionButton
		return "button"
	case strings.Contains(short, "ImageView") || short == "Image":
		return "image"
	case strings.Contains(short, "WebView"):
		return "webview"
	case strings.Contains(short, "RecyclerView") || strings.Contains(short, "ListView") || strings.Contains(short, "GridView"):
		return "list"
	case strings.Contains(short, "ScrollView") || strings.Contains(short, "ViewPager") || r.F&fScrollable != 0:
		return "scroll"
	case strings.Contains(short, "TabWidget") || strings.Contains(short, "TabLayout"):
		return "tabbar"
	case strings.Contains(short, "TextView") || short == "Text":
		if r.F&fClickable != 0 {
			return "button"
		}
		return "text"
	case r.F&fClickable != 0:
		// A clickable container (list row, Compose clickable): a tappable cell.
		return "cell"
	}
	return "other"
}
