//go:build !windows

package main

import (
	"strconv"
	"testing"
)

// The text mobile_list_elements_on_screen returns, also at the end of a mobile_batch_commands
// result (mobile-mcp 1.0.17, lib/format-elements.js).
const mmSample = `Step 1 (mobile_click_on_screen_at_coordinates): Clicked on element @e3
Step 2 (mobile_type_keys): Typed text: test@agemu.dev
One element per line: @ref Type text= label= name= value= id= at=x,y size=WxH [focused] [selected] [checked] [disabled]
@e1 TextView text="Agemu Shop" at=48,180 size=984x96
@e2 EditText text="test@agemu.dev" label="E-mail" at=48,420 size=984x132 focused
@e3 Button text="Sign in" id="dev.agemu.shop:id/sign_in" at=48,760.5 size=984x132
@e4 TextView text="Say \"hi\" · cart: 1" at=48,2600 size=400x60
StaticText label="No ref here" at=10,20 size=30x40 disabled`

func TestDeviceID(t *testing.T) {
	list := `{"devices":[{"id":"Pixel_9_Pro","name":"Pixel_9_Pro","platform":"android","type":"emulator"},` +
		`{"id":"ED6C55BD-83FB","name":"race-iphone-17-pro","platform":"ios","type":"simulator"}]}`
	for device, want := range map[string]string{"Pixel_9_Pro": "Pixel_9_Pro", "ED6C55BD-83FB": "ED6C55BD-83FB", "race-iphone-17-pro": "ED6C55BD-83FB"} {
		if got, err := deviceID(list, device); err != nil || got != want {
			t.Errorf("deviceID(%q) = %q, %v; want %q", device, got, err, want)
		}
	}
	if _, err := deviceID(list, "emulator-5600"); err == nil {
		t.Error("deviceID found a device that is not listed")
	}
}

// On Android the list has a text field's text but not its hint.
func TestHintlessFields(t *testing.T) {
	m := &mobileMCP{hintless: true, els: parseElements(`@e1 TextView text="Catalog" at=60,180 size=1160x120
@e2 EditText text="test@agemu.dev" value="test@agemu.dev" at=60,513 size=1160x152
@e3 EditText at=60,701 size=1160x152
@e4 EditText at=60,889 size=1160x152
@e5 Button text="Sign in" at=60,1086 size=1160x144`)}
	if e, err := m.field("Password"); err != nil || e.ref != "@e3" {
		t.Errorf("the first empty field: %+v, %v", e, err)
	}
	if e, err := m.field("Sign in"); err != nil || e.ref != "@e5" {
		t.Errorf("an element with the label: %+v, %v", e, err)
	}
	if !m.met(Cond{Text: "Search products"}) {
		t.Error("the catalog's title does not stand in for its search field's hint")
	}
	m.hintless = false
	if m.met(Cond{Text: "Search products"}) {
		t.Error("the stand-in is used where the list has labels")
	}
}

// On iOS the list has rows that are under the app's header or below the screen, and an element
// is used only where the list before had it too.
func TestWhereOnIOS(t *testing.T) {
	list := func(y int) []mmEl {
		return parseElements(`@e1 StaticText label="Catalog" name="Catalog" at=20,83 size=106x36
@e9 StaticText label="Bath Towel" name="Bath Towel" at=36,` + strconv.Itoa(y) + ` size=87x21`)
	}
	for y, want := range map[int]int{146: -1, 59: -1, 300: 0, 622: 0, 800: 1, 943: 1} {
		m := &mobileMCP{beyond: true, h: 874, els: list(y), prev: list(y + 1)} // a point off: at rest
		if got, err := m.where("Bath Towel"); err != nil || got != want {
			t.Errorf("where at y=%d: %d, %v; want %d", y, got, err, want)
		}
	}
	m := &mobileMCP{beyond: true, h: 874, els: list(300)}
	if got, _ := m.where("Desk Lamp"); got != 1 {
		t.Errorf("where for an element that is not in the list: %d, want 1", got)
	}
	a, b := list(300)[1], list(300)[1]
	b.ref = "@e8"
	if a.same(b) {
		t.Error("an element under another ref counts as the same")
	}
	// Android: the list has only what is on screen, and it is at rest when the call returns.
	m = &mobileMCP{h: 2856, els: list(146)}
	if got, _ := m.where("Bath Towel"); got != 0 {
		t.Errorf("where on Android at y=146: %d, want 0", got)
	}
}

func TestParseElements(t *testing.T) {
	els := parseElements(mmSample)
	if len(els) != 5 {
		t.Fatalf("got %d elements, want 5: %+v", len(els), els)
	}
	e, _, ok := findEl(els, exact("Sign in"))
	if !ok || e.ref != "@e3" || e.typ != "Button" || e.x != 48 || e.y != 760 || e.w != 984 || e.h != 132 {
		t.Errorf("Sign in: %+v, found %v", e, ok)
	}
	if e, _, ok := findEl(els, exact("E-mail")); !ok || e.ref != "@e2" {
		t.Errorf("E-mail by label: %+v, found %v", e, ok)
	}
	if _, l, ok := findEl(els, contains("cart: ")); !ok || l != `Say "hi" · cart: 1` {
		t.Errorf("contains: %q, found %v", l, ok)
	}
	if e, _, ok := findEl(els, exact("No ref here")); !ok || e.ref != "" || e.x != 10 {
		t.Errorf("element without a ref: %+v, found %v", e, ok)
	}
	m := &mobileMCP{els: els}
	for w, want := range map[Cond]bool{
		{Text: "Sign in"}:                      true,
		{Text: "Sign"}:                         false,
		{Contains: "cart: 1"}:                  true,
		{Gone: "Place order"}:                  true,
		{Gone: "Sign in"}:                      false,
		{Text: "Sign in", Contains: "cart: 2"}: false,
	} {
		if got := m.met(w); got != want {
			t.Errorf("met(%+v) = %v, want %v", w, got, want)
		}
	}
}
