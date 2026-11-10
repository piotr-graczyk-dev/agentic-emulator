package api

import "testing"

func TestTapPoint(t *testing.T) {
	for _, c := range []struct {
		name string
		el   Element
		x, y int
	}{
		// Frames read from the Expo dev menu (iPhone 17 Pro, iOS 26.5).
		{"button keeps its centre", Element{Role: "button", Frame: Rect{X: 70, Y: 2348, W: 1066, H: 133}}, 603, 2414},
		{"SwiftUI Toggle row: its trailing switch", Element{Role: "switch", Frame: Rect{X: 492, Y: 2436, W: 598, H: 80}}, 1010, 2476},
		{"UIKit switch keeps its centre", Element{Role: "switch", Frame: Rect{X: 900, Y: 600, W: 153, H: 93}}, 976, 646},
		{"wide element that is not a switch keeps its centre", Element{Role: "other", Frame: Rect{X: 492, Y: 2436, W: 598, H: 80}}, 791, 2476},
	} {
		x, y := c.el.TapPoint()
		if x != c.x || y != c.y {
			t.Errorf("%s: (%d,%d), want (%d,%d)", c.name, x, y, c.x, c.y)
		}
	}
}
