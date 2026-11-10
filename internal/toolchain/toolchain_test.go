package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeJava puts a java on PATH that reports version v.
func fakeJava(t *testing.T, v string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake java is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "java")
	script := "#!/bin/sh\necho 'openjdk version \"" + v + "\" 2025-01-21' >&2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// A usable java on PATH is the one avdmanager runs when JAVA_HOME is unset, so Java keeps it and
// JavaEnv does not override it with java_home's pick.
func TestJavaKeepsAUsableJavaOnPath(t *testing.T) {
	t.Setenv("JAVA_HOME", "")
	bin := fakeJava(t, "21.0.2")
	if got, home := Java(); got != bin || home != "" {
		t.Fatalf("Java() = %q, %q; want %q and no JAVA_HOME", got, home, bin)
	}
	for _, kv := range JavaEnv() {
		if len(kv) > 10 && kv[:10] == "JAVA_HOME=" && kv != "JAVA_HOME=" {
			t.Fatalf("JavaEnv sets %s over the java on PATH", kv)
		}
	}
}

// JAVA_HOME wins over PATH, as in the SDK's scripts.
func TestJavaPrefersJavaHome(t *testing.T) {
	fakeJava(t, "21.0.2")
	t.Setenv("JAVA_HOME", "/opt/jdk")
	if got, home := Java(); got != "/opt/jdk/bin/java" || home != "/opt/jdk" {
		t.Fatalf("Java() = %q, %q", got, home)
	}
}

// An old java on PATH does not win over a JDK 17+ elsewhere (when the Mac has one).
func TestJavaSkipsAnOldJavaOnPath(t *testing.T) {
	t.Setenv("JAVA_HOME", "")
	bin := fakeJava(t, "1.8.0_402")
	got, home := Java()
	if got == bin {
		if javaHomeTool("-v", "17+") != "" || isFile(filepath.Join(AndroidStudioJBR(), "bin", "java")) {
			t.Fatalf("Java() kept Java 8 on PATH over a JDK 17+")
		}
		return // no JDK 17+ on this Mac: the old one is reported
	}
	if home == "" {
		t.Fatalf("Java() = %q without a JAVA_HOME to make the SDK tools use it", got)
	}
}

func TestParseJavaVersion(t *testing.T) {
	for out, want := range map[string]int{
		`openjdk version "17.0.10" 2024-01-16 LTS`: 17,
		`openjdk version "21" 2023-09-19`:          21,
		`java version "1.8.0_402"`:                 8,
	} {
		if got, err := ParseJavaVersion(out); err != nil || got != want {
			t.Errorf("ParseJavaVersion(%q) = %d, %v; want %d", out, got, err, want)
		}
	}
}

func TestDefaultAndroidImageMatchesHostArchitecture(t *testing.T) {
	for goarch, want := range map[string]string{
		"arm64": "system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a",
		"amd64": "system-images;android-37.1;google_apis_playstore_ps16k;x86_64",
	} {
		if got := defaultAndroidImage(goarch); got != want {
			t.Errorf("defaultAndroidImage(%q) = %q, want %q", goarch, got, want)
		}
	}
}

func TestStudioSDKPerOS(t *testing.T) {
	for _, c := range []struct{ goos, home, lad, want string }{
		{"darwin", "/Users/a", "", filepath.Join("/Users/a", "Library", "Android", "sdk")},
		{"linux", "/home/a", "", filepath.Join("/home/a", "Android", "Sdk")},
		{"windows", `C:\Users\a`, `C:\Users\a\AppData\Local`, filepath.Join(`C:\Users\a\AppData\Local`, "Android", "Sdk")},
		{"windows", `C:\Users\a`, "", filepath.Join(`C:\Users\a`, "AppData", "Local", "Android", "Sdk")},
	} {
		if got := studioSDK(c.goos, c.home, c.lad); got != c.want {
			t.Errorf("studioSDK(%s) = %q, want %q", c.goos, got, c.want)
		}
	}
}

func TestToolNamesPerOS(t *testing.T) {
	t.Setenv("ANDROID_HOME", filepath.Join("sdk"))
	emu, adb, avd := Emulator(), ADB(), AVDManager()
	if runtime.GOOS == "windows" {
		if filepath.Ext(emu) != ".exe" || filepath.Ext(adb) != ".exe" || filepath.Ext(avd) != ".bat" {
			t.Fatalf("Windows tool names: %s %s %s", emu, adb, avd)
		}
		return
	}
	if emu != filepath.Join("sdk", "emulator", "emulator") || adb != filepath.Join("sdk", "platform-tools", "adb") ||
		avd != filepath.Join("sdk", "cmdline-tools", "latest", "bin", "avdmanager") {
		t.Fatalf("tool paths: %s %s %s", emu, adb, avd)
	}
}
