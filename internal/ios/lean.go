package ios

// Lean simulators. A booted iOS 26.5 simulator runs the whole iOS userland: about 240
// processes and 4 GB of memory (summed phys_footprint), and for the first minute after boot
// it keeps 1.5 cores busy (media analysis, asset downloads, Siri voices, the wallpaper
// gallery). Most of that serves features an app under test does not use. agemu turns those
// launchd jobs off in its own simulators. The overrides are written with `launchctl disable`
// inside the device, so they persist and later boots start without the jobs; jobs that run
// when agemu first sees the device are stopped at once (`launchctl bootout`).
// AGEMU_IOS_LEAN=0 turns them back on; they start again from the next boot.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// leanJobs are the launchd jobs a lean simulator does not run, by the feature they serve.
// An app that uses one of these features needs AGEMU_IOS_LEAN=0.
var leanJobs = []string{
	// Home screen widgets (WidgetKit timelines). Widgets show empty.
	"com.apple.chronod",
	// Photo and media analysis (Photos memories, people, scene search).
	"com.apple.mediaanalysisd", "com.apple.mediaanalysisd.service", "com.apple.photoanalysisd",
	// On-demand system asset downloads (Siri voices, dictation, linguistic data, fonts). On the
	// first boot it stages about 1 GB into the device.
	"com.apple.mobileassetd",
	// Siri, text to speech, speech recognition, App Intents donations.
	"com.apple.sirittsd", "com.apple.assistantd", "com.apple.assistant_service", "com.apple.assistant_cdmd",
	"com.apple.siriinferenced", "com.apple.siriactionsd", "com.apple.siriknowledged", "com.apple.corespeechd",
	// Experiments, App Store engagement messages, ads.
	"com.apple.triald", "com.apple.amsengagementd", "com.apple.ap.promotedcontentd",
	// Apple News.
	"com.apple.newsd",
	// Fitness coaching and health content (HealthKit itself, healthd, keeps running).
	"com.apple.fitnesscoachingd", "com.apple.fitnessintelligenced", "com.apple.fitcore", "com.apple.healthcontentd",
	// Spotlight knowledge and Siri suggestions (CoreSpotlight indexing keeps running).
	"com.apple.spotlightknowledged", "com.apple.spotlightknowledged.updater", "com.apple.suggestd",
	// HomeKit.
	"com.apple.homed",
	// Significant locations and visit monitoring.
	"com.apple.routined",
	// Apple Intelligence models and knowledge.
	"com.apple.intelligenceplatformd", "com.apple.modelcatalogd",
	// Watch faces, Screen Time, iCloud Mail, the music library.
	"com.apple.nanotimekitcompaniond", "com.apple.ScreenTimeAgent", "com.apple.icloudmailagent", "com.apple.itunescloudd",
	// Settings sync to a paired Apple Watch. Pairing itself (nanoregistryd) and WatchConnectivity
	// (wcd) keep running. The other watch sync jobs (NPKCompanionAgent, bulletindistributord,
	// addressbooksyncd, eventkitsyncd, appconduitd, companionappd) are not here: nanoregistrylaunchd
	// turns them back on and starts them at every boot.
	"com.apple.nanoprefsyncd.2",
	// Continuity with the user's other devices (Handoff discovery, iPhone Mirroring widgets) and CarPlay.
	"com.apple.rapportd", "com.apple.replicatord", "com.apple.carkitd",
	// Screen Time usage tracking and Ask to Buy.
	"com.apple.UsageTrackingAgent", "com.apple.askpermissiond",
	// Shared with You, Memoji, Find My, Personal Voice. (Not FinanceKit's financed: without it
	// passd, Wallet and Apple Pay, crashed every 20 s.)
	"com.apple.sociallayerd", "com.apple.avatarsd", "com.apple.findmy.findmylocated", "com.apple.voicebankingd",
	// Maps turn-by-turn navigation (MapKit search and directions use geod, which keeps running).
	"com.apple.navd",
	// Safari password monitoring and iCloud bookmark sync; Siri suggestion feedback.
	"com.apple.Safari.passwordbreachd", "com.apple.SafariBookmarksSyncAgent", "com.apple.parsec-fbf",
	// Image Playground and Genmoji, HomeKit events, diagnostic extensions for sysdiagnose.
	"com.apple.generativeexperiencesd", "com.apple.homeeventsd", "com.apple.diagnosticextensionsd",
}

func leanOn() bool {
	switch strings.ToLower(os.Getenv("AGEMU_IOS_LEAN")) {
	case "0", "false", "no":
		return false
	}
	return true
}

// leanDevice brings a booted agemu simulator to the lean setting (or, with AGEMU_IOS_LEAN=0,
// back to the full one) in the background: the first time on a device it takes 10-20 s (two
// launchctl runs per job; 36 s on a busy host), and the device works meanwhile. A device is
// checked once per daemon and again after every cold boot; a request that arrives while the
// device is being reconciled runs once that finishes. After a cold boot of a lean device the
// wallpaper gallery and what iOS starts in the background are stopped too, each on its own
// schedule (a newer boot restarts them), and PosterBoard's gallery is kept from starting at later
// boots (see posterboard.go; galleryBefore is its newest update entry before this boot). Failures
// are logged and leave the device unchecked, so the next boot tries again: a device that is not
// lean still works.
func (b *Backend) leanDevice(d simDevice, cold bool, galleryBefore string) {
	if !managed(d.Name) {
		return
	}
	on := leanOn()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if on && cold {
		b.bootGen[d.UDID]++
		gen := b.bootGen[d.UDID]
		b.bgWG.Add(2)
		go func() {
			defer b.bgWG.Done()
			b.stopWallpaperGallery(d, gen, galleryBefore)
		}()
		go func() {
			defer b.bgWG.Done()
			b.stopBackgroundApps(d, gen)
		}()
	}
	if state, checked := b.lean[d.UDID]; checked && state == on && !cold {
		return
	}
	if b.leaning[d.UDID] {
		b.leanAgain[d.UDID] = true
		return
	}
	b.leaning[d.UDID] = true
	b.bgWG.Add(1)
	go func() {
		defer b.bgWG.Done()
		for {
			ctx, cancel := context.WithTimeout(b.bg, 2*time.Minute)
			b.applyLean(ctx, d, on)
			cancel()
			b.mu.Lock()
			again := b.leanAgain[d.UDID] && !b.closed
			delete(b.leanAgain, d.UDID)
			if !again {
				delete(b.leaning, d.UDID)
				b.mu.Unlock()
				return
			}
			b.mu.Unlock()
		}
	}()
}

// applyLean makes the device's launchd overrides match on (see leanDevice), then reads them
// back: the device is marked as checked only when every lean job matches.
func (b *Backend) applyLean(ctx context.Context, d simDevice, on bool) {
	// Until this reconciliation is verified the device counts as unchecked, so a failure (or a
	// change since an earlier success) is retried by the next boot request.
	b.mu.Lock()
	delete(b.lean, d.UDID)
	b.mu.Unlock()
	if !on {
		// A failure here (a defaults spawn that failed just after boot, say) is logged and
		// leaves the device unchecked, so the next boot tries again; the launchd jobs below are
		// turned back on regardless.
		unfrozen := unfreezeGallery(devicePosterBoard{b, d.UDID}, d.Name)
		if unfrozen != nil {
			log.Printf("ios lean %s: turning PosterBoard's keynote mode off: %v (tried again at the next boot)", d.Name, unfrozen)
			defer func() {
				b.mu.Lock()
				delete(b.lean, d.UDID)
				b.mu.Unlock()
			}()
		}
	}
	start := time.Now()
	out, err := b.launchctl(ctx, d.UDID, "print-disabled", "system")
	if err != nil {
		log.Printf("ios lean %s: reading launchd overrides: %v", d.Name, err)
		return
	}
	todo := leanMismatches(disabledJobs(out), on)
	if len(todo) > 0 {
		// Each launchctl run in the device takes ~175 ms, so they run 8 at a time.
		var wg sync.WaitGroup
		slots := make(chan struct{}, 8)
		verb := "enable"
		if on {
			verb = "disable"
		}
		for _, j := range todo {
			wg.Add(1)
			slots <- struct{}{}
			go func(j string) {
				defer func() { <-slots; wg.Done() }()
				if _, err := b.launchctl(ctx, d.UDID, verb, "system/"+j); err != nil {
					log.Printf("ios lean %s: %s %s: %v", d.Name, verb, j, err)
					return
				}
				if on {
					// Stops the job if it runs now; for a job that is not loaded it only reports so.
					_, _ = b.launchctl(ctx, d.UDID, "bootout", "system/"+j)
				}
			}(j)
		}
		wg.Wait()
		// launchctl's exit status does not come back through the helper, so check the result.
		if out, err = b.launchctl(ctx, d.UDID, "print-disabled", "system"); err != nil {
			log.Printf("ios lean %s: reading launchd overrides back: %v", d.Name, err)
			return
		}
		if left := leanMismatches(disabledJobs(out), on); len(left) > 0 {
			log.Printf("ios lean %s: %d of %d launchd jobs did not change (tried again at the next boot): %s",
				d.Name, len(left), len(todo), strings.Join(left, ", "))
			return
		}
		what := "turned off"
		if !on {
			what = "turned back on (they start at the next boot)"
		}
		log.Printf("ios lean %s: %d launchd jobs %s in %.0f ms", d.Name, len(todo), what, ms(time.Since(start)))
	}
	b.mu.Lock()
	b.lean[d.UDID] = on
	b.mu.Unlock()
}

// leanMismatches lists the lean jobs whose override does not match on yet.
func leanMismatches(disabled map[string]bool, on bool) []string {
	var m []string
	for _, j := range leanJobs {
		if on != disabled[j] {
			m = append(m, j)
		}
	}
	return m
}

// launchctl runs the simulator's own launchctl in the device.
func (b *Backend) launchctl(ctx context.Context, udid string, args ...string) (string, error) {
	return b.spawn(ctx, udid, "bin/launchctl", args...)
}

var disabledLine = regexp.MustCompile(`"([^"]+)"\s*=>\s*disabled`)

// disabledJobs parses `launchctl print-disabled system`.
func disabledJobs(out string) map[string]bool {
	m := map[string]bool{}
	for _, g := range disabledLine.FindAllStringSubmatch(out, -1) {
		m[g[1]] = true
	}
	return m
}

// stopWallpaperGallery stops PosterBoard's wallpaper gallery after a cold boot. PosterBoard
// starts one extension per wallpaper provider in its gallery (9-15 processes of 35-75 MB, each
// rendering for 1-3 s of CPU; on the first boot of a new device up to 21) and keeps them; they
// serve the wallpaper gallery, which an app under test does not open. The providers of the
// posters in use keep running: the lock screen's (in iOS 26.5 the default is
// com.apple.MercuryPoster) and any contact poster's. PosterBoard's data store says which they
// are (see postersInUse). Nothing is stopped until the store has been read, and only the
// gallery's own extensions are (see galleryToStop).
// After the first 45 s stopped gallery extensions stayed stopped, also through lock and unlock,
// and the lock screen drew its wallpaper, clock and notifications. The passes also turn
// PosterBoard's keynote mode on once it has built its gallery (freezeGalleryStep; before is its
// newest update entry before this boot), after which later boots start no gallery extension and
// these passes find none. They run on the schedule of nextGalleryPass. A newer cold boot of the
// device (gen) or the backend closing ends it.
func (b *Backend) stopWallpaperGallery(d simDevice, gen int, before string) {
	ready := time.Now()
	stopped := map[string]int{}
	var keep map[string]bool
	var keepErr error
	settled := false
	at, more := nextGalleryPass(0, false, false, false)
	for more {
		select {
		case <-b.bg.Done():
			return
		case <-time.After(time.Until(ready.Add(at))):
		}
		if !b.currentBoot(d.UDID, gen) {
			return
		}
		// A pass that falls into a park runs once the simulator is resumed, and the later
		// passes move by as long.
		if held, ok := b.waitWhileParked(d.UDID, gen); !ok {
			return
		} else if held >= parkPoll {
			ready = ready.Add(held)
		}
		// ww: the provider is in the extension's launch arguments, so the command lines must not
		// be cut (ps cuts them to the terminal's width when it has one).
		out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
		if err != nil {
			log.Printf("ios lean %s: ps: %v", d.Name, err)
			return
		}
		exts, data := wallpaperExtensions(string(out), d.UDID)
		if keep == nil && len(exts) > 0 {
			// Read once per boot. The first boot of a new device writes the lock screen's poster
			// about 25 s after the device is ready (later on a busy host), so until then a later
			// pass tries again.
			keep, keepErr = postersInUse(b.bg, data)
		}
		n := 0
		for _, e := range galleryToStop(exts, keep) {
			if syscall.Kill(e.pid, syscall.SIGTERM) == nil {
				stopped[e.provider]++
				n++
			}
		}
		if !settled {
			settled = freezeGalleryStep(devicePosterBoard{b, d.UDID}, before, d.Name)
		}
		at, more = nextGalleryPass(at, keep != nil, n > 0, settled)
	}
	if keep == nil && keepErr != nil {
		log.Printf("ios lean %s: wallpaper gallery left running: %v", d.Name, keepErr)
	}
	if !settled {
		log.Printf("ios lean %s: PosterBoard did not finish building its wallpaper gallery in %v; its keynote mode stays off until a later boot", d.Name, galleryRetryUntil)
	}
	if len(stopped) > 0 {
		log.Printf("ios lean %s: stopped %s; kept the posters in use: %s", d.Name, stoppedSummary(stopped), strings.Join(sortedKeys(keep), ", "))
	}
}

// currentBoot reports whether gen is still the device's latest cold boot (see leanDevice).
func (b *Backend) currentBoot(udid string, gen int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bootGen[udid] == gen
}

// galleryPasses are the fixed passes of stopWallpaperGallery, as times after the device is
// ready: the gallery usually starts within 3 s, in some boots later.
var galleryPasses = []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second, 45 * time.Second}

const (
	galleryRetryEvery = 15 * time.Second
	galleryRetryUntil = 5 * time.Minute
)

// nextGalleryPass returns when the pass after the one at last runs (as a time after the device is
// ready), or false when the stop is done. After the fixed passes it goes on every 15 s, up to 5
// min after ready, while the lock screen's providers are not known yet (the first boot of a new
// device), the last pass stopped an extension (the gallery was still starting), or PosterBoard's
// keynote mode is not settled yet (settled false: PosterBoard is still building its gallery).
func nextGalleryPass(last time.Duration, known, stoppedSome, settled bool) (time.Duration, bool) {
	for _, p := range galleryPasses {
		if p > last {
			return p, true
		}
	}
	if (known && !stoppedSome && settled) || last+galleryRetryEvery > galleryRetryUntil {
		return 0, false
	}
	return last + galleryRetryEvery, true
}

// notGallery are poster providers that live next to the gallery's but serve something an app can
// show: contact posters (CNContactViewController, the call screen) and Messages' conversation
// backgrounds. They are never stopped. The Photos provider is also a gallery wallpaper, but it
// renders photo contact posters too. (Only MonogramPosterExtension was seen running; the others
// are kept by design.)
var notGallery = map[string]bool{
	"com.apple.ContactsUI.MonogramPosterExtension":           true,
	"com.apple.AvatarUI.AvatarPosterExtension":               true,
	"com.apple.PhotosUIPrivate.PhotosPosterProvider":         true,
	"com.apple.transcriptBackgroundPoster.DynamicExtension":  true,
	"com.apple.transcriptBackgroundPoster.GradientExtension": true,
}

// galleryToStop picks the gallery extensions to stop: those whose provider is known, is not
// one of the posters in use (keep) and is not in notGallery. With keep nil (the data store has not
// been read) it picks none.
func galleryToStop(exts []galleryExtension, keep map[string]bool) []galleryExtension {
	if keep == nil {
		return nil
	}
	var stop []galleryExtension
	for _, e := range exts {
		if e.provider != "" && !keep[e.provider] && !notGallery[e.provider] {
			stop = append(stop, e)
		}
	}
	return stop
}

// stoppedSummary formats stopped providers as "13 wallpaper gallery extensions (a, b x2, ...)".
func stoppedSummary(stopped map[string]int) string {
	short := map[string]int{}
	for p, n := range stopped {
		short[strings.TrimPrefix(p, "com.apple.")] += n
	}
	return countedNames(short, "wallpaper gallery extensions")
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// posterExtension matches the executable of a wallpaper gallery extension: inside the simulator
// runtime's root, in the places the gallery's providers live in iOS 26.5:
// /System/Library/ExtensionKit/Extensions, poster apps (/Applications/EmojiPoster.app/Extensions,
// /System/Library/CoreServices/AegirProxyApp.app/Extensions) and AmbientUI.framework/PlugIns
// (StandBy). An app's own extensions (in the device's data directory) and posters inside other
// frameworks (ContactsUI's MonogramPosterExtension) do not match.
var posterExtension = regexp.MustCompile(`^/(?:[^/]+/)*[^/]+\.simruntime/Contents/Resources/RuntimeRoot/` +
	`(?:Applications/[^/]+\.app/Extensions|System/Library/CoreServices/[^/]+\.app/Extensions|` +
	`System/Library/ExtensionKit/Extensions|System/Library/PrivateFrameworks/AmbientUI\.framework/PlugIns)/` +
	`[^/]*Poster[^/]*\.appex/`)

// galleryExtension is a running wallpaper extension and the provider (its bundle identifier)
// it serves.
type galleryExtension struct {
	pid      int
	provider string
}

// wallpaperExtensions finds, in `ps -axwwo pid=,ppid=,command=` output, the wallpaper gallery
// extension processes (posterExtension) that the simulator's launchd_sim started, and the
// device's data directory (from launchd_sim's argument).
func wallpaperExtensions(ps, udid string) (exts []galleryExtension, data string) {
	procs, data := simChildren(ps, udid)
	for _, p := range procs {
		if posterExtension.MatchString(p.cmd) {
			exts = append(exts, galleryExtension{p.pid, launchService(p.cmd)})
		}
	}
	return exts, data
}

// simProc is a process that a simulator's launchd_sim started, with its command line.
type simProc struct {
	pid int
	cmd string
}

// simChildren finds, in `ps -axwwo pid=,ppid=,command=` output, the processes that the
// simulator's launchd_sim started, and the device's data directory (from launchd_sim's
// argument).
func simChildren(ps, udid string) (children []simProc, data string) {
	type proc struct {
		ppid int
		cmd  string
	}
	procs := map[int]proc{}
	root := 0
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		cmd := strings.Join(f[2:], " ")
		procs[pid] = proc{ppid, cmd}
		// launchd_sim's argv[0] is "launchd_sim"; its argument is the device's bootstrap plist,
		// <device>/data/var/run/launchd_bootstrap.plist.
		if (f[2] == "launchd_sim" || strings.HasSuffix(f[2], "/launchd_sim")) && strings.Contains(cmd, udid) {
			root = pid
			arg := strings.TrimSpace(strings.TrimPrefix(cmd, f[2]))
			if i := strings.Index(arg, "/"+udid+"/data/"); i >= 0 {
				data = arg[:i+len("/"+udid+"/data")]
			}
		}
	}
	if root == 0 {
		return nil, ""
	}
	for pid, p := range procs {
		if p.ppid == root {
			children = append(children, simProc{pid, p.cmd})
		}
	}
	return children, data
}

var launchArguments = regexp.MustCompile(`-LaunchArguments ([A-Za-z0-9+/=_-]+)`)

// launchService reads the extension's bundle identifier from its command line: ExtensionKit
// passes `-LaunchArguments <base64 JSON>` with a "serviceName". It returns "" when there is none.
func launchService(cmd string) string {
	m := launchArguments.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(m[1], "=")); err != nil {
			return ""
		}
	}
	var args struct {
		ServiceName string `json:"serviceName"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	return args.ServiceName
}

// sqlite3 is the host's SQLite shell, which postersInUse runs read-only on the device's poster
// store (a fixed path, not whichever sqlite3 comes first in the daemon's PATH).
const sqlite3 = "/usr/bin/sqlite3"

// postersInUse reads PosterBoard's data store in the device's data directory and returns the
// providers of the posters in use: the lock screen's, and those of contact posters (the incoming
// call role; none was in the stores checked, so that part is kept by design). The store is a
// SQLite database in a directory numbered by its schema version (61 on iOS 26.5); the newest one
// is read with the host's sqlite3 in read-only mode (no ~/.sqliterc). Any failure is an error,
// and then nothing is stopped.
func postersInUse(ctx context.Context, data string) (map[string]bool, error) {
	if data == "" {
		return nil, fmt.Errorf("the device's data directory is not known")
	}
	dbs, _ := filepath.Glob(filepath.Join(data, "Library/Application Support/PRBPosterExtensionDataStore/*/PBFPosterExtensionDataStoreSQLiteDatabase.sqlite3"))
	if len(dbs) == 0 {
		return nil, fmt.Errorf("no PosterBoard data store in %s", data)
	}
	version := func(db string) int {
		v, _ := strconv.Atoi(filepath.Base(filepath.Dir(db)))
		return v
	}
	sort.Slice(dbs, func(i, j int) bool { return version(dbs[i]) > version(dbs[j]) })
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, sqlite3, "-readonly", "-batch", "-init", os.DevNull, "-list", "-noheader", "-separator", "|", dbs[0],
		"SELECT DISTINCT m.roleId, p.providerId FROM poster p JOIN posterRoleMembership m ON m.posterUUID = p.UUID "+
			"WHERE m.roleId IN ('PRPosterRoleLockScreen', 'PRPosterRoleIncomingCall')").Output()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", dbs[0], err)
	}
	keep := map[string]bool{}
	lockScreen := false
	for _, l := range strings.Split(string(out), "\n") {
		role, provider, ok := strings.Cut(strings.TrimSpace(l), "|")
		if !ok || provider == "" {
			continue
		}
		keep[provider] = true
		lockScreen = lockScreen || role == "PRPosterRoleLockScreen"
	}
	if !lockScreen {
		return nil, fmt.Errorf("no lock screen poster in %s yet", dbs[0])
	}
	return keep, nil
}
