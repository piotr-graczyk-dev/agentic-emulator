//go:build !windows

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
//  2. uninstalls every user-installed app, which deletes its data, app-group containers and
//     privacy grants,
//  3. resets the keychain, and puts back what the test capabilities change
//     (resetCapabilities): light appearance, no simulated location, an empty pasteboard, and no
//     privacy grant for any app,
//  4. brings SpringBoard to the front (goHome: home is pressed only when another app is
//     still in front) and waits for a still screen.
//
// Not reset (unlike `simctl erase`): data of built-in apps (Settings search history, Safari,
// Contacts, Photos...), other system settings an agent changed (language...), notification
// permissions answered in the system alert, and media added with `simctl addmedia`. See
// docs/IOS.md.
func (b *Backend) Reset(ctx context.Context, id string) error {
	_, err := b.reset(ctx, id)
	return err
}

// simctlResetTimeout bounds the simctl call of a reset (keychain).
const simctlResetTimeout = 60 * time.Second

func (b *Backend) reset(ctx context.Context, id string) (ResetReport, error) {
	start := time.Now()
	var rep ResetReport
	d, err := b.target(ctx, id)
	if err != nil {
		return rep, err
	}
	defer b.appOp(d.UDID, false)()
	if d.StateCode != stateBooted {
		if d, err = b.bootReady(ctx, d.UDID); err != nil {
			return rep, err
		}
	}
	udid := d.UDID
	// Every app is about to end at agemu's request; the capture is emptied at the end.
	defer b.expectExits(udid, "")(0)
	b.noteInput(udid)

	// simctl is slow to start (~100 ms, seconds on a loaded Mac): reset the keychain alongside
	// the app cleanup. (`simctl privacy reset all` without a bundle id, used before, changes
	// nothing on iOS 26.5; resetCapabilities resets each grant instead.)
	sctx, cancel := context.WithTimeout(ctx, simctlResetTimeout)
	defer cancel()
	var wg sync.WaitGroup
	var simctlErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, simctlErr = simctl(sctx, "keychain", udid, "reset")
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

	// After the uninstalls, so their apps' grants are gone with them.
	capErr := b.resetCapabilities(ctx, udid)
	tCap := time.Since(start)
	wg.Wait()
	tSimctl := time.Since(start)
	if simctlErr != nil {
		return rep, fmt.Errorf("reset: %w", simctlErr)
	}
	if capErr != nil {
		return rep, fmt.Errorf("reset: %w", capErr)
	}
	if err := goHome(ctx, helperHome{b, udid}); err != nil {
		return rep, fmt.Errorf("reset: %w", err)
	}
	// Logs and crash records from before the reset are not returned (the stream keeps running).
	b.mu.Lock()
	dl := b.logs[udid]
	b.mu.Unlock()
	if dl != nil {
		dl.clear()
	}
	rep.Ms = ms(time.Since(start))
	log.Printf("ios reset %s: %.0f ms (app lists %.0f, terminate %d apps %.0f, uninstall %v %.0f, appearance/location/pasteboard/privacy %.0f, simctl keychain done at %.0f, home %.0f)",
		udid, rep.Ms, ms(tList), len(all), ms(tTerminate-tList), rep.Uninstalled, ms(tUninstall-tTerminate), ms(tCap-tUninstall), ms(tSimctl), ms(time.Since(start)-tSimctl))
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

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
