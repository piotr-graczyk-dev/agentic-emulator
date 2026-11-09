package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeJava puts a java on PATH that reports version v.
func fakeJava(t *testing.T, v string) string {
	t.Helper()
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
		if javaHomeTool("-v", "17+") != "" || isFile(filepath.Join(AndroidStudioJBR, "bin", "java")) {
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
