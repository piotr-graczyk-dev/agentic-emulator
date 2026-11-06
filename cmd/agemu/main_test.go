package main

import "testing"

func TestParse(t *testing.T) {
	c := parse([]string{"-d", "android:x", "--text", "Log in", "--index=2", "100", "-5", "--cold"})
	if c.flags["device"] != "android:x" || c.flags["text"] != "Log in" || c.flags["index"] != "2" || c.flags["cold"] != "true" {
		t.Fatalf("flags: %#v", c.flags)
	}
	if len(c.pos) != 2 || c.pos[0] != "100" || c.pos[1] != "-5" {
		t.Fatalf("pos: %#v", c.pos)
	}
	s := c.selector()
	if s == nil || s.Text != "Log in" || s.Index != 2 {
		t.Fatalf("selector: %#v", s)
	}
}
