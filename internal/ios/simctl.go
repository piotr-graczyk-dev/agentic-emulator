package ios

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// simctl runs `xcrun simctl <args>` for operations where speed does not matter
// (create, privacy/keychain reset, pasteboard).
func simctl(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("simctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func simctlStdin(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("simctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// newestRuntime picks the newest available iOS runtime ($AGEMU_IOS_RUNTIME overrides).
func newestRuntime(ctx context.Context) (string, error) {
	if r := os.Getenv("AGEMU_IOS_RUNTIME"); r != "" {
		return r, nil
	}
	out, err := simctl(ctx, "list", "runtimes", "-j")
	if err != nil {
		return "", err
	}
	var v struct {
		Runtimes []struct {
			Identifier  string `json:"identifier"`
			Version     string `json:"version"`
			Platform    string `json:"platform"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", err
	}
	best, bestVersion := "", ""
	for _, r := range v.Runtimes {
		if !r.IsAvailable || (r.Platform != "iOS" && !strings.Contains(r.Identifier, ".iOS-")) {
			continue
		}
		if best == "" || versionLess(bestVersion, r.Version) {
			best, bestVersion = r.Identifier, r.Version
		}
	}
	if best == "" {
		return "", fmt.Errorf("no available iOS simulator runtime (install one in Xcode > Settings > Components)")
	}
	return best, nil
}

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			fmt.Sscanf(as[i], "%d", &x)
		}
		if i < len(bs) {
			fmt.Sscanf(bs[i], "%d", &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}
