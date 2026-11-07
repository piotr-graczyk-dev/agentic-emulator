package android

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// probeState is what the agent's "permstate" returns for testapps/android/AgemuProbe (a real
// reply from agemu-lab, trimmed to the fields planPermissions reads).
const probeStateJSON = `{"pkg":"dev.agemu.probe","uid":10229,"targetSdk":36,"system":false,"requested":[
{"name":"android.permission.CAMERA","granted":false,"protection":"dangerous","flags":["USER_SENSITIVE_WHEN_GRANTED","USER_SENSITIVE_WHEN_DENIED"]},
{"name":"android.permission.RECORD_AUDIO","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.ACCESS_FINE_LOCATION","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.ACCESS_COARSE_LOCATION","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.ACCESS_BACKGROUND_LOCATION","granted":false,"protection":"dangerous","flags":["RESTRICTION_INSTALLER_EXEMPT"]},
{"name":"android.permission.READ_CONTACTS","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.WRITE_CONTACTS","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.READ_CALENDAR","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.POST_NOTIFICATIONS","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.READ_MEDIA_IMAGES","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.READ_MEDIA_VIDEO","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.READ_MEDIA_VISUAL_USER_SELECTED","granted":false,"protection":"dangerous","flags":[]},
{"name":"android.permission.SYSTEM_ALERT_WINDOW","granted":false,"protection":"signature","op":"android:system_alert_window","mode":"default"},
{"name":"android.permission.VIBRATE","granted":true,"protection":"normal"}]}`

func probeState(t *testing.T, edit func(*permState)) permState {
	t.Helper()
	var st permState
	if err := json.Unmarshal([]byte(probeStateJSON), &st); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&st)
	}
	return st
}

func setEntry(st *permState, name string, f func(*permEntry)) {
	for i := range st.Requested {
		if st.Requested[i].Name == name {
			f(&st.Requested[i])
			return
		}
	}
	panic("no entry " + name)
}

const (
	pCam   = "android.permission.CAMERA"
	pFine  = "android.permission.ACCESS_FINE_LOCATION"
	pCoar  = "android.permission.ACCESS_COARSE_LOCATION"
	pBG    = "android.permission.ACCESS_BACKGROUND_LOCATION"
	pRC    = "android.permission.READ_CONTACTS"
	pWC    = "android.permission.WRITE_CONTACTS"
	pImg   = "android.permission.READ_MEDIA_IMAGES"
	pVid   = "android.permission.READ_MEDIA_VIDEO"
	pVUS   = "android.permission.READ_MEDIA_VISUAL_USER_SELECTED"
	pSAW   = "android.permission.SYSTEM_ALERT_WINDOW"
	pExt   = "android.permission.READ_EXTERNAL_STORAGE"
	pNotif = "android.permission.POST_NOTIFICATIONS"
)

func grantOf(p string) permStep  { return runtimeStep(p, api.PermissionGrant) }
func revokeOf(p string) permStep { return runtimeStep(p, api.PermissionRevoke) }
func resetOf(p string) permStep  { return runtimeStep(p, api.PermissionReset) }

func TestPermMapCoversEveryName(t *testing.T) {
	notImpl := map[api.Permission]bool{api.PermPhotosAdd: true, api.PermContactsLimited: true, api.PermReminders: true, api.PermSiri: true}
	for _, p := range api.Permissions {
		m, ok := permMap[p]
		if !ok {
			t.Errorf("%s has no Android mapping", p)
			continue
		}
		if (m.notImpl != "") != notImpl[p] {
			t.Errorf("%s: notImpl=%q, want not implemented=%v", p, m.notImpl, notImpl[p])
		}
		if m.notImpl == "" && len(m.perms) == 0 {
			t.Errorf("%s maps to no permission", p)
		}
	}
	if len(permMap) != len(api.Permissions) {
		t.Errorf("permMap has %d names, api has %d", len(permMap), len(api.Permissions))
	}
}

func TestRuntimeStepTargetStates(t *testing.T) {
	// The flags the dialog / Settings leave (spec: grant USER_SET; revoke USER_SET+USER_FIXED;
	// reset neither).
	g, r, z := grantOf(pCam), revokeOf(pCam), resetOf(pCam)
	if !g.Grant || !reflect.DeepEqual(g.Set, []string{"USER_SET"}) || !contains(g.Clear, "USER_FIXED") {
		t.Errorf("grant step %+v", g)
	}
	if r.Grant || !reflect.DeepEqual(r.Set, []string{"USER_SET", "USER_FIXED"}) {
		t.Errorf("revoke step %+v", r)
	}
	if z.Grant || len(z.Set) != 0 || !contains(z.Clear, "USER_SET") || !contains(z.Clear, "USER_FIXED") {
		t.Errorf("reset step %+v", z)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPlanPermissions(t *testing.T) {
	bgGranted := func(st *permState) { setEntry(st, pBG, func(e *permEntry) { e.Granted = true }) }
	sawStep := func(mode string) permStep {
		return permStep{Perm: pSAW, Op: "android:system_alert_window", Mode: mode}
	}
	dropBG := permStep{Perm: pBG, Set: []string{"USER_SET"}, Clear: []string{"USER_FIXED", "ONE_TIME"}}
	cases := []struct {
		name   string
		edit   func(*permState)
		action api.PermissionAction
		perms  []api.Permission
		want   []permStep
		errHas string // non-empty: an error containing this
		notImp bool   // the error is api.ErrNotImplemented
	}{
		{name: "grant camera", action: api.PermissionGrant, perms: []api.Permission{"camera"}, want: []permStep{grantOf(pCam)}},
		{name: "revoke camera", action: api.PermissionRevoke, perms: []api.Permission{"camera"}, want: []permStep{revokeOf(pCam)}},
		{name: "reset camera", action: api.PermissionReset, perms: []api.Permission{"camera"}, want: []permStep{resetOf(pCam)}},
		{name: "contacts maps to read and write", action: api.PermissionGrant, perms: []api.Permission{"contacts"},
			want: []permStep{grantOf(pRC), grantOf(pWC)}},
		{name: "calendar: only what the app requests (READ only)", action: api.PermissionGrant, perms: []api.Permission{"calendar"},
			want: []permStep{grantOf("android.permission.READ_CALENDAR")}},
		{name: "notifications", action: api.PermissionGrant, perms: []api.Permission{"notifications"}, want: []permStep{grantOf(pNotif)}},
		{name: "grant location, background not granted", action: api.PermissionGrant, perms: []api.Permission{"location"},
			want: []permStep{grantOf(pFine), grantOf(pCoar)}},
		{name: "grant location steps down from always", edit: bgGranted, action: api.PermissionGrant, perms: []api.Permission{"location"},
			want: []permStep{grantOf(pFine), grantOf(pCoar), dropBG}},
		{name: "grant location-always: foreground first", action: api.PermissionGrant, perms: []api.Permission{"location-always"},
			want: []permStep{grantOf(pFine), grantOf(pCoar), grantOf(pBG)}},
		{name: "revoke location removes all location, background first", action: api.PermissionRevoke, perms: []api.Permission{"location"},
			want: []permStep{revokeOf(pBG), revokeOf(pFine), revokeOf(pCoar)}},
		{name: "revoke location-always, background first", action: api.PermissionRevoke, perms: []api.Permission{"location-always"},
			want: []permStep{revokeOf(pBG), revokeOf(pCoar), revokeOf(pFine)}},
		{name: "reset location", action: api.PermissionReset, perms: []api.Permission{"location"},
			want: []permStep{resetOf(pBG), resetOf(pFine), resetOf(pCoar)}},
		{name: "later name wins: location then location-always", edit: bgGranted, action: api.PermissionGrant,
			perms: []api.Permission{"location", "location-always"}, want: []permStep{grantOf(pFine), grantOf(pCoar), grantOf(pBG)}},
		{name: "later name wins: location-always then location", edit: bgGranted, action: api.PermissionGrant,
			perms: []api.Permission{"location-always", "location"}, want: []permStep{grantOf(pFine), grantOf(pCoar), dropBG}},
		{name: "later name wins: location-always then location, background not granted before", action: api.PermissionGrant,
			perms: []api.Permission{"location-always", "location"}, want: []permStep{grantOf(pFine), grantOf(pCoar)}},
		{name: "later name wins: location then location-always, background not granted before", action: api.PermissionGrant,
			perms: []api.Permission{"location", "location-always"}, want: []permStep{grantOf(pFine), grantOf(pCoar), grantOf(pBG)}},
		{name: "raw background location is granted after the foreground pair", action: api.PermissionGrant,
			perms: []api.Permission{pBG, pFine}, want: []permStep{grantOf(pFine), grantOf(pBG)}},
		{name: "raw background location, then location: not granted", action: api.PermissionGrant,
			perms: []api.Permission{pBG, "location"}, want: []permStep{grantOf(pFine), grantOf(pCoar)}},
		{name: "grant photos (API 33+ app)", action: api.PermissionGrant, perms: []api.Permission{"photos"},
			want: []permStep{grantOf(pImg), grantOf(pVid)}},
		{name: "revoke photos also takes partial access", action: api.PermissionRevoke, perms: []api.Permission{"photos"},
			want: []permStep{revokeOf(pVUS), revokeOf(pImg), revokeOf(pVid)}},
		{name: "photos for an app targeting API < 33", action: api.PermissionGrant, perms: []api.Permission{"photos"},
			edit: func(st *permState) {
				st.TargetSdk = 30
				st.Requested = append(st.Requested, permEntry{Name: pExt, Protection: "dangerous"})
			},
			want: []permStep{grantOf(pExt)}},
		{name: "grant a name the app does not request", action: api.PermissionGrant, perms: []api.Permission{"motion"},
			errHas: "does not request android.permission.ACTIVITY_RECOGNITION in its manifest"},
		{name: "revoke a name the app does not request: nothing to do", action: api.PermissionRevoke, perms: []api.Permission{"motion"}},
		{name: "not implemented name", action: api.PermissionGrant, perms: []api.Permission{"photos-add"}, errHas: `permission "photos-add" is not implemented on android`, notImp: true},
		{name: "every name is checked before planning", action: api.PermissionGrant, perms: []api.Permission{"camera", "siri"}, errHas: `"siri"`, notImp: true},
		{name: "raw runtime permission", action: api.PermissionGrant, perms: []api.Permission{pCam}, want: []permStep{grantOf(pCam)}},
		{name: "raw app-op grant", action: api.PermissionGrant, perms: []api.Permission{pSAW}, want: []permStep{sawStep("allowed")}},
		{name: "raw app-op revoke", action: api.PermissionRevoke, perms: []api.Permission{pSAW}, want: []permStep{sawStep("errored")}},
		{name: "raw app-op reset", action: api.PermissionReset, perms: []api.Permission{pSAW}, want: []permStep{sawStep("default")}},
		{name: "raw normal permission", action: api.PermissionGrant, perms: []api.Permission{"android.permission.VIBRATE"},
			errHas: "android.permission.VIBRATE is a normal permission"},
		{name: "raw permission not requested", action: api.PermissionRevoke, perms: []api.Permission{"android.permission.READ_SMS"},
			errHas: "dev.agemu.probe does not request android.permission.READ_SMS"},
		{name: "policy-fixed", action: api.PermissionGrant, perms: []api.Permission{"camera"},
			edit: func(st *permState) {
				setEntry(st, pCam, func(e *permEntry) { e.Flags = append(e.Flags, "POLICY_FIXED") })
			},
			errHas: "fixed by device policy"},
		{name: "system-fixed", action: api.PermissionRevoke, perms: []api.Permission{"camera"},
			edit:   func(st *permState) { setEntry(st, pCam, func(e *permEntry) { e.Flags = []string{"SYSTEM_FIXED"} }) },
			errHas: "fixed by the system"},
		{name: "grant needs a name", action: api.PermissionGrant, errHas: "needs at least one permission"},
		{name: "reset all: runtime permissions and app-ops, not normal ones", action: api.PermissionReset,
			edit: func(st *permState) { setEntry(st, pCam, func(e *permEntry) { e.Flags = []string{"SYSTEM_FIXED"} }) },
			want: []permStep{
				resetOf("android.permission.RECORD_AUDIO"), resetOf(pFine), resetOf(pCoar), resetOf(pBG), resetOf(pRC), resetOf(pWC),
				resetOf("android.permission.READ_CALENDAR"), resetOf(pNotif), resetOf(pImg), resetOf(pVid), resetOf(pVUS), sawStep("default"),
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := planPermissions(probeState(t, c.edit), c.action, c.perms)
			if c.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("err = %v, want one containing %q", err, c.errHas)
				}
				if errors.Is(err, api.ErrNotImplemented) != c.notImp {
					t.Fatalf("errors.Is(err, ErrNotImplemented) = %v, want %v", !c.notImp, c.notImp)
				}
				if got != nil {
					t.Fatalf("steps %v planned despite the error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("steps\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestPermStepJSON(t *testing.T) {
	b, _ := json.Marshal([]permStep{grantOf(pCam), {Perm: pSAW, Op: "android:system_alert_window", Mode: "allowed"}})
	want := `[{"perm":"android.permission.CAMERA","grant":true,"set":["USER_SET"],"clear":["USER_FIXED","ONE_TIME","AUTO_REVOKED"]},` +
		`{"perm":"android.permission.SYSTEM_ALERT_WINDOW","grant":false,"op":"android:system_alert_window","mode":"allowed"}]`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestCheckApplied(t *testing.T) {
	steps := []permStep{grantOf(pCam), revokeOf(pFine), {Perm: pSAW, Op: "android:system_alert_window", Mode: "allowed"}}
	ok := func(st *permState) {
		setEntry(st, pCam, func(e *permEntry) { e.Granted, e.Flags = true, []string{"USER_SET", "USER_SENSITIVE_WHEN_GRANTED"} })
		setEntry(st, pFine, func(e *permEntry) { e.Granted, e.Flags = false, []string{"USER_SET", "USER_FIXED"} })
		setEntry(st, pSAW, func(e *permEntry) { e.Mode = "allowed" })
	}
	if err := checkApplied(probeState(t, ok), steps); err != nil {
		t.Fatalf("applied state rejected: %v", err)
	}
	bad := []struct {
		name, has string
		edit      func(*permState)
	}{
		{"not granted", "granted=false", func(st *permState) { setEntry(st, pCam, func(e *permEntry) { e.Granted = false }) }},
		{"flag not set", "flag USER_FIXED not set", func(st *permState) { setEntry(st, pFine, func(e *permEntry) { e.Flags = []string{"USER_SET"} }) }},
		{"flag not cleared", "flag USER_FIXED not cleared", func(st *permState) {
			setEntry(st, pCam, func(e *permEntry) { e.Flags = []string{"USER_SET", "USER_FIXED"} })
		}},
		{"app-op mode", "is default after setting it to allowed", func(st *permState) { setEntry(st, pSAW, func(e *permEntry) { e.Mode = "default" }) }},
	}
	for _, c := range bad {
		st := probeState(t, ok)
		c.edit(&st)
		if err := checkApplied(st, steps); err == nil || !strings.Contains(err.Error(), c.has) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.has)
		}
	}
}

func TestCapErr(t *testing.T) {
	for in, want := range map[string]string{
		"agent: IllegalArgumentException: no.such.app is not installed":     "no.such.app is not installed",
		"agent: IllegalStateException: night mode did not change":           "night mode did not change",
		"agent: SecurityException: uid 2000 does not have android.perm.FOO": "agent: SecurityException: uid 2000 does not have android.perm.FOO",
	} {
		if got := capErr(errors.New(in)).Error(); got != want {
			t.Errorf("capErr(%q) = %q, want %q", in, got, want)
		}
	}
	if capErr(nil) != nil {
		t.Error("capErr(nil) != nil")
	}
}

func TestAppsParse(t *testing.T) {
	// The agent's "apps" reply (Caps.apps) decodes into api.App as the daemon returns it.
	var apps []api.App
	if err := json.Unmarshal([]byte(`[{"id":"dev.agemu.probe","name":"Agemu Probe","version":"1.2"},{"id":"x.y"}]`), &apps); err != nil {
		t.Fatal(err)
	}
	want := []api.App{{ID: "dev.agemu.probe", Name: "Agemu Probe", Version: "1.2"}, {ID: "x.y"}}
	if !reflect.DeepEqual(apps, want) {
		t.Fatalf("got %+v", apps)
	}
}

func TestSameFix(t *testing.T) {
	if !sameFix(39.237256, -123.150032, 39.2372560000001, -123.150032) {
		t.Error("a value that differs in the last bits should match")
	}
	if sameFix(39.237256, -123.150032, 39.237356, -123.150032) {
		t.Error("fixes 11 m apart should not match")
	}
}

func TestDefaultFix(t *testing.T) {
	t.Setenv("AGEMU_HOME", t.TempDir())
	d := &device{name: "agemu-fixtest"}
	d.run.PID = 4242
	if got := d.defaultFix(); got != emulator37Fix {
		t.Fatalf("no record: got %+v, want emulator 37's fix", got)
	}
	// A record from this emulator process (as a restarted daemon finds it) is used.
	if err := os.MkdirAll(logDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixFile(d.name), []byte(`{"lat":1.5,"lon":-2.5,"pid":4242}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := d.defaultFix(); got.Lat != 1.5 || got.Lon != -2.5 {
		t.Fatalf("recorded fix not used: %+v", got)
	}
	// A record from another emulator process is not.
	d2 := &device{name: "agemu-fixtest"}
	d2.run.PID = 4343
	if got := d2.defaultFix(); got != emulator37Fix {
		t.Fatalf("stale record used: %+v", got)
	}
}
