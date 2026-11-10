package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The parts of doctor that differ per host OS. agemu runs on Apple Silicon Macs (iOS and
// Android) and on Linux and Windows (Android). Each takes the OS so tests cover every host.

func osName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return goos
}

// hostCheck says whether agemu runs on this OS and CPU.
func hostCheck(goos, goarch string) check {
	switch goos {
	case "darwin":
		c := check{Name: "Apple Silicon Mac", Status: statusOK}
		ver, _ := output(5*time.Second, "sw_vers", "-productVersion")
		c.Detail = strings.TrimSpace("macOS " + ver + " (" + goarch + ")")
		if goarch != "arm64" {
			c.Status, c.Detail = statusFail, "an Intel Mac: agemu runs on Apple Silicon Macs (and on Linux and Windows for Android)"
		}
		return c
	case "linux", "windows":
		c := check{Name: "host", Status: statusOK, Detail: fmt.Sprintf("%s (%s); Android only, the iOS Simulator needs macOS", osName(goos), goarch)}
		if goarch != "amd64" && goarch != "arm64" {
			c.Status, c.Detail = statusFail, fmt.Sprintf("%s on %s: agemu supports x86-64 and arm64 hosts", osName(goos), goarch)
		}
		return c
	}
	return check{Name: "host", Status: statusFail, Detail: fmt.Sprintf("%s/%s: agemu runs on macOS (Apple Silicon), Linux and Windows", goos, goarch)}
}

// installSDKFix is how to get an Android SDK agemu finds by itself.
func installSDKFix(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"brew install --cask android-commandlinetools temurin    # agemu finds this SDK by itself"}
	case "windows":
		return []string{
			"Install Android Studio (https://developer.android.com/studio) and open it once: it puts the SDK in %LOCALAPPDATA%\\Android\\Sdk, where agemu finds it",
			"Or unpack the command-line tools (https://developer.android.com/studio#command-tools) to <sdk>\\cmdline-tools\\latest and set ANDROID_HOME=<sdk>",
		}
	}
	return []string{
		"Install Android Studio (https://developer.android.com/studio) and open it once: it puts the SDK in ~/Android/Sdk, where agemu finds it",
		"Or unpack the command-line tools (https://developer.android.com/studio#command-tools) to <sdk>/cmdline-tools/latest and export ANDROID_HOME=<sdk>",
	}
}

// installJavaFix installs a Java 17+ runtime for the SDK's Java tools.
func installJavaFix(goos string) string {
	switch goos {
	case "darwin":
		return "brew install --cask temurin"
	case "windows":
		return "winget install EclipseAdoptium.Temurin.21.JDK"
	}
	return "sudo apt install openjdk-21-jdk-headless    # or your distribution's JDK 17+ package"
}

// accelCheck asks the emulator whether the hypervisor it needs is usable (`emulator
// -accel-check`): KVM on Linux, WHPX (Hyper-V's Windows Hypervisor Platform) or AEHD on
// Windows. Without it the emulator refuses to run an x86-64 or arm64 image. macOS always has
// Hypervisor.framework, so doctor does not ask there.
func accelCheck(goos, emulator string) check {
	c := check{Name: "hardware acceleration"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, emulator, "-accel-check").CombinedOutput()
	detail := accelDetail(string(out))
	if err == nil {
		c.Status, c.Detail = statusOK, detail
		return c
	}
	if detail == "" {
		detail = err.Error()
	}
	c.Status, c.Detail = statusFail, detail
	c.Fix = accelFix(goos)
	return c
}

// accelDetail is the line of `emulator -accel-check` output that says what it found, without
// the "accel:" framing and status code lines around it.
func accelDetail(out string) string {
	best := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l == "accel:" || l == "accel" || strings.Trim(l, "0123456789") == "" ||
			strings.HasPrefix(l, "INFO") || strings.HasPrefix(l, "WARNING") {
			continue
		}
		best = l
	}
	return best
}

func accelFix(goos string) []string {
	if goos == "windows" {
		return []string{
			"Turn on virtualization (VT-x / AMD-V) in the firmware settings if it is off",
			"Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform    # PowerShell as Administrator, then restart",
		}
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	return []string{
		"Turn on virtualization (VT-x / AMD-V) in the firmware settings if it is off",
		"sudo apt install qemu-kvm && sudo usermod -aG kvm " + user + "    # then log out and back in, so /dev/kvm is yours to open",
	}
}
