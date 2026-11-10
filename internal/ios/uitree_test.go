//go:build !windows

package ios

import (
	"encoding/json"
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/profiles"
)

func TestPixelRect(t *testing.T) {
	cases := []struct {
		frame []float64
		scale float64
		want  api.Rect
	}{
		{[]float64{16, 380.3333333333333, 370, 52}, 3, api.Rect{X: 48, Y: 1141, W: 1110, H: 156}},
		{[]float64{0, 0, 402, 874}, 3, api.Rect{X: 0, Y: 0, W: 1206, H: 2622}},
		{[]float64{0, 0, 375, 667}, 2, api.Rect{X: 0, Y: 0, W: 750, H: 1334}},
		// Edges round independently: two adjacent 1/3-point frames stay adjacent.
		{[]float64{10.3333333, 0, 20.3333333, 1}, 3, api.Rect{X: 31, Y: 0, W: 61, H: 3}},
		{[]float64{30.6666666, 0, 10, 1}, 3, api.Rect{X: 92, Y: 0, W: 30, H: 3}},
		{[]float64{1, 2, 3}, 3, api.Rect{}},
		{[]float64{5, 5, 10, 10}, 0, api.Rect{X: 5, Y: 5, W: 10, H: 10}},
	}
	for _, c := range cases {
		if got := PixelRect(c.frame, c.scale); got != c.want {
			t.Errorf("PixelRect(%v, %v) = %+v, want %+v", c.frame, c.scale, got, c.want)
		}
	}
	if PointToPixel(201.5, 2) != 403 {
		t.Errorf("PointToPixel")
	}
}

func TestNormalizeRole(t *testing.T) {
	cases := map[[2]string]string{
		{"AXButton", ""}:                 "button",
		{"AXStaticText", ""}:             "text",
		{"AXHeading", ""}:                "text",
		{"AXTextField", "AXSearchField"}: "textfield",
		{"AXTextField", ""}:              "textfield",
		{"AXCheckBox", "AXSwitch"}:       "switch",
		{"AXCheckBox", ""}:               "checkbox",
		{"AXImage", ""}:                  "image",
		{"AXCell", ""}:                   "cell",
		{"AXGroup", ""}:                  "group",
		{"AXApplication", ""}:            "application",
		{"AXSomethingNew", ""}:           "other",
	}
	for in, want := range cases {
		if got := normalizeRole(in[0], in[1]); got != want {
			t.Errorf("normalizeRole(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// A trimmed real response from agemu-ios-helper for the iOS 26.5 Settings app.
const settingsJSON = `{
  "pid": 13783, "scale": 3, "width": 1206, "height": 2622,
  "root": {"role": "AXApplication", "label": "Settings", "enabled": true, "frame": [0, 0, 402, 874], "children": [
    {"role": "AXHeading", "label": "Settings", "enabled": true, "frame": [16, 119.66666666666667, 133, 40.66666666666667]},
    {"role": "AXGroup", "enabled": true, "frame": [0, 0, 402, 874], "children": [
      {"role": "AXButton", "label": "General", "id": "com.apple.settings.general", "enabled": true, "frame": [16, 380.3333333333333, 370, 52]},
      {"role": "AXButton", "label": "Offscreen", "enabled": true, "frame": [-500, 380, 370, 52]},
      {"role": "AXButton", "label": "Zero", "enabled": true, "frame": [0, 0, 0, 0]},
      {"role": "AXCheckBox", "subrole": "AXSwitch", "label": "Airplane Mode", "value": "1", "enabled": false, "frame": [16, 440, 370, 52]}
    ]},
    {"role": "AXGroup", "label": "Toolbar", "id": "Toolbar", "enabled": true, "frame": [0, 788, 402, 86], "children": [
      {"role": "AXTextField", "subrole": "AXSearchField", "value": "Search", "placeholder": "Search", "enabled": true, "focused": true, "frame": [33, 803, 336, 38]}
    ]}
  ]}
}`

func TestConvertTree(t *testing.T) {
	var ax axTree
	if err := json.Unmarshal([]byte(settingsJSON), &ax); err != nil {
		t.Fatal(err)
	}
	tree := convertTree(&ax, "com.apple.Preferences")
	if tree.App != "com.apple.Preferences" || tree.Width != 1206 || tree.Height != 2622 {
		t.Fatalf("tree header: %+v", tree)
	}
	var all []*api.Element
	tree.Walk(func(e *api.Element) { all = append(all, e) })
	// root, heading, group, General, switch, toolbar, field: offscreen and zero-size leaves dropped.
	if len(all) != 7 {
		for _, e := range all {
			t.Logf("%s %s %q %+v", e.Ref, e.Role, e.Label, e.Frame)
		}
		t.Fatalf("got %d elements, want 7", len(all))
	}
	for i, e := range all {
		if want := "e" + itoa(i+1); e.Ref != want {
			t.Errorf("element %d has ref %s, want %s (refs must be dense)", i, e.Ref, want)
		}
	}

	general := daemon.Find(tree, api.Selector{Text: "General"})
	if len(general) != 1 {
		t.Fatalf("find General: %d matches", len(general))
	}
	g := general[0]
	if g.Role != "button" || g.ID != "com.apple.settings.general" || !g.Clickable || !g.Enabled {
		t.Errorf("General: %+v", g)
	}
	if g.Frame != (api.Rect{X: 48, Y: 1141, W: 1110, H: 156}) {
		t.Errorf("General frame in pixels: %+v", g.Frame)
	}
	if x, y := g.Frame.Center(); x != 603 || y != 1219 {
		t.Errorf("General center: %d,%d", x, y)
	}
	if len(daemon.Find(tree, api.Selector{ID: "com.apple.settings.general"})) != 1 {
		t.Errorf("find by id")
	}

	heading := daemon.Find(tree, api.Selector{Role: "text", Text: "Settings"})
	if len(heading) != 1 || heading[0].Type != "AXHeading" || heading[0].Text != "Settings" {
		t.Errorf("heading: %+v", heading)
	}

	sw := daemon.Find(tree, api.Selector{Role: "switch"})
	if len(sw) != 1 || sw[0].Checked == nil || !*sw[0].Checked || sw[0].Enabled {
		t.Errorf("switch: %+v", sw)
	}

	fields := daemon.Find(tree, api.Selector{Role: "textfield"})
	if len(fields) != 1 {
		t.Fatalf("textfield: %d", len(fields))
	}
	f := fields[0]
	if f.Text != "" || f.Value != "" || f.Label != "Search" || !f.Focused || f.Type != "AXTextField:AXSearchField" {
		t.Errorf("empty search field must not report its placeholder as text: %+v", f)
	}

	if len(daemon.Find(tree, api.Selector{Text: "Offscreen"})) != 0 || len(daemon.Find(tree, api.Selector{Text: "Zero"})) != 0 {
		t.Errorf("offscreen / zero-size elements should be dropped")
	}
}

func TestTypedFieldValue(t *testing.T) {
	n := &axNode{Role: "AXTextField", Subrole: "AXSearchField", Value: "Wallpaper", Placeholder: "Search", Frame: []float64{33, 803, 276, 38}}
	e := convertNode(n, 3)
	if e.Text != "Wallpaper" || e.Value != "Wallpaper" || e.Label != "Search" {
		t.Errorf("typed field: %+v", e)
	}
}

func TestProfilesMatchDeviceTypes(t *testing.T) {
	// Pixel sizes as listed in each device type's profile.plist (mainScreenWidth/Height/Scale),
	// Xcode 26.6; checked against real screenshots for iphone-17-pro, iphone-se-3 and ipad-mini.
	want := map[string][3]int{
		"iphone-17-pro-max": {1320, 2868, 480},
		"iphone-17-pro":     {1206, 2622, 480},
		"iphone-17":         {1206, 2622, 480},
		"iphone-air":        {1260, 2736, 480},
		"iphone-16e":        {1170, 2532, 480},
		"iphone-se-3":       {750, 1334, 320},
		"ipad-mini":         {1488, 2266, 320},
		"ipad-pro-11":       {1668, 2420, 320},
	}
	got := profiles.All(api.IOS)
	if len(got) != len(want) {
		t.Fatalf("%d iOS profiles, want %d", len(got), len(want))
	}
	for _, p := range got {
		w, ok := want[p.Name]
		if !ok {
			t.Errorf("unexpected profile %s", p.Name)
			continue
		}
		if p.Width != w[0] || p.Height != w[1] || p.Density != w[2] {
			t.Errorf("%s: %dx%d@%d, want %dx%d@%d", p.Name, p.Width, p.Height, p.Density, w[0], w[1], w[2])
		}
		if profileFor(p.DeviceType) != p.Name {
			t.Errorf("profileFor(%s) = %q", p.DeviceType, profileFor(p.DeviceType))
		}
	}
}

func TestTypeable(t *testing.T) {
	if !typeable("Hello, World! 123 ~`[]{}\n\t") {
		t.Error("ASCII should be typeable")
	}
	for _, s := range []string{"zażółć", "emoji 😀", "\x01"} {
		if typeable(s) {
			t.Errorf("%q should not be typeable", s)
		}
	}
}

func TestUDIDOf(t *testing.T) {
	if u, err := udidOf("ios:ABC-123"); err != nil || u != "ABC-123" {
		t.Errorf("udidOf: %q %v", u, err)
	}
	for _, bad := range []string{"android:x", "ios:", "ABC"} {
		if _, err := udidOf(bad); err == nil {
			t.Errorf("udidOf(%q) should fail", bad)
		}
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("26.4", "26.5") || versionLess("26.10", "26.9") || !versionLess("18.6", "26.0") {
		t.Error("versionLess")
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
