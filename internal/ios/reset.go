package ios

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
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

// simctlResetTimeout bounds each simctl call of a reset (privacy, keychain, pasteboard).
const simctlResetTimeout = 60 * time.Second

func (b *Backend) reset(ctx context.Context, id string) (ResetReport, error) {
	start := time.Now()
	var rep ResetReport
	d, err := b.target(ctx, id)
	if err != nil {
		return rep, err
	}
	if d.StateCode != stateBooted {
		if d, err = b.bootReady(ctx, d.UDID); err != nil {
			return rep, err
		}
	}
	udid := d.UDID

	// simctl calls are slow to start (~100 ms each, seconds on a loaded Mac); run them
	// alongside the app cleanup.
	sctx, cancel := context.WithTimeout(ctx, simctlResetTimeout)
	defer cancel()
	var wg sync.WaitGroup
	var simctlErr error
	var errMu sync.Mutex
	fail := func(err error) {
		errMu.Lock()
		if simctlErr == nil {
			simctlErr = err
		}
		errMu.Unlock()
	}
	for _, args := range [][]string{
		{"privacy", udid, "reset", "all"},
		{"keychain", udid, "reset"},
	} {
		wg.Add(1)
		go func(args []string) {
			defer wg.Done()
			if _, err := simctl(sctx, args...); err != nil {
				fail(err)
			}
		}(args)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := simctlStdin(sctx, "", "pbcopy", udid); err != nil {
			fail(err)
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
	tList := time.Since(start)

	// Terminating an app that is not running fails in about a millisecond, so terminating
	// everything is cheaper than asking launchctl what runs. The calls are independent:
	// run 8 at a time.
	all := append(append([]string{}, system...), user...)
	var terminated int
	var tMu sync.Mutex
	sem := make(chan struct{}, 8)
	var twg sync.WaitGroup
	for _, bundle := range all {
		twg.Add(1)
		sem <- struct{}{}
		go func(bundle string) {
			defer func() { <-sem; twg.Done() }()
			if err := b.call(ctx, "terminate", map[string]any{"udid": udid, "bundleId": bundle}, nil); err == nil {
				tMu.Lock()
				terminated++
				tMu.Unlock()
			}
		}(bundle)
	}
	twg.Wait()
	rep.Terminated = terminated
	tTerminate := time.Since(start)
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
	tUninstall := time.Since(start)

	wg.Wait()
	tSimctl := time.Since(start)
	if simctlErr != nil {
		return rep, fmt.Errorf("reset: %w", simctlErr)
	}
	if err := b.goHome(ctx, udid, strings.Contains(d.DeviceType, "iPad")); err != nil {
		return rep, fmt.Errorf("reset: %w", err)
	}
	rep.Ms = ms(time.Since(start))
	log.Printf("ios reset %s: %.0f ms (app lists %.0f, terminate %d apps %.0f, uninstall %v %.0f, simctl privacy/keychain/pbcopy done at %.0f, home %.0f)",
		udid, rep.Ms, ms(tList), len(all), ms(tTerminate-tList), rep.Uninstalled, ms(tUninstall-tTerminate), ms(tSimctl), ms(time.Since(start)-tSimctl))
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

const springboardBundle = "com.apple.springboard"

// goHome presses home and waits (up to 15 s) until SpringBoard is the frontmost application,
// recognised by the bundle id accessibility reports. On iPad (iOS 26.5) accessibility reports
// the dock's DockFolderViewService as the frontmost application, on the home screen and in front
// of apps alike, so SpringBoard cannot be confirmed there: after pressing home, a frontmost
// "...ViewService" is accepted once the screen has been still for 300 ms.
func (b *Backend) goHome(ctx context.Context, udid string, ipad bool) error {
	if err := b.call(ctx, "key", map[string]any{"udid": udid, "key": "home"}, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	last := "nothing yet"
	for {
		var front struct {
			Pid      int    `json:"pid"`
			BundleID string `json:"bundleId"`
		}
		err := b.call(ctx, "frontmost", map[string]any{"udid": udid, "timeoutMs": 3000}, &front)
		switch {
		case err != nil:
			last = err.Error()
		case ipad && strings.HasSuffix(front.BundleID, "ViewService"):
			_ = b.call(ctx, "settle", map[string]any{"udid": udid, "quietMs": 300, "maxMs": 2000}, nil)
			log.Printf("ios reset %s: iPad accessibility reports %s frontmost; pressed home without confirming SpringBoard", udid, front.BundleID)
			return nil
		case front.BundleID == springboardBundle:
			// SpringBoard reports itself frontmost before it has finished handling the
			// press; an app launched in that window starts in the background. Let it settle.
			time.Sleep(300 * time.Millisecond)
			return nil
		default:
			last = fmt.Sprintf("frontmost is %q (pid %d)", front.BundleID, front.Pid)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SpringBoard did not come to the front within 15 s after pressing home (%s)", last)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
