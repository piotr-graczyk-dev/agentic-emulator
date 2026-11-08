package android

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalIDUsesTheAVDsOwnSpelling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", dir)
	for _, n := range []string{"agemu-lab.ini", "agemu-Pixel.ini", "agemu-twin.ini", "agemu-TWIN.ini"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A case-insensitive disk (the macOS default) cannot hold both twins; skip that case there.
	entries, _ := os.ReadDir(dir)
	twins := len(entries) == 4
	b := &Backend{}
	cases := map[string]string{
		"android:agemu-lab":   "android:agemu-lab",
		"android:Agemu-LAB":   "android:agemu-lab",
		"android:agemu-pixel": "android:agemu-Pixel",
		"android:agemu-none":  "android:agemu-none",
		"ios:agemu-lab":       "ios:agemu-lab",
	}
	if twins {
		cases["android:agemu-twin"] = "android:agemu-twin" // an exact match wins
		cases["android:Agemu-Twin"] = "android:Agemu-Twin" // ambiguous: left as given
	}
	for in, want := range cases {
		if got := b.CanonicalID(in); got != want {
			t.Errorf("CanonicalID(%q) = %q, want %q", in, got, want)
		}
	}
}
