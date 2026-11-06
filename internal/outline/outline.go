// Package outline renders a UI tree as compact text that is cheap for an LLM
// agent to read: one line per meaningful element, indented by depth.
package outline

import (
	"fmt"
	"strings"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Render prints the tree, skipping anonymous layout containers that carry no
// text, label, id or interactivity (their children are kept, de-indented).
func Render(t *api.UITree) string {
	var b strings.Builder
	fmt.Fprintf(&b, "screen %dx%d", t.Width, t.Height)
	if t.App != "" {
		fmt.Fprintf(&b, " app=%s", t.App)
	}
	b.WriteByte('\n')
	var rec func(e *api.Element, depth int)
	rec = func(e *api.Element, depth int) {
		if e == nil {
			return
		}
		next := depth
		if meaningful(e) {
			b.WriteString(strings.Repeat("  ", depth))
			b.WriteString(Line(e))
			b.WriteByte('\n')
			next = depth + 1
		}
		for _, c := range e.Children {
			rec(c, next)
		}
	}
	rec(t.Root, 0)
	return b.String()
}

func meaningful(e *api.Element) bool {
	return e.Text != "" || e.Label != "" || e.ID != "" || e.Value != "" || e.Clickable ||
		e.Role == "button" || e.Role == "textfield" || e.Role == "switch" || e.Checked != nil
}

// Line renders one element: [ref] role "text" label=... id=... @(x,y wxh) flags
func Line(e *api.Element) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", e.Ref, e.Role)
	if e.Text != "" {
		fmt.Fprintf(&b, " %q", e.Text)
	}
	if e.Label != "" && e.Label != e.Text {
		fmt.Fprintf(&b, " label=%q", e.Label)
	}
	if e.Value != "" && e.Value != e.Text {
		fmt.Fprintf(&b, " value=%q", e.Value)
	}
	if e.ID != "" {
		fmt.Fprintf(&b, " id=%s", e.ID)
	}
	fmt.Fprintf(&b, " @(%d,%d %dx%d)", e.Frame.X, e.Frame.Y, e.Frame.W, e.Frame.H)
	if !e.Enabled {
		b.WriteString(" disabled")
	}
	if e.Focused {
		b.WriteString(" focused")
	}
	if e.Selected {
		b.WriteString(" selected")
	}
	if e.Checked != nil {
		if *e.Checked {
			b.WriteString(" checked")
		} else {
			b.WriteString(" unchecked")
		}
	}
	return b.String()
}
