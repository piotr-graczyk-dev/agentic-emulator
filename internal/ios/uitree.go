//go:build !windows

package ios

import (
	"fmt"
	"math"
	"strings"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// axNode is one accessibility element as agemu-ios-helper reports it. Frames are in
// iOS points, origin top-left: [x, y, w, h].
type axNode struct {
	Role        string    `json:"role"`
	Subrole     string    `json:"subrole"`
	Label       string    `json:"label"`
	ID          string    `json:"id"`
	Value       string    `json:"value"`
	Placeholder string    `json:"placeholder"`
	Title       string    `json:"title"`
	Frame       []float64 `json:"frame"`
	Enabled     *bool     `json:"enabled"`
	Selected    bool      `json:"selected"`
	Focused     bool      `json:"focused"`
	Children    []*axNode `json:"children"`
}

// axTree is the helper's "ui" result.
type axTree struct {
	Pid      int     `json:"pid"`
	BundleID string  `json:"bundleId"` // the frontmost app, from accessibility
	Root     *axNode `json:"root"`
	Scale    float64 `json:"scale"`
	Width    int     `json:"width"`  // screen pixels
	Height   int     `json:"height"` // screen pixels
	Elements int     `json:"elements"`
	Requests int     `json:"requests"`
}

// PixelRect converts a frame in points to screenshot pixels. Edges are rounded
// independently so adjacent elements stay adjacent.
func PixelRect(f []float64, scale float64) api.Rect {
	if len(f) != 4 {
		return api.Rect{}
	}
	if scale <= 0 {
		scale = 1
	}
	x1 := math.Round(f[0] * scale)
	y1 := math.Round(f[1] * scale)
	x2 := math.Round((f[0] + f[2]) * scale)
	y2 := math.Round((f[1] + f[3]) * scale)
	return api.Rect{X: int(x1), Y: int(y1), W: int(x2 - x1), H: int(y2 - y1)}
}

// PointToPixel converts one coordinate from points to pixels.
func PointToPixel(v, scale float64) int { return int(math.Round(v * scale)) }

// normalizeRole maps AX roles (the macOS-side translation of UIKit accessibility) to agemu's
// platform-neutral roles.
func normalizeRole(role, subrole string) string {
	switch subrole {
	case "AXSwitch", "AXToggle":
		return "switch"
	case "AXSearchField", "AXSecureTextField":
		return "textfield"
	case "AXTabButton":
		return "tab"
	}
	switch role {
	case "AXButton", "AXPopUpButton", "AXMenuButton", "AXDisclosureTriangle":
		return "button"
	case "AXStaticText", "AXHeading":
		return "text"
	case "AXTextField", "AXTextArea", "AXSecureTextField", "AXComboBox", "AXSearchField":
		return "textfield"
	case "AXImage":
		return "image"
	case "AXCheckBox":
		return "checkbox"
	case "AXSwitch", "AXToggle":
		return "switch"
	case "AXRadioButton":
		return "radio"
	case "AXLink":
		return "link"
	case "AXSlider":
		return "slider"
	case "AXIncrementor", "AXStepper":
		return "stepper"
	case "AXProgressIndicator", "AXBusyIndicator", "AXLevelIndicator":
		return "progress"
	case "AXCell", "AXRow":
		return "cell"
	case "AXTable", "AXList", "AXOutline", "AXCollection", "AXGrid":
		return "list"
	case "AXScrollArea":
		return "scroll"
	case "AXTabGroup", "AXTabBar":
		return "tabbar"
	case "AXToolbar":
		return "toolbar"
	case "AXMenuItem":
		return "menuitem"
	case "AXApplication":
		return "application"
	case "AXWindow", "AXSheet", "AXDialog":
		return "window"
	case "AXGroup":
		return "group"
	case "AXKeyboard":
		return "keyboard"
	}
	return "other"
}

var clickableRoles = map[string]bool{
	"button": true, "link": true, "cell": true, "switch": true, "checkbox": true, "radio": true,
	"textfield": true, "tab": true, "menuitem": true, "slider": true, "stepper": true,
}

// convertTree maps the helper's tree to api.UITree in screenshot pixels. Elements that lie
// entirely outside the screen (e.g. the previous page during a navigation transition) are
// dropped unless a descendant is visible; zero-sized leaves are dropped too.
func convertTree(t *axTree, app string) *api.UITree {
	out := &api.UITree{App: app, Width: t.Width, Height: t.Height}
	screen := api.Rect{W: t.Width, H: t.Height}
	n := 0
	var conv func(a *axNode) *api.Element
	conv = func(a *axNode) *api.Element {
		if a == nil {
			return nil
		}
		e := convertNode(a, t.Scale)
		n++
		e.Ref = fmt.Sprintf("e%d", n)
		for _, c := range a.Children {
			if ce := conv(c); ce != nil {
				e.Children = append(e.Children, ce)
			}
		}
		if len(e.Children) == 0 && !visible(e.Frame, screen) && a != t.Root {
			n-- // keep refs dense
			return nil
		}
		return e
	}
	out.Root = conv(t.Root)
	return out
}

func visible(r, screen api.Rect) bool {
	if r.W <= 0 || r.H <= 0 {
		return false
	}
	if screen.W <= 0 || screen.H <= 0 {
		return true
	}
	return r.X < screen.X+screen.W && r.X+r.W > screen.X && r.Y < screen.Y+screen.H && r.Y+r.H > screen.Y
}

func convertNode(a *axNode, scale float64) *api.Element {
	role := normalizeRole(a.Role, a.Subrole)
	typ := a.Role
	if a.Subrole != "" {
		typ += ":" + a.Subrole
	}
	value := a.Value
	if role == "textfield" && a.Placeholder != "" && value == a.Placeholder {
		value = "" // iOS reports the placeholder as the value of an empty field
	}
	e := &api.Element{
		Role:     role,
		Type:     typ,
		Label:    a.Label,
		ID:       a.ID,
		Frame:    PixelRect(a.Frame, scale),
		Enabled:  a.Enabled == nil || *a.Enabled,
		Focused:  a.Focused,
		Selected: a.Selected,
	}
	switch role {
	case "textfield":
		e.Text = value
		e.Value = value
		if e.Label == "" && a.Placeholder != "" {
			e.Label = a.Placeholder
		}
	case "text":
		e.Text = a.Label
		if e.Text == "" {
			e.Text = value
		}
		if value != e.Text {
			e.Value = value
		}
	default:
		e.Text = a.Title
		e.Value = value
	}
	switch role {
	case "switch", "checkbox", "radio":
		switch strings.ToLower(value) {
		case "1", "on", "true":
			v := true
			e.Checked = &v
		case "0", "off", "false":
			v := false
			e.Checked = &v
		}
	}
	e.Clickable = clickableRoles[role]
	return e
}
