package android

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// A login form: the e-mail field at y 378-513, the password field at y 545-680, a button below.
func formTree(focused string) *api.UITree {
	field := func(label string, y int) *api.Element {
		return &api.Element{Role: "textfield", Label: label, Frame: api.Rect{X: 53, Y: y, W: 974, H: 135}, Focused: label == focused}
	}
	return &api.UITree{Root: &api.Element{Role: "window", Type: "screen", Children: []*api.Element{
		{Role: "window", Label: "status bar", Children: []*api.Element{
			{Role: "textfield", Label: "behind", Frame: api.Rect{W: 1080, H: 2424}},
		}},
		{Role: "window", Label: "app", Focused: true, Children: []*api.Element{
			{Role: "other", Frame: api.Rect{W: 1080, H: 2424}, Children: []*api.Element{
				field("E-mail", 378), field("Password", 545),
				{Role: "button", Text: "Sign in", Frame: api.Rect{X: 53, Y: 797, W: 974, H: 126}},
			}},
		}},
	}}}
}

func TestFieldAt(t *testing.T) {
	tr := formTree("E-mail")
	if f := fieldAt(tr, 540, 612); f == nil || f.Label != "Password" {
		t.Errorf("at the password field: %+v", f)
	}
	if f := fieldAt(tr, 540, 445); f == nil || f.Label != "E-mail" || !f.Focused {
		t.Errorf("at the e-mail field: %+v", f)
	}
	if f := fieldAt(tr, 540, 860); f != nil {
		t.Errorf("at the button: %+v, want no text field (the one in the window behind does not count)", f)
	}
}

func TestAwaitTapFocus(t *testing.T) {
	onPassword := tapMark{x: 540, y: 612, at: time.Now()}
	reads := 0
	read := func(focusedFrom int) func() (*api.UITree, error) {
		reads = 0
		return func() (*api.UITree, error) {
			reads++
			if reads >= focusedFrom {
				return formTree("Password"), nil
			}
			return formTree("E-mail"), nil
		}
	}
	ctx := context.Background()

	if awaitTapFocus(ctx, onPassword, read(3)) || reads != 3 {
		t.Errorf("the focus arrived at the third read: %d reads", reads)
	}
	if awaitTapFocus(ctx, onPassword, read(1)) || reads != 1 {
		t.Errorf("a field that has the focus: %d reads, want 1", reads)
	}
	if awaitTapFocus(ctx, tapMark{x: 540, y: 860, at: time.Now()}, read(99)) || reads != 1 {
		t.Errorf("a tap on a button: %d reads, want 1", reads)
	}
	if awaitTapFocus(ctx, tapMark{x: 540, y: 612, at: time.Now().Add(-3 * time.Second)}, read(99)) || reads != 0 {
		t.Errorf("an old tap: %d reads, want none", reads)
	}
	if awaitTapFocus(ctx, tapMark{}, read(99)) || reads != 0 {
		t.Errorf("no tap yet: %d reads, want none", reads)
	}
	start := time.Now()
	if !awaitTapFocus(ctx, onPassword, read(1<<30)) {
		t.Error("a field that never takes the focus: the wait did not report that it gave up")
	}
	if took := time.Since(start); took < focusWaitFor || took > focusWaitFor+500*time.Millisecond {
		t.Errorf("gave up after %v, want about %v", took, focusWaitFor)
	}
	reads = 0
	failing := func() (*api.UITree, error) { reads++; return nil, errors.New("no tree") }
	if awaitTapFocus(ctx, tapMark{x: 540, y: 612, at: time.Now()}, failing) || reads != 1 {
		t.Errorf("a failed read: %d reads, want 1 and no wait", reads)
	}
}
