package android

import (
	"path/filepath"
	"reflect"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The emulator writes its discovery files somewhere different on each host OS; agemu must read
// every place the emulator may have picked.
func TestDiscoveryDirsPerOS(t *testing.T) {
	j := filepath.Join
	for _, c := range []struct {
		name, goos, home string
		env              map[string]string
		want             []string
	}{
		{"macOS", "darwin", "/Users/a", nil,
			[]string{j("/Users/a", "Library", "Caches", "TemporaryItems", "avd", "running")}},
		{"Linux with XDG_RUNTIME_DIR", "linux", "/home/a", map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "USER": "a"},
			[]string{j("/run/user/1000", "avd", "running"), j("/tmp", "android-a", "avd", "running")}},
		{"Linux without", "linux", "/home/a", map[string]string{"USER": "a"},
			[]string{j("/tmp", "android-a", "avd", "running")}},
		{"Linux with TMPDIR", "linux", "/home/a", map[string]string{"USER": "a", "TMPDIR": "/var/tmp"},
			[]string{j("/var/tmp", "android-a", "avd", "running"), j("/tmp", "android-a", "avd", "running")}},
		{"Windows", "windows", `C:\Users\a`, map[string]string{"LOCALAPPDATA": `C:\Users\a\AppData\Local`, "TEMP": `C:\Users\a\AppData\Local\Temp`, "TMP": `D:\tmp`},
			[]string{j(`C:\Users\a\AppData\Local`, "Temp", "avd", "running"), j(`D:\tmp`, "avd", "running")}},
	} {
		got := discoveryDirsFor(c.goos, c.home, envMap(c.env))
		if c.goos == "windows" && filepath.Separator != '\\' {
			// On a Unix test host filepath.Join keeps the backslashes as name characters, so the
			// two spellings of %TEMP% are different strings: compare the count and the first.
			if len(got) < 2 || got[0] != c.want[0] {
				t.Errorf("%s: %q, want %q first", c.name, got, c.want[0])
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDiscoveryDirOverride(t *testing.T) {
	t.Setenv("ANDROID_EMULATOR_DISCOVERY_DIR", "/x/running")
	if got := discoveryDirs(); !reflect.DeepEqual(got, []string{"/x/running"}) {
		t.Fatalf("discoveryDirs() = %q", got)
	}
}

// A Linux server or CI runner has no display for the host GPU; the emulator then needs software
// rendering, or it fails to start.
func TestDefaultGPU(t *testing.T) {
	for _, c := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"darwin", nil, "host"},
		{"windows", nil, "host"},
		{"linux", nil, "auto"},
		{"linux", map[string]string{"DISPLAY": ":0"}, "host"},
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, "host"},
	} {
		if got := defaultGPU(c.goos, envMap(c.env)); got != c.want {
			t.Errorf("defaultGPU(%s, %v) = %q, want %q", c.goos, c.env, got, c.want)
		}
	}
}

func TestDefaultSHM(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "1", "linux": "1", "windows": "0"} {
		if got := defaultSHM(goos); got != want {
			t.Errorf("defaultSHM(%s) = %q, want %q", goos, got, want)
		}
	}
}
