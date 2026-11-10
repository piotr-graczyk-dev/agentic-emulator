//go:build !windows

package ios

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

func TestPlanPermissionsMapsNamesToSimulatorServices(t *testing.T) {
	cases := []struct {
		action api.PermissionAction
		perms  []api.Permission
		want   []privacyOp
	}{
		{api.PermissionGrant, []api.Permission{api.PermCamera}, []privacyOp{{"kTCCServiceCamera", "grant"}}},
		{api.PermissionGrant, []api.Permission{api.PermContacts, api.PermMotion},
			[]privacyOp{{"kTCCServiceAddressBook", "grant"}, {"kTCCServiceMotion", "grant"}}},
		{api.PermissionGrant, []api.Permission{api.PermLocation}, []privacyOp{{"__CoreLocationWhenInUse", "grant"}}},
		{api.PermissionGrant, []api.Permission{api.PermLocationAlways}, []privacyOp{{"__CoreLocationAlways", "grant"}}},
		{api.PermissionRevoke, []api.Permission{api.PermLocationAlways}, []privacyOp{{"__CoreLocationAlways", "revoke"}}},
		{api.PermissionGrant, []api.Permission{api.PermPhotosAdd}, []privacyOp{{"kTCCServicePhotosAdd", "grant"}}},
		{api.PermissionGrant, []api.Permission{api.PermMediaLibrary}, []privacyOp{{"kTCCServiceMediaLibrary", "grant"}}},
		{api.PermissionGrant, []api.Permission{api.PermCalendar, api.PermReminders},
			[]privacyOp{{"kTCCServiceCalendar", "grant"}, {"kTCCServiceReminders", "grant"}}},
		// Photos: the grant has no faithful path, revoke and reset do.
		{api.PermissionRevoke, []api.Permission{api.PermPhotos}, []privacyOp{{"kTCCServicePhotos", "revoke"}}},
		{api.PermissionReset, []api.Permission{api.PermPhotos}, []privacyOp{{"kTCCServicePhotos", "reset"}}},
		// reset without names: every service of the app, location included.
		{api.PermissionReset, nil, []privacyOp{{"kTCCServiceAll", "reset"}}},
	}
	for _, c := range cases {
		got, err := planPermissions(c.action, c.perms)
		if err != nil {
			t.Errorf("%s %v: %v", c.action, c.perms, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v = %v, want %v", c.action, c.perms, got, c.want)
		}
	}
}

func TestPlanPermissionsNotImplementedBeforeAnyChange(t *testing.T) {
	cases := []struct {
		action api.PermissionAction
		perms  []api.Permission
		what   string
	}{
		{api.PermissionGrant, []api.Permission{api.PermNotifications}, `permission "notifications"`},
		{api.PermissionReset, []api.Permission{api.PermNotifications}, `permission "notifications"`},
		{api.PermissionGrant, []api.Permission{api.PermContactsLimited}, `permission "contacts-limited"`},
		{api.PermissionGrant, []api.Permission{api.PermPhotos}, `permission grant "photos"`},
		{api.PermissionGrant, []api.Permission{"android.permission.READ_SMS"}, `permission "android.permission.READ_SMS"`},
		// A not-implemented name anywhere in the list: nothing is planned at all.
		{api.PermissionGrant, []api.Permission{api.PermCamera, api.PermNotifications}, `permission "notifications"`},
	}
	for _, c := range cases {
		ops, err := planPermissions(c.action, c.perms)
		if !errors.Is(err, api.ErrNotImplemented) {
			t.Errorf("%s %v: err %v, want not implemented", c.action, c.perms, err)
			continue
		}
		if ops != nil {
			t.Errorf("%s %v planned %v despite the error", c.action, c.perms, ops)
		}
		if !strings.Contains(err.Error(), c.what+" is not implemented on ios: ") {
			t.Errorf("%s %v: message %q does not name %s with a reason", c.action, c.perms, err, c.what)
		}
	}
	if _, err := planPermissions(api.PermissionGrant, nil); err == nil || errors.Is(err, api.ErrNotImplemented) {
		t.Errorf("grant without names: %v, want an argument error", err)
	}
}

func TestEveryNeutralPermissionHasAnIOSEntry(t *testing.T) {
	for _, p := range api.Permissions {
		s, ok := privacyServices[p]
		if !ok {
			t.Errorf("%s has no iOS entry", p)
			continue
		}
		if s.notImplemented == "" && (s.service == "" || len(s.usage) == 0) {
			t.Errorf("%s: an implemented name needs a service and its usage keys: %+v", p, s)
		}
	}
}

func TestMissingUsageNamesTheKey(t *testing.T) {
	keys := []string{"NSCameraUsageDescription", "NSCalendarsUsageDescription", "NSLocationWhenInUseUsageDescription"}
	if err := missingUsage("app", []api.Permission{api.PermCamera, api.PermLocation}, keys); err != nil {
		t.Fatal(err)
	}
	// The pre-iOS 17 calendar key is accepted as well as the full-access one.
	if err := missingUsage("app", []api.Permission{api.PermCalendar}, keys); err != nil {
		t.Fatal(err)
	}
	err := missingUsage("dev.app", []api.Permission{api.PermMicrophone, api.PermLocationAlways}, keys)
	if err == nil {
		t.Fatal("grant without usage descriptions was accepted")
	}
	for _, want := range []string{"dev.app", "NSMicrophoneUsageDescription (for microphone)", "NSLocationAlwaysAndWhenInUseUsageDescription (for location-always)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "NSLocationWhenInUseUsageDescription") {
		t.Errorf("error %q names a key the app has", err)
	}
}

func TestParseAppDetailsAndApps(t *testing.T) {
	raw := `{"id":"dev.agemu.testapp","installed":true,"type":"User","path":"/x/Containers/Bundle/Application/U/A.app",
		"dataContainer":"/x/Containers/Data/Application/D","groupContainers":{"group.dev":"/x/Containers/Shared/AppGroup/G"},
		"name":"AgemuTest","version":"1.0","usageKeys":["NSCameraUsageDescription"]}`
	var a appDetails
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if !a.Installed || a.Type != "User" || a.GroupContainers["group.dev"] == "" || a.UsageKeys[0] != "NSCameraUsageDescription" {
		t.Fatalf("parsed %+v", a)
	}
	var apps []api.App
	if err := json.Unmarshal([]byte(`[{"id":"b.app","name":"B","version":"2","path":"/p"},{"id":"a.app","name":"","version":""}]`), &apps); err != nil {
		t.Fatal(err)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	out, _ := json.Marshal(apps)
	if string(out) != `[{"id":"a.app"},{"id":"b.app","name":"B","version":"2"}]` {
		t.Fatalf("apps JSON %s", out)
	}
}

func TestParseAppearance(t *testing.T) {
	for _, s := range []string{"light", "dark"} {
		if a, err := parseAppearance(s); err != nil || string(a) != s {
			t.Errorf("%s -> %q, %v", s, a, err)
		}
	}
	if _, err := parseAppearance("unknown"); err == nil {
		t.Error("unknown appearance accepted")
	}
}

func TestReinstallFreshOrderAndFailures(t *testing.T) {
	var calls []string
	step := func(name string, errs ...error) func() error {
		n := 0
		return func() error {
			calls = append(calls, name)
			var err error
			if n < len(errs) {
				err = errs[n]
			}
			n++
			return err
		}
	}
	join := func() string { s := strings.Join(calls, ","); calls = nil; return s }

	if err := reinstallFresh(step("clone"), step("uninstall"), step("install"), step("cleanup"), "/tmp/x/A.app"); err != nil {
		t.Fatal(err)
	}
	if got := join(); got != "clone,uninstall,install,cleanup" {
		t.Fatalf("calls %s", got)
	}
	// No copy: the app is not touched.
	err := reinstallFresh(step("clone", errors.New("ENOSPC")), step("uninstall"), step("install"), step("cleanup"), "b")
	if err == nil || !strings.Contains(err.Error(), "copying the app bundle") || join() != "clone,cleanup" {
		t.Fatalf("clone failure: %v", err)
	}
	// Uninstall failed: the copy is removed, the app is still there.
	if err := reinstallFresh(step("clone"), step("uninstall", errors.New("busy")), step("install"), step("cleanup"), "b"); err == nil || join() != "clone,uninstall,cleanup" {
		t.Fatalf("uninstall failure: %v", err)
	}
	// One failed install is retried.
	if err := reinstallFresh(step("clone"), step("uninstall"), step("install", errors.New("installd")), step("cleanup"), "b"); err != nil || join() != "clone,uninstall,install,install,cleanup" {
		t.Fatalf("install retry: %v", err)
	}
	// Two failed installs: the copy stays and the error says where it is.
	err = reinstallFresh(step("clone"), step("uninstall"), step("install", errors.New("a"), errors.New("b")), step("cleanup"), "/tmp/x/A.app")
	if err == nil || !strings.Contains(err.Error(), "kept at /tmp/x/A.app") || join() != "clone,uninstall,install,install" {
		t.Fatalf("install failure: %v", err)
	}
}

func TestResetOpsOnlyTouchCapabilityGrants(t *testing.T) {
	var g privacyGrants
	raw := `{"tcc":[
		{"service":"kTCCServiceLiverpool","client":"com.apple.Maps"},
		{"service":"kTCCServiceCamera","client":"com.apple.Maps"},
		{"service":"kTCCServiceAddressBook","client":"dev.app"},
		{"service":"kTCCServiceCamera","client":"com.apple.Maps"},
		{"service":"kTCCServiceUbiquity","client":"com.apple.mobilecal"},
		{"service":"kTCCServiceMicrophone","client":"/usr/libexec/sometool","clientType":"1"},
		{"service":"kTCCServiceCamera","client":"/usr/bin/tool"},
		{"service":"kTCCServiceContactsLimited","client":"dev.app","clientType":"0"}],
		"location":["dev.app","com.apple.Maps"]}`
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	want := []privacyReset{
		{"kTCCServiceCamera", "com.apple.Maps"},
		{"kTCCServiceAddressBook", "dev.app"},
		{"kTCCServiceContactsLimited", "dev.app"},
		{"__CoreLocationWhenInUse", "dev.app"},
		{"__CoreLocationWhenInUse", "com.apple.Maps"},
	}
	if got := g.resetOps(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reset ops %v, want %v", got, want)
	}
}

func TestPasteRestoresThePasteboard(t *testing.T) {
	var calls []string
	step := func(name string, err error) func() error {
		return func() error { calls = append(calls, name); return err }
	}
	pasted := func(confirmed bool, err error) func() (bool, error) {
		return func() (bool, error) { calls = append(calls, "paste"); return confirmed, err }
	}
	keep := func() { calls = append(calls, "keep") }
	if err := pasteRestoring(step("save", nil), pasted(true, nil), step("restore", nil), keep); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "save,paste,restore" {
		t.Fatalf("calls %s", got)
	}
	// An unconfirmed paste (secure field, or a second paste) keeps the typed text: a restore
	// could win the race against the app's own read of the pasteboard.
	calls = nil
	if err := pasteRestoring(step("save", nil), pasted(false, nil), step("restore", nil), keep); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "save,paste,keep" {
		t.Fatalf("unconfirmed paste: calls %s, want no restore", got)
	}
	// A failed paste still restores, and its error wins.
	calls = nil
	err := pasteRestoring(step("save", nil), pasted(false, errors.New("hid")), step("restore", errors.New("pb")), keep)
	if err == nil || err.Error() != "hid" || strings.Join(calls, ",") != "save,paste,restore" {
		t.Fatalf("err %v calls %v", err, calls)
	}
	// A failed restore after a good paste is reported.
	calls = nil
	err = pasteRestoring(step("save", nil), pasted(true, nil), step("restore", errors.New("pb")), keep)
	if err == nil || !strings.Contains(err.Error(), "restoring the pasteboard") {
		t.Fatalf("err %v", err)
	}
	// Without a saved pasteboard nothing is pasted.
	calls = nil
	if err := pasteRestoring(step("save", errors.New("conn")), pasted(true, nil), step("restore", nil), keep); err == nil || strings.Join(calls, ",") != "save" {
		t.Fatalf("err %v calls %v", err, calls)
	}
}
