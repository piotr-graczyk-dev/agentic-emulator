//go:build !windows

package ios

import (
	"testing"
	"time"
)

// type --keys sleeps AGEMU_IOS_KEY_GAP_MS after every key; the helper call's timeout must cover it.
func TestKeyBudgetCoversTheKeyGap(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", 50 * time.Millisecond},
		{"100", 140 * time.Millisecond},
		{"0", 40 * time.Millisecond},
		{"-5", 50 * time.Millisecond}, // the helper ignores what is not a UInt32 and uses 10
	} {
		t.Setenv("AGEMU_IOS_KEY_GAP_MS", c.env)
		if got := keyBudget(); got != c.want {
			t.Fatalf("AGEMU_IOS_KEY_GAP_MS=%q: budget %v, want %v", c.env, got, c.want)
		}
	}
}
