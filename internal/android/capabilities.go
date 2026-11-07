package android

// Test capabilities (docs/CAPABILITIES.md, internal/backend: URLOpener, PermissionSetter,
// LocationSetter, ClipboardAccessor, AppearanceSetter, AppManager). Each one is the system call
// the matching user action or standard tool makes, sent from a long-lived process instead of a
// new one per call:
//
//   - the on-device agent (android-agent/src/agemu/Caps.java, shell uid) for open_url,
//     permissions, clipboard, night mode, apps, uninstall and clear data;
//   - the emulator's gRPC GNSS (SetGps, what `adb emu geo fix` does) for the location.
//
// Push has no faithful path on Android (the daemon answers not implemented, see
// internal/daemon/capabilities.go), so there is no PushSender here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
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
)

// capDev returns a connected device for a capability call.
func (b *Backend) capDev(ctx context.Context, id string) (*device, *emuClient, error) {
	d, err := b.devFor(id)
	if err != nil {
		return nil, nil, err
	}
	e, _, err := d.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	return d, e, nil
}

// capErr turns an agent error ("agent: IllegalArgumentException: x is not installed") into the
// agent's own message where the agent raised it on purpose.
func capErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, p := range []string{"agent: IllegalArgumentException: ", "agent: IllegalStateException: "} {
		if rest, ok := strings.CutPrefix(msg, p); ok {
			return errors.New(rest)
		}
	}
	return err
}

// ------------------------------------------------------------------ open URL

// OpenURL opens a URL as a tapped link does (agent: ACTION_VIEW + CATEGORY_BROWSABLE +
// NEW_TASK, no package, startActivityAndWait) and returns the package of the activity that was
// displayed (WaitResult.who). With several handlers that is the system's chooser (not checked on
// a device yet).
func (b *Backend) OpenURL(ctx context.Context, id, url string) (string, error) {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return "", err
	}
	// The daemon watches the app that took the URL, as after a launch (and, as there, the
	// capture starts once the call has returned).
	defer d.startCapture()
	// Returns once the target has drawn: its frames count from the start.
	d.markInput()
	var res struct {
		App string `json:"app"`
	}
	if err := d.call(ctx, map[string]any{"cmd": "openurl", "url": url}, &res); err != nil {
		return "", capErr(err)
	}
	return res.App, nil
}

// ------------------------------------------------------------------ permissions

const androidPerm = "android.permission."

// permMapping is what a neutral permission name means on Android (docs/CAPABILITIES.md).
type permMapping struct {
	perms   []string // runtime permissions, in grant order
	legacy  []string // used instead of perms for apps targeting API < 33
	extra   []string // revoke and reset act on these too
	dropBG  bool     // a grant also takes background location away
	notImpl string   // non-empty: no faithful mapping, with the reason
}

var permMap = map[api.Permission]permMapping{
	api.PermCamera:     {perms: []string{"CAMERA"}},
	api.PermMicrophone: {perms: []string{"RECORD_AUDIO"}},
	api.PermPhotos: {perms: []string{"READ_MEDIA_IMAGES", "READ_MEDIA_VIDEO"}, legacy: []string{"READ_EXTERNAL_STORAGE"},
		extra: []string{"READ_MEDIA_VISUAL_USER_SELECTED"}},
	api.PermPhotosAdd:       {notImpl: "apps add photos to MediaStore without a permission"},
	api.PermLocation:        {perms: []string{"ACCESS_FINE_LOCATION", "ACCESS_COARSE_LOCATION"}, extra: []string{"ACCESS_BACKGROUND_LOCATION"}, dropBG: true},
	api.PermLocationAlways:  {perms: []string{"ACCESS_FINE_LOCATION", "ACCESS_COARSE_LOCATION", "ACCESS_BACKGROUND_LOCATION"}},
	api.PermContacts:        {perms: []string{"READ_CONTACTS", "WRITE_CONTACTS"}},
	api.PermContactsLimited: {notImpl: "Android has no limited contacts access"},
	api.PermCalendar:        {perms: []string{"READ_CALENDAR", "WRITE_CALENDAR"}},
	api.PermReminders:       {notImpl: "Android has no system reminders store"},
	api.PermNotifications:   {perms: []string{"POST_NOTIFICATIONS"}},
	api.PermMotion:          {perms: []string{"ACTIVITY_RECOGNITION"}},
	api.PermMediaLibrary:    {perms: []string{"READ_MEDIA_AUDIO"}, legacy: []string{"READ_EXTERNAL_STORAGE"}},
	api.PermSiri:            {notImpl: "Android has no Siri"},
}

// permState is the agent's view of an app's requested permissions ("permstate").
type permState struct {
	Pkg       string      `json:"pkg"`
	UID       int         `json:"uid"`
	TargetSdk int         `json:"targetSdk"`
	System    bool        `json:"system"`
	Requested []permEntry `json:"requested"`
}

type permEntry struct {
	Name       string   `json:"name"`
	Granted    bool     `json:"granted"`
	Protection string   `json:"protection"` // dangerous (runtime), normal, signature, internal, unknown
	Flags      []string `json:"flags"`      // runtime permissions: PackageManager FLAG_PERMISSION_* names
	Op         string   `json:"op"`         // app-op permissions (special access): the op
	Mode       string   `json:"mode"`       // and its mode: allowed, ignored, errored, default, foreground
}

func (e permEntry) runtime() bool { return e.Protection == "dangerous" }

func (e permEntry) fixed() string {
	switch {
	case slices.Contains(e.Flags, "SYSTEM_FIXED"):
		return "the system"
	case slices.Contains(e.Flags, "POLICY_FIXED"):
		return "device policy"
	}
	return ""
}

// permStep is one change the agent applies ("perms"): a runtime permission granted or revoked
// with flags set and cleared, or an app-op permission's mode.
type permStep struct {
	Perm  string   `json:"perm"`
	Grant bool     `json:"grant"`
	Set   []string `json:"set,omitempty"`
	Clear []string `json:"clear,omitempty"`
	Op    string   `json:"op,omitempty"`
	Mode  string   `json:"mode,omitempty"`
}

// The target states (api.PermissionAction): the flags the system dialog and Settings leave.
var (
	grantSet    = []string{"USER_SET"}
	grantClear  = []string{"USER_FIXED", "ONE_TIME", "AUTO_REVOKED"}
	revokeSet   = []string{"USER_SET", "USER_FIXED"}
	revokeClear = []string{"ONE_TIME"}
	resetClear  = []string{"USER_SET", "USER_FIXED", "ONE_TIME", "AUTO_REVOKED"}
)

func runtimeStep(perm string, action api.PermissionAction) permStep {
	switch action {
	case api.PermissionGrant:
		return permStep{Perm: perm, Grant: true, Set: grantSet, Clear: grantClear}
	case api.PermissionRevoke:
		return permStep{Perm: perm, Set: revokeSet, Clear: revokeClear}
	}
	return permStep{Perm: perm, Clear: resetClear}
}

func appOpStep(e permEntry, action api.PermissionAction) permStep {
	mode := map[api.PermissionAction]string{api.PermissionGrant: "allowed", api.PermissionRevoke: "errored", api.PermissionReset: "default"}[action]
	return permStep{Perm: e.Name, Op: e.Op, Mode: mode}
}

// planPermissions turns a permission call into the agent's steps. Every name is checked before
// any step is planned, so a refused name changes nothing. A later name's step for the same
// permission replaces an earlier one (grant location, location-always: background is granted;
// grant location-always, location: it is not).
func planPermissions(st permState, action api.PermissionAction, names []api.Permission) ([]permStep, error) {
	req := map[string]permEntry{}
	for _, e := range st.Requested {
		req[e.Name] = e
	}
	var steps []permStep
	add := func(s permStep) {
		for i := range steps {
			if steps[i].Perm == s.Perm {
				steps = append(steps[:i], steps[i+1:]...)
				break
			}
		}
		steps = append(steps, s)
	}
	checkFixed := func(e permEntry) error {
		if by := e.fixed(); by != "" {
			return fmt.Errorf("%s of %s is fixed by %s and cannot be changed", e.Name, st.Pkg, by)
		}
		return nil
	}

	if len(names) == 0 {
		if action != api.PermissionReset {
			return nil, fmt.Errorf("permission %s needs at least one permission name", action)
		}
		// Reset all: every runtime permission the app requests, and its special-access app-ops.
		for _, e := range st.Requested {
			switch {
			case e.runtime() && e.fixed() == "":
				add(runtimeStep(e.Name, action))
			case e.Op != "":
				add(appOpStep(e, action))
			}
		}
		return steps, nil
	}

	for _, name := range names {
		if name.Raw() {
			e, ok := req[string(name)]
			if !ok {
				return nil, fmt.Errorf("%s does not request %s in its manifest", st.Pkg, name)
			}
			switch {
			case e.runtime():
				if err := checkFixed(e); err != nil {
					return nil, err
				}
				add(runtimeStep(e.Name, action))
			case e.Op != "":
				add(appOpStep(e, action))
			default:
				return nil, fmt.Errorf("%s is a %s permission: Android grants it at install, and it cannot be granted or revoked", name, e.Protection)
			}
			continue
		}
		m, ok := permMap[name]
		if !ok {
			return nil, fmt.Errorf("unknown permission %q", name)
		}
		if m.notImpl != "" {
			return nil, api.NotImplemented(api.Android, fmt.Sprintf("permission %q", name), m.notImpl)
		}
		perms := m.perms
		if st.TargetSdk > 0 && st.TargetSdk < 33 && m.legacy != nil {
			perms = m.legacy
		}
		var asked []permEntry
		for _, p := range perms {
			if e, ok := req[androidPerm+p]; ok && e.runtime() {
				asked = append(asked, e)
			}
		}
		if action == api.PermissionGrant && len(asked) == 0 {
			full := make([]string, len(perms))
			for i, p := range perms {
				full[i] = androidPerm + p
			}
			return nil, fmt.Errorf("cannot grant %s: %s does not request %s in its manifest (a device could not grant it either)",
				name, st.Pkg, strings.Join(full, " or "))
		}
		if action != api.PermissionGrant {
			// Revoke and reset take the background permission first, then the foreground ones.
			for _, p := range m.extra {
				if e, ok := req[androidPerm+p]; ok && e.runtime() {
					asked = append([]permEntry{e}, asked...)
				}
			}
			if name == api.PermLocationAlways {
				slices.Reverse(asked)
			}
		}
		for _, e := range asked {
			if err := checkFixed(e); err != nil {
				return nil, err
			}
			add(runtimeStep(e.Name, action))
		}
		if action == api.PermissionGrant && m.dropBG {
			// "Allow only while using the app": background location ends up not granted, also
			// when an earlier name in this call planned to grant it.
			if e, ok := req[bgLocation]; ok && e.fixed() == "" {
				switch {
				case e.Granted:
					add(permStep{Perm: e.Name, Set: grantSet, Clear: []string{"USER_FIXED", "ONE_TIME"}})
				default:
					steps = slices.DeleteFunc(steps, func(s permStep) bool { return s.Perm == bgLocation })
				}
			}
		}
	}
	// Background location is granted after the foreground pair, as the system dialogs do.
	if i := slices.IndexFunc(steps, func(s permStep) bool { return s.Perm == bgLocation && s.Grant }); i >= 0 {
		s := steps[i]
		steps = append(slices.Delete(steps, i, i+1), s)
	}
	return steps, nil
}

const bgLocation = androidPerm + "ACCESS_BACKGROUND_LOCATION"

// checkApplied compares the state after the steps with what they asked for.
func checkApplied(after permState, steps []permStep) error {
	got := map[string]permEntry{}
	for _, e := range after.Requested {
		got[e.Name] = e
	}
	for _, s := range steps {
		e := got[s.Perm]
		if s.Op != "" {
			if e.Mode != s.Mode {
				return fmt.Errorf("%s: app-op %s is %s after setting it to %s", s.Perm, s.Op, e.Mode, s.Mode)
			}
			continue
		}
		if e.Granted != s.Grant {
			return fmt.Errorf("%s: granted=%v after the change (wanted %v); the system refused it (a hard-restricted permission?)", s.Perm, e.Granted, s.Grant)
		}
		for _, f := range s.Set {
			if !slices.Contains(e.Flags, f) {
				return fmt.Errorf("%s: flag %s not set (flags %v)", s.Perm, f, e.Flags)
			}
		}
		for _, f := range s.Clear {
			if slices.Contains(e.Flags, f) {
				return fmt.Errorf("%s: flag %s not cleared (flags %v)", s.Perm, f, e.Flags)
			}
		}
	}
	return nil
}

// SetPermissions grants, revokes or resets an app's permissions without the system dialog: the
// agent reads the app's requested permissions, the plan is made here (planPermissions), and the
// agent applies it with UiAutomation's grant / revoke (the shell's, as `pm grant`) and the
// permission flags the dialog and Settings leave. App-op permissions (special access such as
// SYSTEM_ALERT_WINDOW, raw names only) get their app-op mode, as the Settings switch does.
func (b *Backend) SetPermissions(ctx context.Context, id, app string, action api.PermissionAction, perms []api.Permission) error {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	var st permState
	if err := d.call(ctx, map[string]any{"cmd": "permstate", "pkg": app}, &st); err != nil {
		return capErr(err)
	}
	steps, err := planPermissions(st, action, perms)
	if err != nil || len(steps) == 0 {
		return err
	}
	// A revoke kills the app's process (as on a device), so its screen goes away; that exit is
	// agemu's, not a crash.
	for _, s := range steps {
		if !s.Grant {
			d.markInput()
			d.expectExit(app)
			break
		}
	}
	var after permState
	if err := d.call(ctx, map[string]any{"cmd": "perms", "pkg": app, "uid": st.UID, "steps": steps}, &after); err != nil {
		return capErr(err)
	}
	return checkApplied(after, steps)
}

// ------------------------------------------------------------------ location

// gpsFix is a GNSS position in WGS84 degrees.
type gpsFix struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	PID int     `json:"pid"` // emulator process it was read from (0: the built-in fallback)
}

// emulator37Fix is the GNSS fix emulator 37 reports before anyone sets one (the constants are in
// its qemu binary). It is only the fallback: the device's default fix is read from the emulator
// itself when agemu starts it (recordDefaultFix).
var emulator37Fix = gpsFix{Lat: 39.237256, Lon: -123.150032}

// sameFix compares fixes to about 1 cm, so a value that went through another representation
// in the emulator still matches.
func sameFix(lat1, lon1, lat2, lon2 float64) bool {
	const eps = 1e-7
	return math.Abs(lat1-lat2) < eps && math.Abs(lon1-lon2) < eps
}

func fixFile(name string) string { return filepath.Join(logDir(), name+".gps.json") }

// recordDefaultFix reads the fix of an emulator process agemu has just started, before anything
// can set one: the emulator's GNSS always reports a fix, so this is the baseline location that
// ClearLocation and Reset restore. It is kept next to the device's log, so a restarted daemon
// still knows it while that emulator process runs.
func (d *device) recordDefaultFix(ctx context.Context, e *emuClient, pid int) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	g, err := e.gps(c)
	if err != nil {
		logf("%s: could not read the emulator's default GPS fix (%v); using emulator 37's", d.name, err)
		return
	}
	f := gpsFix{Lat: g.GetLatitude(), Lon: g.GetLongitude(), PID: pid}
	d.mu.Lock()
	d.fix = f
	d.mu.Unlock()
	if !sameFix(f.Lat, f.Lon, emulator37Fix.Lat, emulator37Fix.Lon) {
		logf("%s: the emulator's default GPS fix is %.6f,%.6f (not emulator 37's)", d.name, f.Lat, f.Lon)
	}
	if b, err := json.Marshal(f); err == nil {
		_ = os.WriteFile(fixFile(d.name), b, 0o644)
	}
}

// defaultFix is the baseline fix of the running emulator: recorded when agemu started it, or
// emulator 37's built-in one for an emulator started before (by another daemon without a
// record, or by hand).
func (d *device) defaultFix() gpsFix {
	d.mu.Lock()
	f, pid := d.fix, d.run.PID
	d.mu.Unlock()
	if f.PID != 0 && f.PID == pid {
		return f
	}
	if b, err := os.ReadFile(fixFile(d.name)); err == nil {
		var saved gpsFix
		if json.Unmarshal(b, &saved) == nil && saved.PID != 0 && saved.PID == pid {
			d.mu.Lock()
			d.fix = saved
			d.mu.Unlock()
			return saved
		}
	}
	return emulator37Fix
}

// setGps sets the fix the emulator's GNSS reports to the guest (what `adb emu geo fix` does).
func (e *emuClient) setGps(ctx context.Context, lat, lon float64) error {
	_, err := e.ctl.SetGps(ctx, &emupb.GpsState{PassiveUpdate: true, Latitude: lat, Longitude: lon, Satellites: 4})
	return err
}

func (e *emuClient) gps(ctx context.Context) (*emupb.GpsState, error) {
	return e.ctl.GetGps(ctx, &emptypb.Empty{})
}

func (b *Backend) SetLocation(ctx context.Context, id string, lat, lon float64) error {
	_, e, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	return e.setGps(ctx, lat, lon)
}

func (b *Backend) ClearLocation(ctx context.Context, id string) error {
	d, e, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	f := d.defaultFix()
	return e.setGps(ctx, f.Lat, f.Lon)
}

// sendDefaultGps sets the default fix before a snapshot load: the fix is emulator-process state
// that a snapshot does not hold, and the guest keeps running during the load, so sending it
// first means the restored guest never sees the old one.
func (d *device) sendDefaultGps(ctx context.Context, e *emuClient) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	f := d.defaultFix()
	if err := e.setGps(c, f.Lat, f.Lon); err != nil {
		logf("%s: could not send the default GPS fix: %v", d.name, err)
	}
}

// restoreGps checks after a snapshot load that the emulator reports the default fix, and sends
// it again if not.
func (d *device) restoreGps(ctx context.Context, e *emuClient) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	f := d.defaultFix()
	g, err := e.gps(c)
	if err == nil && sameFix(g.GetLatitude(), g.GetLongitude(), f.Lat, f.Lon) {
		return
	}
	if err := e.setGps(c, f.Lat, f.Lon); err != nil {
		logf("%s: could not restore the default GPS fix: %v", d.name, err)
	}
}

// ------------------------------------------------------------------ clipboard

// Clipboard reads the primary clip's text as the system clipboard holds it (the agent, as the
// shell, which may read it in the background).
func (b *Backend) Clipboard(ctx context.Context, id string) (string, error) {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return "", err
	}
	var text string
	if err := d.call(ctx, map[string]any{"cmd": "clipget"}, &text); err != nil {
		return "", capErr(err)
	}
	return text, nil
}

// SetClipboard copies plain text as another app copying it would (the clip's source is the
// shell); "" clears the clipboard.
func (b *Backend) SetClipboard(ctx context.Context, id, text string) error {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	return capErr(d.call(ctx, map[string]any{"cmd": "clipset", "text": text}, nil))
}

// ------------------------------------------------------------------ appearance

func (b *Backend) Appearance(ctx context.Context, id string) (api.Appearance, error) {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return "", err
	}
	var res struct {
		Appearance api.Appearance `json:"appearance"`
	}
	if err := d.call(ctx, map[string]any{"cmd": "appearance"}, &res); err != nil {
		return "", capErr(err)
	}
	return res.Appearance, nil
}

// SetAppearance sets the night mode as `cmd uimode night yes|no` does. It returns once the
// global configuration has it and the focused app has redrawn for it: its activity recreated
// and laid out (at most 2 s), or no new window within 300 ms (the app handled the change).
func (b *Backend) SetAppearance(ctx context.Context, id string, a api.Appearance) error {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	d.markInput()
	var res struct {
		ApplyMs  int    `json:"applyMs"`
		SettleMs int    `json:"settleMs"`
		Settled  string `json:"settled"`
	}
	if err := d.call(ctx, map[string]any{"cmd": "appearance", "mode": string(a), "settleMs": 2000, "graceMs": 300}, &res); err != nil {
		return capErr(err)
	}
	if trace {
		logf("%s: appearance %s applied in %d ms, app %s after %d ms", d.name, a, res.ApplyMs, res.Settled, res.SettleMs)
	}
	return nil
}

// ------------------------------------------------------------------ apps

func (b *Backend) Apps(ctx context.Context, id string) ([]api.App, error) {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return nil, err
	}
	var apps []api.App
	if err := d.call(ctx, map[string]any{"cmd": "apps"}, &apps); err != nil {
		return nil, capErr(err)
	}
	return apps, nil
}

// Uninstall removes a user app with its data, for all users (`pm uninstall`).
func (b *Backend) Uninstall(ctx context.Context, id, app string) error {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	defer d.markInput()
	d.expectExit(app)
	return capErr(d.call(ctx, map[string]any{"cmd": "uninstall", "pkg": app}, nil))
}

// ClearAppData is Settings' "Clear storage" (`pm clear`, ActivityManager
// clearApplicationUserData): the app is stopped, its data deleted and its permissions reset.
func (b *Backend) ClearAppData(ctx context.Context, id, app string) error {
	d, _, err := b.capDev(ctx, id)
	if err != nil {
		return err
	}
	defer d.markInput()
	d.expectExit(app)
	return capErr(d.call(ctx, map[string]any{"cmd": "clear", "pkg": app}, nil))
}
