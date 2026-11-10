//go:build !windows

package ios

import (
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

func treeOf(els ...*api.Element) *api.UITree {
	return &api.UITree{Root: &api.Element{Role: "application", Frame: api.Rect{W: 1206, H: 2622}, Children: els}}
}

func button(label string, x, y int) *api.Element {
	return &api.Element{Role: "button", Label: label, Frame: api.Rect{X: x, Y: y, W: 200, H: 60}}
}

func TestFreshElementIsOneTheAgentCouldNotSeeBeforeTheLastInput(t *testing.T) {
	b := &Backend{seen: map[string]*seenElements{}}
	const udid = "U"
	show := button("Show alert", 10, 100)
	ok := button("OK", 300, 1400)

	// No read before any input: everything may be new.
	if !b.freshElement(udid, show) {
		t.Fatal("with no read before the input, an element must count as fresh")
	}
	// The agent reads the screen, taps "Show alert" (input), and reads the alert.
	b.noteTree(udid, treeOf(show))
	b.noteInput(udid)
	b.noteTree(udid, treeOf(ok))
	if !b.freshElement(udid, ok) {
		t.Fatal("the alert's button appeared after the input: fresh")
	}
	if b.freshElement(udid, show) {
		t.Fatal("the button read before the input is not fresh")
	}
	// Tapping OK is the next input: OK was read before it, so a second tap on it is not fresh.
	b.noteInput(udid)
	if b.freshElement(udid, ok) {
		t.Fatal("OK was read before the second input: not fresh")
	}
	// An element that moved is another element.
	if !b.freshElement(udid, button("OK", 300, 1200)) {
		t.Fatal("a moved element must count as fresh")
	}
	// Two inputs without a read in between: what appeared between them was never read.
	b.noteTree(udid, treeOf(show))
	b.noteInput(udid)
	b.noteInput(udid)
	if b.freshElement(udid, show) {
		t.Fatal("the last read before both inputs still counts")
	}
	// Devices are separate.
	if !b.freshElement("other", show) {
		t.Fatal("another device has no reads")
	}
}

func TestElementTapParams(t *testing.T) {
	el := button("OK", 300, 1400)
	x, y := el.TapPoint()
	p := elementTapParams("U", el, x, y, false, true)
	if p["x"] != 400 || p["y"] != 1430 {
		t.Fatalf("centre: %v", p)
	}
	if r, ok := p["ready"].([]int); !ok || len(r) != 4 || r[0] != 300 || r[1] != 1400 || r[2] != 200 || r[3] != 60 {
		t.Fatalf("a fresh element must send its frame as ready: %v", p)
	}
	if _, ok := p["settleMaxMs"]; ok {
		t.Fatalf("an element not at rest keeps the helper's settle: %v", p)
	}
	p = elementTapParams("U", el, x, y, true, false)
	if _, ok := p["ready"]; ok {
		t.Fatalf("an element the agent could see is tapped as before: %v", p)
	}
	if p["settleMaxMs"] != 0 {
		t.Fatalf("an element at rest skips the screen-wide settle: %v", p)
	}
}
