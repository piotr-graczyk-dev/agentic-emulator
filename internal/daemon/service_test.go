package daemon

import (
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

func tree() *api.UITree {
	return &api.UITree{Width: 100, Height: 200, Root: &api.Element{Ref: "e0", Role: "window", Frame: api.Rect{W: 100, H: 200}, Children: []*api.Element{
		{Ref: "e1", Role: "button", Text: "Log in", ID: "com.app:id/login", Frame: api.Rect{X: 10, Y: 20, W: 30, H: 10}},
		{Ref: "e2", Role: "text", Label: "Welcome back", Frame: api.Rect{X: 0, Y: 0, W: 100, H: 10}},
		{Ref: "e3", Role: "button", Text: "Hidden", Frame: api.Rect{}},
	}}}
}

func TestFind(t *testing.T) {
	cases := []struct {
		sel  api.Selector
		want []string
	}{
		{api.Selector{Text: "Log in"}, []string{"e1"}},
		{api.Selector{Contains: "welcome"}, []string{"e2"}},
		{api.Selector{ID: "login"}, []string{"e1"}},
		{api.Selector{ID: "com.app:id/login"}, []string{"e1"}},
		{api.Selector{Role: "button"}, []string{"e1"}}, // zero-size element is skipped
		{api.Selector{Text: "Hidden"}, nil},
	}
	for _, c := range cases {
		got := Find(tree(), c.sel)
		if len(got) != len(c.want) {
			t.Fatalf("%+v: got %d matches, want %d", c.sel, len(got), len(c.want))
		}
		for i := range got {
			if got[i].Ref != c.want[i] {
				t.Fatalf("%+v: got %s want %s", c.sel, got[i].Ref, c.want[i])
			}
		}
	}
}

func TestCenter(t *testing.T) {
	x, y := api.Rect{X: 10, Y: 20, W: 30, H: 10}.Center()
	if x != 25 || y != 25 {
		t.Fatalf("center = %d,%d", x, y)
	}
}
