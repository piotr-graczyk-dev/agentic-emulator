package android

import (
	"reflect"
	"testing"
)

func TestSnapshotCompressEnv(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"1", []string{"ANDROID_SNAPSHOT_COMPRESS=1"}},
		// "0" is passed on: unset would leave the emulator free to compress on its own.
		{"0", []string{"ANDROID_SNAPSHOT_COMPRESS=0"}},
		{"", nil},
		{"yes", nil},
	} {
		if got := snapshotCompressEnv(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("snapshotCompressEnv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
