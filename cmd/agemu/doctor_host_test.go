package main

import (
	"strings"
	"testing"
)

func TestHostCheck(t *testing.T) {
	for _, c := range []struct {
		goos, goarch string
		want         checkStatus
	}{
		{"darwin", "arm64", statusOK},
		{"darwin", "amd64", statusFail},
		{"linux", "amd64", statusOK},
		{"linux", "arm64", statusOK},
		{"windows", "amd64", statusOK},
		{"windows", "386", statusFail},
		{"freebsd", "amd64", statusFail},
	} {
		if got := hostCheck(c.goos, c.goarch); got.Status != c.want {
			t.Errorf("hostCheck(%s/%s) = %s (%s), want %s", c.goos, c.goarch, got.Status, got.Detail, c.want)
		}
	}
}

func TestAccelDetail(t *testing.T) {
	for out, want := range map[string]string{
		"accel:\n0\nKVM (version 12) is installed and usable.\naccel\n":                                   "KVM (version 12) is installed and usable.",
		"accel:\n0\nWHPX(10.0.22631) is installed and usable.\naccel\n":                                   "WHPX(10.0.22631) is installed and usable.",
		"accel:\n10\n/dev/kvm is not found: VT disabled in BIOS or KVM kernel module not loaded\naccel\n": "/dev/kvm is not found: VT disabled in BIOS or KVM kernel module not loaded",
		"": "",
	} {
		if got := accelDetail(out); got != want {
			t.Errorf("accelDetail(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestPerOSFixesNameTheRightTools(t *testing.T) {
	if !strings.Contains(installJavaFix("darwin"), "brew") || !strings.Contains(installJavaFix("windows"), "winget") ||
		!strings.Contains(installJavaFix("linux"), "apt") {
		t.Error("Java install fixes do not match their OS")
	}
	if !strings.Contains(strings.Join(installSDKFix("windows"), " "), "LOCALAPPDATA") ||
		!strings.Contains(strings.Join(installSDKFix("linux"), " "), "~/Android/Sdk") {
		t.Error("SDK install fixes do not name Android Studio's SDK location")
	}
	if !strings.Contains(strings.Join(accelFix("linux"), " "), "kvm") || !strings.Contains(strings.Join(accelFix("windows"), " "), "HypervisorPlatform") {
		t.Error("acceleration fixes do not match their OS")
	}
}
