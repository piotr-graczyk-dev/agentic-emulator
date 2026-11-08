// Package toolchain finds the host tools agemu depends on: the Android SDK, the system image
// agemu creates devices from, and a Java runtime for the SDK's avdmanager. The daemon uses it to
// run them and `agemu doctor` to check them, so both agree on where they are. It uses only the
// standard library, so the thin agemu CLI can import it.
package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// JavaHome finds a Java runtime for avdmanager and sdkmanager: $JAVA_HOME, the JDK macOS's
// java_home picks, or Android Studio's bundled runtime. "" when there is none.
func JavaHome() string {
	if v := os.Getenv("JAVA_HOME"); v != "" {
		return v
	}
	if out, err := exec.Command("/usr/libexec/java_home").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	if isFile(filepath.Join(AndroidStudioJBR, "bin", "java")) {
		return AndroidStudioJBR
	}
	return ""
}

// JavaEnv is the environment for running an SDK Java tool: the current one, with JAVA_HOME set
// when it is not and a runtime was found (so a Mac with only Android Studio works).
func JavaEnv() []string {
	env := os.Environ()
	if os.Getenv("JAVA_HOME") != "" {
		return env
	}
	if jh := JavaHome(); jh != "" {
		env = append(env, "JAVA_HOME="+jh)
	}
	return env
}

func isDir(p string) bool  { st, err := os.Stat(p); return err == nil && st.IsDir() }
func isFile(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
