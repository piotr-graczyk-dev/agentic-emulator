package ios

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// TestIntegrationParkDuringShutdown parks a simulator while it is being shut down (and the other
// way round), at offsets of a few milliseconds, as the daemon's auto-park can when a boot shuts
// down the least recently used simulator to make room. Every round must end with the simulator
// shut down, not marked parked, its launchd_sim gone, and the shutdown back in seconds.
//
// It boots and shuts down a real simulator, so it runs only when asked:
//
//	AGEMU_IOS_RACE_UDID=<udid of an agemu-* simulator> AGEMU_IOS_HELPER=$PWD/bin/agemu-ios-helper \
//	  AGEMU_HOME=<state dir> go test -run TestIntegrationParkDuringShutdown -v ./internal/ios
//
// AGEMU_IOS_RACE_ROUNDS sets the number of rounds (default 8).
func TestIntegrationParkDuringShutdown(t *testing.T) {
	udid := os.Getenv("AGEMU_IOS_RACE_UDID")
	if udid == "" {
		t.Skip("set AGEMU_IOS_RACE_UDID to an agemu simulator to run")
	}
	rounds := 8
	if n, err := strconv.Atoi(os.Getenv("AGEMU_IOS_RACE_ROUNDS")); err == nil && n > 0 {
		rounds = n
	}
	b := New()
	defer b.Close()
	id := "ios:" + udid
	// Park starts this much after the shutdown (negative: before it).
	offsets := []time.Duration{-30 * time.Millisecond, 0, 10 * time.Millisecond, 30 * time.Millisecond,
		60 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond, 250 * time.Millisecond}
	for i := 0; i < rounds; i++ {
		off := offsets[i%len(offsets)]
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		if _, err := b.Boot(ctx, id, api.BootOptions{}); err != nil {
			cancel()
			t.Fatalf("round %d: boot: %v", i, err)
		}
		time.Sleep(2 * time.Second)
		parkErr := make(chan error, 1)
		shutErr := make(chan error, 1)
		var shutTook time.Duration
		start := time.Now()
		go func() {
			if off < 0 {
				time.Sleep(-off)
			}
			s := time.Now()
			err := b.Shutdown(ctx, id)
			shutTook = time.Since(s)
			shutErr <- err
		}()
		go func() {
			if off > 0 {
				time.Sleep(off)
			}
			parkErr <- b.Park(ctx, id)
		}()
		pe, se := <-parkErr, <-shutErr
		var d simDevice
		derr := b.call(ctx, "describe", map[string]any{"udid": udid}, &d)
		ps, _ := psTree()
		root, rootStopped, _ := simTree(ps, udid)
		cancel()
		t.Logf("round %d offset %v: park err=%v, shutdown err=%v in %v (total %v); state %s, parked mark %v, launchd_sim %d stopped %v",
			i, off, pe, se, shutTook.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), d.State, b.isParked(udid), root, rootStopped)
		if se != nil || derr != nil || d.StateCode != stateShutdown || b.isParked(udid) || root != 0 || shutTook > 30*time.Second {
			t.Errorf("round %d offset %v: %s", i, off, fmt.Sprintf("shutdown err %v, describe err %v, state %s, parked %v, launchd_sim %d (stopped %v), shutdown took %v",
				se, derr, d.State, b.isParked(udid), root, rootStopped, shutTook))
			if root != 0 && rootStopped {
				_, _, _ = b.unpark(udid) // do not leave a stopped simulator behind
			}
		}
	}
}
