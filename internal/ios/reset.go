package ios

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// installedApp is the helper's "apps" entry.
type installedApp struct {
	Type string `json:"type"` // "System" or "User"
	Name string `json:"name"`
	Path string `json:"path"`
}

// ResetReport says what Reset did (logged by the daemon, used by tests and docs).
type ResetReport struct {
	Terminated  int      `json:"terminated"`
	Uninstalled []string `json:"uninstalled"`
	Ms          float64  `json:"ms"`
}

// Reset returns a booted simulator to a clean state WITHOUT rebooting it ("soft reset").
// `simctl erase` needs a shutdown, an erase and a full first boot (tens of seconds); this
// takes about a second. It:
//
//  1. terminates every app (system and user),
//  2. uninstalls every user-installed app, which deletes its data and app-group containers,
//  3. resets privacy permissions (TCC) and the keychain, and clears the pasteboard,
//  4. presses home and waits until SpringBoard is frontmost.
//
// Not reset (unlike `simctl erase`): data of built-in apps (Settings search history, Safari,
// Contacts, Photos...), system settings an agent changed (language, appearance...), and media
// added with `simctl addmedia`. See docs/IOS.md.
func (b *Backend) Reset(ctx context.Context, id string) error {
	_, err := b.reset(ctx, id)
	return err
}

func (b *Backend) reset(ctx context.Context, id string) (ResetReport, error) {
	start := time.Now()
	var rep ResetReport
	d, err := b.target(ctx, id)
	if err != nil {
		return rep, err
	}
	if d.StateCode != stateBooted {
		if err := b.bootReady(ctx, d.UDID); err != nil {
			return rep, err
		}
	}
	udid := d.UDID

	// simctl calls are slow to start (~100 ms each); run them alongside the app cleanup.
	var wg sync.WaitGroup
	var simctlErr error
	var errMu sync.Mutex
	for _, args := range [][]string{
		{"privacy", udid, "reset", "all"},
		{"keychain", udid, "reset"},
	} {
		wg.Add(1)
		go func(args []string) {
			defer wg.Done()
			if _, err := simctl(ctx, args...); err != nil {
				errMu.Lock()
				simctlErr = err
				errMu.Unlock()
			}
		}(args)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := simctlStdin(ctx, "", "pbcopy", udid); err != nil {
			errMu.Lock()
			simctlErr = err
			errMu.Unlock()
		}
	}()

	system, err := b.systemApps(ctx, d)
	if err != nil {
		wg.Wait()
		return rep, err
	}
	var user []string
	if err := b.call(ctx, "userapps", map[string]any{"udid": udid}, &user); err != nil {
		wg.Wait()
		return rep, err
	}
	// Terminating an app that is not running fails in about a millisecond, so terminating
	// everything is cheaper than asking launchctl what runs (~0.5 s).
	for _, bundle := range append(append([]string{}, system...), user...) {
		if err := b.call(ctx, "terminate", map[string]any{"udid": udid, "bundleId": bundle}, nil); err == nil {
			rep.Terminated++
		}
	}
	for _, bundle := range user {
		if err := b.call(ctx, "uninstall", map[string]any{"udid": udid, "bundleId": bundle}, nil); err != nil {
			wg.Wait()
			return rep, fmt.Errorf("reset: %w", err)
		}
		rep.Uninstalled = append(rep.Uninstalled, bundle)
	}
	b.mu.Lock()
	delete(b.apps, udid)
	b.mu.Unlock()

	tCleanup := time.Since(start)
	wg.Wait()
	if d := time.Since(start) - tCleanup; d > 100*time.Millisecond {
		log.Printf("ios reset %s: waited %.0f ms for simctl privacy/keychain/pbcopy", udid, ms(d))
	}
	if simctlErr != nil {
		return rep, fmt.Errorf("reset: %w", simctlErr)
	}
	tApps := time.Since(start)
	if err := b.goHome(ctx, udid); err != nil {
		return rep, fmt.Errorf("reset: %w", err)
	}
	rep.Ms = float64(time.Since(start).Microseconds()) / 1000
	log.Printf("ios reset %s: %.0f ms (apps+simctl %.0f ms, home %.0f ms), terminated %d, uninstalled %v",
		udid, rep.Ms, ms(tApps), ms(time.Since(start)-tApps), rep.Terminated, rep.Uninstalled)
	return rep, nil
}

// systemApps lists the runtime's built-in apps. They are fixed per runtime, so the slow
// CoreSimulator query (~1-2 s) runs once per runtime.
func (b *Backend) systemApps(ctx context.Context, d simDevice) ([]string, error) {
	b.mu.Lock()
	cached, ok := b.systemAppCache[d.Runtime]
	b.mu.Unlock()
	if ok {
		return cached, nil
	}
	var apps map[string]installedApp
	if err := b.call(ctx, "apps", map[string]any{"udid": d.UDID}, &apps); err != nil {
		return nil, err
	}
	var out []string
	for bundle, a := range apps {
		if a.Type != "User" {
			out = append(out, bundle)
		}
	}
	sort.Strings(out)
	b.mu.Lock()
	b.systemAppCache[d.Runtime] = out
	b.mu.Unlock()
	return out, nil
}

// goHome presses home and waits until SpringBoard is the frontmost application.
func (b *Backend) goHome(ctx context.Context, udid string) error {
	sb, err := b.springboardPid(ctx, udid, false)
	if err != nil {
		return err
	}
	if err := b.call(ctx, "key", map[string]any{"udid": udid, "key": "home"}, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	refreshed := false
	for {
		var front struct {
			Pid int `json:"pid"`
		}
		if err := b.call(ctx, "frontmost", map[string]any{"udid": udid}, &front); err == nil && front.Pid == sb {
			// SpringBoard reports itself frontmost before it has finished handling the
			// press; an app launched in that window starts in the background. Let it settle.
			time.Sleep(300 * time.Millisecond)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SpringBoard (pid %d) did not come to the front", sb)
		}
		if !refreshed && time.Now().After(deadline.Add(-8*time.Second)) {
			// SpringBoard may have restarted since we cached its pid.
			if p, err := b.springboardPid(ctx, udid, true); err == nil {
				sb = p
			}
			refreshed = true
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (b *Backend) springboardPid(ctx context.Context, udid string, refresh bool) (int, error) {
	b.mu.Lock()
	pid, ok := b.springboard[udid]
	b.mu.Unlock()
	if ok && !refresh {
		return pid, nil
	}
	var running []struct {
		BundleID string `json:"bundleId"`
		Pid      int    `json:"pid"`
	}
	if err := b.call(ctx, "running", map[string]any{"udid": udid}, &running); err != nil {
		return 0, err
	}
	for _, r := range running {
		if r.BundleID == "com.apple.springboard" {
			b.mu.Lock()
			b.springboard[udid] = r.Pid
			b.mu.Unlock()
			return r.Pid, nil
		}
	}
	return 0, fmt.Errorf("SpringBoard is not running on %s", udid)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
