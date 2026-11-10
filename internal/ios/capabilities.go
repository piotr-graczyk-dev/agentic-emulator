//go:build !windows

package ios

// Test capabilities (docs/CAPABILITIES.md, docs/IOS.md "Test capabilities"). Each is the call
// `simctl` makes (SimDevice openURL, setPrivacyAccessForService, setLocationWithLatitude,
// setUIInterfaceStyle, sendPushNotification, the SimPasteboardPlus pasteboard), made by the
// long-lived helper in process instead of one simctl process per call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

var (
	_ backend.URLOpener         = (*Backend)(nil)
	_ backend.PermissionSetter  = (*Backend)(nil)
	_ backend.LocationSetter    = (*Backend)(nil)
	_ backend.ClipboardAccessor = (*Backend)(nil)
	_ backend.AppearanceSetter  = (*Backend)(nil)
	_ backend.AppManager        = (*Backend)(nil)
	_ backend.PushSender        = (*Backend)(nil)
)

// OpenURL opens url with SimDevice openURL, what `simctl openurl` does. It returns once the
// system has accepted the URL; the app comes up after that (use --wait-*). For a custom scheme
// the system may first ask "Open in <app>?" (as for a link in Safari): agemu does not answer it.
// A URL that no app handles is an error. The handling app is not reported ("").
func (b *Backend) OpenURL(ctx context.Context, id, url string) (string, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return "", err
	}
	defer b.appOp(d.UDID, true)()
	b.noteInput(d.UDID)
	if err := b.call(ctx, "openurl", map[string]any{"udid": d.UDID, "url": url}, nil); err != nil {
		return "", err
	}
	return "", nil
}

// privacyService maps a neutral permission name to the iOS Simulator's privacy service.
type privacyService struct {
	// service is CoreSimulator's privacy service (simctl privacy maps its names to these).
	service string
	// usage lists the Info.plist keys a grant needs: one key of every group.
	usage [][]string
	// notImplemented, when set, is why the name has no faithful path on iOS (every action).
	notImplemented string
	// grantNotImplemented, when set, is why only a grant has no faithful path.
	grantNotImplemented string
}

const (
	serviceAll            = "kTCCServiceAll"
	serviceLocation       = "__CoreLocationWhenInUse"
	serviceLocationAlways = "__CoreLocationAlways"
)

// privacyServices is the iOS column of docs/CAPABILITIES.md's permission table, as verified
// on iOS 26.5 with the probe app (testapps/ios) reading each status.
var privacyServices = map[api.Permission]privacyService{
	api.PermCamera:     {service: "kTCCServiceCamera", usage: [][]string{{"NSCameraUsageDescription"}}},
	api.PermMicrophone: {service: "kTCCServiceMicrophone", usage: [][]string{{"NSMicrophoneUsageDescription"}}},
	api.PermPhotos: {service: "kTCCServicePhotos", usage: [][]string{{"NSPhotoLibraryUsageDescription"}},
		grantNotImplemented: "the Simulator's privacy grant (simctl privacy grant photos) writes a TCC record that Photos ignores on iOS 26: the app still reads notDetermined. Reset the permission and tap Allow Full Access in the app's system dialog; revoke and reset work"},
	api.PermPhotosAdd: {service: "kTCCServicePhotosAdd", usage: [][]string{{"NSPhotoLibraryAddUsageDescription"}}},
	api.PermLocation:  {service: serviceLocation, usage: [][]string{{"NSLocationWhenInUseUsageDescription"}}},
	api.PermLocationAlways: {service: serviceLocationAlways,
		usage: [][]string{{"NSLocationWhenInUseUsageDescription"}, {"NSLocationAlwaysAndWhenInUseUsageDescription"}}},
	api.PermContacts: {service: "kTCCServiceAddressBook", usage: [][]string{{"NSContactsUsageDescription"}}},
	api.PermContactsLimited: {notImplemented: "the Simulator's contacts-limited service is the old basic-contact-info access: " +
		"the app reads full authorization, not iOS 18's limited access. Use the app's system dialog"},
	api.PermCalendar: {service: "kTCCServiceCalendar",
		usage: [][]string{{"NSCalendarsFullAccessUsageDescription", "NSCalendarsUsageDescription"}}},
	api.PermReminders: {service: "kTCCServiceReminders",
		usage: [][]string{{"NSRemindersFullAccessUsageDescription", "NSRemindersUsageDescription"}}},
	api.PermNotifications: {notImplemented: "iOS has no privacy service for notifications: tap Allow in the app's system alert " +
		"(agemu ui shows it; reset and clear-data do not reset it, see docs/IOS.md)"},
	api.PermMotion:       {service: "kTCCServiceMotion", usage: [][]string{{"NSMotionUsageDescription"}}},
	api.PermMediaLibrary: {service: "kTCCServiceMediaLibrary", usage: [][]string{{"NSAppleMusicUsageDescription"}}},
	api.PermSiri:         {service: "kTCCServiceSiri", usage: [][]string{{"NSSiriUsageDescription"}}},
}

// privacyOp is one CoreSimulator privacy call.
type privacyOp struct {
	Service string
	Action  string // grant, revoke or reset
}

// planPermissions maps a permission request to privacy calls, checking every name first: a
// name without a faithful path answers api.NotImplemented and nothing changes. No names with
// reset means every service of the app (kTCCServiceAll also resets location).
func planPermissions(action api.PermissionAction, perms []api.Permission) ([]privacyOp, error) {
	if len(perms) == 0 {
		if action != api.PermissionReset {
			return nil, fmt.Errorf("permission %s needs at least one permission", action)
		}
		return []privacyOp{{Service: serviceAll, Action: string(api.PermissionReset)}}, nil
	}
	var ops []privacyOp
	for _, p := range perms {
		if p.Raw() {
			return nil, api.NotImplemented(api.IOS, fmt.Sprintf("permission %q", p), "raw platform permission names are Android's; use the neutral names")
		}
		s, ok := privacyServices[p]
		if !ok {
			return nil, fmt.Errorf("unknown permission %q", p)
		}
		if s.notImplemented != "" {
			return nil, api.NotImplemented(api.IOS, fmt.Sprintf("permission %q", p), s.notImplemented)
		}
		if action == api.PermissionGrant && s.grantNotImplemented != "" {
			return nil, api.NotImplemented(api.IOS, fmt.Sprintf("permission grant %q", p), s.grantNotImplemented)
		}
		ops = append(ops, privacyOp{Service: s.service, Action: string(action)})
	}
	return ops, nil
}

// missingUsage returns an error naming the Info.plist usage descriptions app lacks for a grant
// of perms (on a device the app would crash when it asks, so a silent grant would hide that).
func missingUsage(app string, perms []api.Permission, keys []string) error {
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	var missing []string
	for _, p := range perms {
		for _, group := range privacyServices[p].usage {
			found := false
			for _, k := range group {
				found = found || have[k]
			}
			if !found {
				missing = append(missing, fmt.Sprintf("%s (for %s)", strings.Join(group, " or "), p))
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to grant: %s has no %s in its Info.plist; on a device the app crashes when it asks for it", app, strings.Join(missing, ", "))
}

// appDetails is the helper's "appinfo" answer.
type appDetails struct {
	ID              string            `json:"id"`
	Installed       bool              `json:"installed"`
	Type            string            `json:"type"` // "User" or "System"
	Path            string            `json:"path"`
	DataContainer   string            `json:"dataContainer"`
	GroupContainers map[string]string `json:"groupContainers"`
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	UsageKeys       []string          `json:"usageKeys"`
}

func (b *Backend) appInfo(ctx context.Context, udid, app string) (appDetails, error) {
	var a appDetails
	if err := b.call(ctx, "appinfo", map[string]any{"udid": udid, "bundleId": app}, &a); err != nil {
		return a, err
	}
	if !a.Installed {
		return a, fmt.Errorf("%s is not installed", app)
	}
	return a, nil
}

// SetPermissions grants, revokes or resets privacy services with CoreSimulator's
// setPrivacyAccessForService / resetPrivacyAccessForService (what `simctl privacy` does). The
// system may terminate the app, as Settings does on a device.
func (b *Backend) SetPermissions(ctx context.Context, id, app string, action api.PermissionAction, perms []api.Permission) error {
	ops, err := planPermissions(action, perms)
	if err != nil {
		return err
	}
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	defer b.appOp(d.UDID, false)()
	info, err := b.appInfo(ctx, d.UDID, app)
	if err != nil {
		return err
	}
	if action == api.PermissionGrant {
		if err := missingUsage(app, perms, info.UsageKeys); err != nil {
			return err
		}
	}
	// iOS may end the app when a permission changes: that exit is agemu's, not a crash.
	defer b.expectExits(d.UDID, app)(0)
	for _, op := range ops {
		req := map[string]any{"udid": d.UDID, "service": op.Service, "bundleId": app, "action": op.Action}
		if err := b.call(ctx, "privacy", req, nil); err != nil {
			return err
		}
	}
	return nil
}

// SetLocation sets the simulated location (SimDevice setLocationWithLatitude, as `simctl
// location set`): CoreLocation in every app reports this fix.
func (b *Backend) SetLocation(ctx context.Context, id string, lat, lon float64) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	defer b.appOp(d.UDID, false)()
	return b.call(ctx, "location", map[string]any{"udid": d.UDID, "latitude": lat, "longitude": lon}, nil)
}

// ClearLocation removes the simulated location (the baseline: no fix).
func (b *Backend) ClearLocation(ctx context.Context, id string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	defer b.appOp(d.UDID, false)()
	return b.call(ctx, "location", map[string]any{"udid": d.UDID, "clear": true}, nil)
}

// Clipboard reads the device pasteboard's text through the helper's pasteboard connection.
func (b *Backend) Clipboard(ctx context.Context, id string) (string, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return "", err
	}
	var r struct {
		Text string `json:"text"`
	}
	err = b.call(ctx, "pasteboard", map[string]any{"udid": d.UDID, "op": "get"}, &r)
	return r.Text, err
}

// SetClipboard replaces the device pasteboard with text ("" empties it).
func (b *Backend) SetClipboard(ctx context.Context, id, text string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	return b.call(ctx, "pasteboard", map[string]any{"udid": d.UDID, "op": "set", "text": text}, nil)
}

// appearanceSettleMs bounds the wait for the screen to show an appearance change: iOS
// crossfades the whole screen for ~500 ms after it, starting 50-650 ms after the call.
const appearanceSettleMs = 1500

// Appearance reads the system appearance (SimDevice currentUIInterfaceStyle).
func (b *Backend) Appearance(ctx context.Context, id string) (api.Appearance, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return "", err
	}
	var r struct {
		Appearance string `json:"appearance"`
	}
	if err := b.call(ctx, "appearance", map[string]any{"udid": d.UDID}, &r); err != nil {
		return "", err
	}
	return parseAppearance(r.Appearance)
}

func parseAppearance(s string) (api.Appearance, error) {
	switch api.Appearance(s) {
	case api.AppearanceLight, api.AppearanceDark:
		return api.Appearance(s), nil
	}
	return "", fmt.Errorf("the simulator reports an unknown appearance (%q)", s)
}

// SetAppearance switches light/dark mode (SimDevice setUIInterfaceStyle, as `simctl ui
// appearance`) and returns once the screen shows it: iOS crossfades for ~0.5 s, and agemu waits
// up to 1 s for the crossfade to begin and then for the screen to be still for 120 ms (at most
// 1.5 s in all). Apps get the new trait at once, so the UI tree has it before the call returns;
// the wait is for screenshots. When no change shows (an app that keeps its own appearance), the
// call returns after the 1 s and logs it.
func (b *Backend) SetAppearance(ctx context.Context, id string, a api.Appearance) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	var r struct {
		Changed  bool    `json:"changed"`
		Redrawn  bool    `json:"redrawn"`
		SettleMs float64 `json:"settleMs"`
	}
	if err := b.call(ctx, "appearance", map[string]any{"udid": d.UDID, "style": string(a), "settleMaxMs": appearanceSettleMs}, &r); err != nil {
		return err
	}
	if r.Changed && !r.Redrawn {
		log.Printf("ios appearance %s on %s: no screen change seen in %.0f ms (the app in front may keep its own appearance); a screenshot may still show the old one", a, d.UDID, r.SettleMs)
	}
	return nil
}

// Apps lists the user-installed apps, read from the device's data directory.
func (b *Backend) Apps(ctx context.Context, id string) ([]api.App, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return nil, err
	}
	var apps []api.App
	if err := b.call(ctx, "userappinfo", map[string]any{"udid": d.UDID}, &apps); err != nil {
		return nil, err
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps, nil
}

// userApp returns a user app's details, refusing a built-in app.
func (b *Backend) userApp(ctx context.Context, udid, app, what string) (appDetails, error) {
	info, err := b.appInfo(ctx, udid, app)
	if err != nil {
		return info, err
	}
	if info.Type != "User" {
		return info, fmt.Errorf("%s is a built-in (%s) app: %s works on user-installed apps", app, strings.ToLower(info.Type), what)
	}
	return info, nil
}

// Uninstall removes a user app with its data (SimDevice uninstallApplication, as `simctl
// uninstall`).
func (b *Backend) Uninstall(ctx context.Context, id, app string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	defer b.appOp(d.UDID, false)()
	if _, err := b.userApp(ctx, d.UDID, app, "uninstall"); err != nil {
		return err
	}
	defer b.expectExits(d.UDID, app)(0)
	if err := b.call(ctx, "uninstall", map[string]any{"udid": d.UDID, "bundleId": app}, nil); err != nil {
		return err
	}
	b.forgetApp(d.UDID, app)
	return nil
}

func (b *Backend) forgetApp(udid, app string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for pid, a := range b.apps[udid] {
		if a == app {
			delete(b.apps[udid], pid)
		}
	}
}

// ClearAppData gives a user app the state of a fresh install and leaves it installed. iOS has
// no such action; on a device the equivalent is delete and reinstall, so that is what it does,
// with a copy of the installed bundle:
//  1. clones the installed .app (an APFS clone: no data copied, milliseconds even for a large
//     app),
//  2. uninstalls the app (CoreSimulator, as `simctl uninstall`): its data container, app-group
//     containers no other app uses, preferences, and privacy and notification permissions go,
//     as on a device,
//  3. installs the copy again and deletes it.
//
// The keychain is kept, as a device keeps it across delete and reinstall. Checked with the probe
// app: defaults, Documents, Caches, app-group file and defaults gone; keychain item kept;
// camera and notification permissions "not determined". Wiping the containers in place (with
// `defaults delete` for cfprefsd and a privacy reset) took as long (~290 ms) but kept the
// notification permission. If the reinstall fails, the copy is kept and the error says where.
func (b *Backend) ClearAppData(ctx context.Context, id, app string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	defer b.appOp(d.UDID, true)() // it reinstalls the app: app work, like install (idle.go cut-off)
	start := time.Now()
	info, err := b.userApp(ctx, d.UDID, app, "clear-data")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "agemu-clear-data-")
	if err != nil {
		return err
	}
	bundle := filepath.Join(tmp, filepath.Base(info.Path))
	defer b.expectExits(d.UDID, app)(0)
	var tClone, tUninstall time.Duration
	err = reinstallFresh(
		func() error {
			err := b.call(ctx, "clone", map[string]any{"src": info.Path, "dst": bundle}, nil)
			tClone = time.Since(start)
			return err
		},
		func() error {
			err := b.call(ctx, "uninstall", map[string]any{"udid": d.UDID, "bundleId": app}, nil)
			b.forgetApp(d.UDID, app)
			tUninstall = time.Since(start)
			return err
		},
		func() error { return b.call(ctx, "install", map[string]any{"udid": d.UDID, "path": bundle}, nil) },
		func() error { return os.RemoveAll(tmp) },
		bundle,
	)
	if err != nil {
		return fmt.Errorf("clear-data %s: %w", app, err)
	}
	log.Printf("ios clear-data %s on %s: %.0f ms (info+clone %.0f, uninstall %.0f, install %.0f)",
		app, d.UDID, ms(time.Since(start)), ms(tClone), ms(tUninstall-tClone), ms(time.Since(start)-tUninstall))
	return nil
}

// reinstallFresh runs clear-data's steps: copy the bundle, uninstall, install the copy (a
// second try after a failure), and remove the copy. A failed copy changes nothing; after a
// failed reinstall the copy stays at bundle for the caller to install.
func reinstallFresh(clone, uninstall, install, cleanup func() error, bundle string) error {
	if err := clone(); err != nil {
		_ = cleanup()
		return fmt.Errorf("copying the app bundle: %w", err)
	}
	if err := uninstall(); err != nil {
		_ = cleanup()
		return err
	}
	if err := install(); err != nil {
		if err2 := install(); err2 != nil {
			return fmt.Errorf("the app was uninstalled, but installing it again failed (%v); its bundle is kept at %s", err2, bundle)
		}
	}
	return cleanup()
}

// Push delivers a push notification with SimDevice sendPushNotification (what `simctl push`
// does): the app receives it as from APNs. An alert shows only with notification permission;
// in the foreground the app's notification delegate decides.
func (b *Backend) Push(ctx context.Context, id, app string, payload []byte) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	defer b.appOp(d.UDID, true)()
	if _, err := b.appInfo(ctx, d.UDID, app); err != nil {
		return err
	}
	if !json.Valid(payload) {
		return fmt.Errorf("push payload is not JSON")
	}
	return b.call(ctx, "push", map[string]any{"udid": d.UDID, "bundleId": app, "payload": string(payload)}, nil)
}

// privacyGrants is the helper's "privacygrants" answer: the device's TCC rows and the
// location clients with an authorization.
type privacyGrants struct {
	TCC []struct {
		Service string `json:"service"`
		Client  string `json:"client"`
		// ClientType is TCC's client_type: "0" (or empty) a bundle identifier, "1" an
		// executable path, which CoreSimulator's per-bundle reset cannot address.
		ClientType string `json:"clientType"`
	} `json:"tcc"`
	Location []string `json:"location"`
}

// grantedServices is every CoreSimulator service a permission name maps to (the ones agemu can
// change, and reset puts back).
func grantedServices() map[string]bool {
	m := map[string]bool{}
	for _, s := range privacyServices {
		if s.service != "" && !strings.HasPrefix(s.service, "__CoreLocation") {
			m[s.service] = true
		}
	}
	// contacts-limited has no neutral mapping, but an app's dialog can set it.
	m["kTCCServiceContactsLimited"] = true
	return m
}

// resetOps lists the privacy resets that return the device to "not determined" for every app:
// one per TCC row of a service agemu's permission names cover, and one location reset per app
// with a location authorization. Other rows (the baseline's own system grants, e.g. CloudKit)
// are left alone, and so are rows whose client is an executable path (no app agemu grants to;
// they cannot be reset by bundle id).
func (g privacyGrants) resetOps() []privacyReset {
	services := grantedServices()
	var out []privacyReset
	seen := map[privacyReset]bool{}
	add := func(r privacyReset) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, row := range g.TCC {
		bundle := row.ClientType == "" || row.ClientType == "0"
		if services[row.Service] && row.Client != "" && bundle && !strings.HasPrefix(row.Client, "/") {
			add(privacyReset{Service: row.Service, App: row.Client})
		}
	}
	for _, app := range g.Location {
		add(privacyReset{Service: serviceLocation, App: app})
	}
	return out
}

type privacyReset struct{ Service, App string }

// resetPrivacy resets every permission an app holds on the device (see resetOps). `simctl
// privacy reset all` without a bundle id leaves grants in place on iOS 26.5.
func (b *Backend) resetPrivacy(ctx context.Context, udid string) (int, error) {
	var g privacyGrants
	if err := b.call(ctx, "privacygrants", map[string]any{"udid": udid}, &g); err != nil {
		return 0, err
	}
	ops := g.resetOps()
	errs := make([]error, len(ops))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, op := range ops {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, op privacyReset) {
			defer func() { <-sem; wg.Done() }()
			errs[i] = b.call(ctx, "privacy", map[string]any{"udid": udid, "service": op.Service, "bundleId": op.App, "action": "reset"}, nil)
		}(i, op)
	}
	wg.Wait()
	return len(ops), errors.Join(errs...)
}

// resetCapabilities puts back what the capabilities change: light appearance, no simulated
// location, an empty pasteboard and no privacy grants (user apps' grants go with their
// uninstall; this covers the rest).
func (b *Backend) resetCapabilities(ctx context.Context, udid string) error {
	start := time.Now()
	steps := []struct {
		name string
		run  func() error
	}{
		{"appearance", func() error {
			return b.call(ctx, "appearance", map[string]any{"udid": udid, "style": string(api.AppearanceLight)}, nil)
		}},
		{"location", func() error { return b.call(ctx, "location", map[string]any{"udid": udid, "clear": true}, nil) }},
		{"pasteboard", func() error {
			return b.call(ctx, "pasteboard", map[string]any{"udid": udid, "op": "set", "text": ""}, nil)
		}},
		{"privacy", func() error {
			n, err := b.resetPrivacy(ctx, udid)
			if n > 0 {
				log.Printf("ios reset %s: reset %d privacy grants", udid, n)
			}
			return err
		}},
	}
	errs := make([]error, len(steps))
	var wg sync.WaitGroup
	for i := range steps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := steps[i].run(); err != nil {
				errs[i] = fmt.Errorf("%s: %w", steps[i].name, err)
			}
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	log.Printf("ios reset %s: appearance, location, pasteboard, privacy in %.0f ms", udid, ms(time.Since(start)))
	return nil
}
