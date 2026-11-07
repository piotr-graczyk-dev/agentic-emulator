package android

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The on-device agent's rules for skipping the windows of dead or crashing apps
// (android-agent/src/agemu/Owners.java) are plain Java; their tests
// (android-agent/test/agemu/OwnersTest.java) run here on the host JVM. Skipped without a JDK.
func TestAgentOwnerRules(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("no javac")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("no java")
	}
	root := filepath.Join("..", "..", "android-agent")
	out := t.TempDir()
	build := exec.Command(javac, "-nowarn", "-encoding", "UTF-8", "-d", out,
		filepath.Join(root, "src", "agemu", "Owners.java"),
		filepath.Join(root, "test", "agemu", "OwnersTest.java"))
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, b)
	}
	if b, err := exec.Command(java, "-cp", out, "agemu.OwnersTest").CombinedOutput(); err != nil {
		t.Fatalf("OwnersTest: %v\n%s", err, b)
	}
}
