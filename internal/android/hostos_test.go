package android

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
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

// The SDK's tools look for AVDs in several places; agemu must look where avdmanager writes them
// (a fresh Linux machine without ~/.android gets the XDG location).
func TestAVDHomeLikeTheSDK(t *testing.T) {
	j := filepath.Join
	none := func(string) bool { return false }
	xdgOnly := func(p string) bool { return p == j("/home/a", ".config", ".android", "avd") }
	for _, c := range []struct {
		name, goos string
		env        map[string]string
		isDir      func(string) bool
		want       string
	}{
		{"ANDROID_AVD_HOME", "linux", map[string]string{"ANDROID_AVD_HOME": "/x", "ANDROID_USER_HOME": "/u"}, none, "/x"},
		{"ANDROID_USER_HOME", "darwin", map[string]string{"ANDROID_USER_HOME": "/u"}, none, j("/u", "avd")},
		{"ANDROID_EMULATOR_HOME", "linux", map[string]string{"ANDROID_EMULATOR_HOME": "/e"}, none, j("/e", "avd")},
		{"ANDROID_SDK_HOME", "linux", map[string]string{"ANDROID_SDK_HOME": "/s"}, none, j("/s", ".android", "avd")},
		{"macOS default", "darwin", nil, none, j("/home/a", ".android", "avd")},
		{"Linux, nothing yet", "linux", nil, none, j("/home/a", ".android", "avd")},
		{"Linux, XDG only", "linux", nil, xdgOnly, j("/home/a", ".config", ".android", "avd")},
		{"Linux, XDG_CONFIG_HOME", "linux", map[string]string{"XDG_CONFIG_HOME": "/c"}, func(p string) bool { return p == j("/c", ".android", "avd") }, j("/c", ".android", "avd")},
		{"Linux, both", "linux", nil, func(string) bool { return true }, j("/home/a", ".android", "avd")},
	} {
		if got := avdHomeFor(c.goos, "/home/a", envMap(c.env), c.isDir); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestADBKeyPresent(t *testing.T) {
	home := t.TempDir()
	if adbKeyPresent(home, envMap(nil)) {
		t.Fatal("a key is reported in an empty home")
	}
	user := t.TempDir()
	if err := os.WriteFile(filepath.Join(user, "adbkey.pub"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !adbKeyPresent(home, envMap(map[string]string{"ANDROID_USER_HOME": user})) {
		t.Fatal("the key under ANDROID_USER_HOME is not found")
	}
	if err := os.MkdirAll(filepath.Join(home, ".android"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".android", "adbkey.pub"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !adbKeyPresent(home, envMap(nil)) {
		t.Fatal("the key in ~/.android is not found")
	}
}

// A forwarded TCP connection that the guest never answers (its network not up yet on a slow
// host; seen on a Linux CI emulator) must not use up the call's time: the agent is reached over
// adb within the same deadline.
func TestDialFallsBackToADBWhenTCPHangs(t *testing.T) {
	hang, err := net.Listen("tcp", "127.0.0.1:0") // accepts, never replies
	if err != nil {
		t.Fatal(err)
	}
	defer hang.Close()
	go func() {
		for {
			c, err := hang.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	adb, err := net.Listen("tcp", "127.0.0.1:0") // a fake adb server: OKAY to transport and service
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	go func() {
		for {
			c, err := adb.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				for i := 0; i < 2; i++ {
					var n [4]byte
					if _, err := io.ReadFull(c, n[:]); err != nil {
						return
					}
					l, _ := strconv.ParseUint(string(n[:]), 16, 32)
					if _, err := io.ReadFull(c, make([]byte, l)); err != nil {
						return
					}
					c.Write([]byte("OKAY"))
				}
			}(c)
		}
	}()
	t.Setenv("ANDROID_ADB_SERVER_PORT", strconv.Itoa(adb.Addr().(*net.TCPAddr).Port))
	port := hang.Addr().(*net.TCPAddr).Port
	a := &agentClient{serial: "emulator-5580", token: "t", tcp: func(context.Context, bool) (int, error) { return port, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, _, via, err := a.dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
	if via != "adb" {
		t.Fatalf("via %q, want adb", via)
	}
}
