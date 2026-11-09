// Package toolchain finds the host tools agemu depends on: the Android SDK, the system image
// agemu creates devices from, and a Java runtime for the SDK's avdmanager. The daemon uses it to
// run them and `agemu doctor` to check them, so both agree on where they are. It uses only the
// standard library, so the thin agemu CLI can import it.
package toolchain

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultAndroidImage is the system image agemu creates Android devices from.
const DefaultAndroidImage = "system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a"

// HomebrewAndroidSDK is where `brew install --cask android-commandlinetools` puts the SDK.
const HomebrewAndroidSDK = "/opt/homebrew/share/android-commandlinetools"

// AndroidStudioJBR is the Java runtime bundled with Android Studio.
const AndroidStudioJBR = "/Applications/Android Studio.app/Contents/jbr/Contents/Home"

// AndroidSDK is the Android SDK root: $ANDROID_HOME, $ANDROID_SDK_ROOT, Android Studio's
// ~/Library/Android/sdk, or Homebrew's android-commandlinetools when only that exists.
func AndroidSDK() string {
	sdk, _ := AndroidSDKSource()
	return sdk
}

// AndroidSDKSource is AndroidSDK and where it came from (an environment variable, "default" or
// "homebrew"), for doctor's report.
func AndroidSDKSource() (string, string) {
	for _, k := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(k); v != "" {
			return v, "$" + k
		}
	}
	home, _ := os.UserHomeDir()
	studio := filepath.Join(home, "Library", "Android", "sdk")
	if !isDir(studio) && isDir(HomebrewAndroidSDK) {
		return HomebrewAndroidSDK, "homebrew"
	}
	return studio, "default"
}

// AndroidTool is a path inside the SDK.
func AndroidTool(parts ...string) string {
	return filepath.Join(append([]string{AndroidSDK()}, parts...)...)
}

// AndroidImage is the system image agemu creates devices from ($AGEMU_ANDROID_IMAGE overrides).
func AndroidImage() string {
	if v := os.Getenv("AGEMU_ANDROID_IMAGE"); v != "" {
		return v
	}
	return DefaultAndroidImage
}

// AndroidImageDir is the directory the image is installed in.
func AndroidImageDir(image string) string {
	return AndroidTool(strings.Split(image, ";")...)
}

// MinJava is the oldest Java the SDK's avdmanager and sdkmanager run on.
const MinJava = 17

// Java finds the Java runtime the SDK's avdmanager and sdkmanager run with, the way their scripts
// pick it: $JAVA_HOME's, else the java on PATH. When JAVA_HOME is unset and PATH has no Java 17+,
// it falls back to a JDK 17+ macOS's java_home knows, then Android Studio's bundled runtime; home
// is then the JAVA_HOME JavaEnv sets so the tools use it. bin is "" when there is no Java at all.
func Java() (bin, home string) {
	if v := os.Getenv("JAVA_HOME"); v != "" {
		return filepath.Join(v, "bin", "java"), v
	}
	pathJava := ""
	// /usr/bin/java is macOS's stub, which runs java_home's default: the fallbacks cover it.
	if p, err := exec.LookPath("java"); err == nil && p != "/usr/bin/java" {
		if m, err := JavaMajor(p); err == nil && m >= MinJava {
			return p, ""
		}
		pathJava = p
	}
	if jh := javaHomeTool("-v", fmt.Sprintf("%d+", MinJava)); jh != "" {
		return filepath.Join(jh, "bin", "java"), jh
	}
	if isFile(filepath.Join(AndroidStudioJBR, "bin", "java")) {
		return filepath.Join(AndroidStudioJBR, "bin", "java"), AndroidStudioJBR
	}
	if pathJava != "" { // too old or broken; `agemu doctor` says which
		return pathJava, ""
	}
	if jh := javaHomeTool(); jh != "" {
		return filepath.Join(jh, "bin", "java"), jh
	}
	return "", ""
}

// javaHomeTool runs macOS's /usr/libexec/java_home with args; "" when it finds no JDK.
func javaHomeTool(args ...string) string {
	out, err := exec.Command("/usr/libexec/java_home", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// JavaEnv is the environment for running an SDK Java tool: the current one, with JAVA_HOME set
// when Java picked a runtime the tool would not find by itself (so a Mac with only Android Studio
// works, and an old java on PATH does not win over a JDK 17+).
func JavaEnv() []string {
	env := os.Environ()
	if os.Getenv("JAVA_HOME") != "" {
		return env
	}
	if _, home := Java(); home != "" {
		env = append(env, "JAVA_HOME="+home)
	}
	return env
}

// JavaMajor runs a java binary and returns its major version (8 for "1.8.0").
func JavaMajor(bin string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-version").CombinedOutput()
	if err != nil {
		return 0, err
	}
	return ParseJavaVersion(string(out))
}

// ParseJavaVersion reads the major version from `java -version` output.
func ParseJavaVersion(out string) (int, error) {
	m := javaVersionRe.FindStringSubmatch(out)
	if m == nil {
		first, _, _ := strings.Cut(out, "\n")
		return 0, fmt.Errorf("unrecognised java -version output: %s", first)
	}
	major, _ := strconv.Atoi(m[1])
	if major == 1 && m[2] != "" { // 1.8.0_402 is Java 8
		major, _ = strconv.Atoi(m[2])
	}
	return major, nil
}

var javaVersionRe = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

func isDir(p string) bool  { st, err := os.Stat(p); return err == nil && st.IsDir() }
func isFile(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
