package android

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotCompressEnv(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		// Compressed by default: unset would leave the choice to the emulator.
		{"", []string{"ANDROID_SNAPSHOT_COMPRESS=1"}},
		{"1", []string{"ANDROID_SNAPSHOT_COMPRESS=1"}},
		{"yes", []string{"ANDROID_SNAPSHOT_COMPRESS=1"}},
		// "0" is the only way to raw, and it is passed on: the emulator compresses by itself
		// when the host has little free RAM.
		{"0", []string{"ANDROID_SNAPSHOT_COMPRESS=0"}},
	} {
		if got := snapshotCompressEnv(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("snapshotCompressEnv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The emulator's environment (launch) must carry the setting even when the daemon inherited
// another ANDROID_SNAPSHOT_COMPRESS: checked with a real exec, which keeps the last value of a
// duplicate key.
func TestEmulatorEnvOverridesInheritedCompress(t *testing.T) {
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env binary")
	}
	for _, c := range []struct{ inherited, setting, want string }{
		{"0", "", "ANDROID_SNAPSHOT_COMPRESS=1"},
		{"1", "0", "ANDROID_SNAPSHOT_COMPRESS=0"},
	} {
		t.Setenv("AGEMU_ANDROID_SNAPSHOT_COMPRESS", c.setting)
		cmd := exec.Command(envBin)
		cmd.Env = emulatorEnv([]string{"PATH=/usr/bin:/bin", "ANDROID_SNAPSHOT_COMPRESS=" + c.inherited})
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "ANDROID_SNAPSHOT_COMPRESS=") {
				got = append(got, l)
			}
		}
		if !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("inherited %q, setting %q: the emulator sees %q, want [%s]", c.inherited, c.setting, got, c.want)
		}
	}
}
