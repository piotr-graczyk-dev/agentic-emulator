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
	"log"
	"os"
	"os/exec"
	"regexp"
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
}

func leanOn() bool {
	switch strings.ToLower(os.Getenv("AGEMU_IOS_LEAN")) {
	case "0", "false", "no":
		return false
	}
	return true
}

// leanDevice brings a booted agemu simulator to the lean setting (or, with AGEMU_IOS_LEAN=0,
// back to the full one) in the background: the first time on a device it takes ~10 s (64
// launchctl runs), and the device works meanwhile. A device is checked once per daemon and
// again after every cold boot; a request that arrives while the device is being reconciled runs
// once that finishes. After a cold boot of a lean device the wallpaper gallery is stopped too,
// on its own schedule (a newer boot restarts it). Failures are logged and leave the device
// unchecked, so the next boot tries again: a device that is not lean still works.
func (b *Backend) leanDevice(d simDevice, cold bool) {
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
		b.wallGen[d.UDID]++
		gen := b.wallGen[d.UDID]
		b.bgWG.Add(1)
		go func() {
			defer b.bgWG.Done()
			b.stopWallpaperGallery(d, gen)
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
	var r struct {
		Stdout string `json:"stdout"`
	}
	err := b.call(ctx, "spawn", map[string]any{"udid": udid, "path": "bin/launchctl", "args": args, "timeoutMs": 10000}, &r)
	return r.Stdout, err
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

// stopWallpaperGallery stops PosterBoard's wallpaper extensions after a cold boot. PosterBoard
// starts one per wallpaper in its gallery (6-16 processes of 35-75 MB, each rendering for 1-3
// s of CPU) and keeps them. iOS relaunches the one the lock screen shows when it is needed (the
// lock screen drew normally right after); the others stay stopped. They usually start within
// 3 s of the device being ready, in some boots later, hence passes 3, 8, 20 and 45 s after the
// device is ready. A newer cold boot of the device (gen) or the backend closing ends it.
func (b *Backend) stopWallpaperGallery(d simDevice, gen int) {
	n := 0
	for _, wait := range []time.Duration{3 * time.Second, 5 * time.Second, 12 * time.Second, 25 * time.Second} {
		select {
		case <-b.bg.Done():
			return
		case <-time.After(wait):
		}
		b.mu.Lock()
		current := b.wallGen[d.UDID] == gen
		b.mu.Unlock()
		if !current {
			return
		}
		out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
		if err != nil {
			log.Printf("ios lean %s: ps: %v", d.Name, err)
			return
		}
		for _, pid := range wallpaperExtensions(string(out), d.UDID) {
			if syscall.Kill(pid, syscall.SIGTERM) == nil {
				n++
			}
		}
	}
	if n > 0 {
		log.Printf("ios lean %s: stopped %d wallpaper gallery extensions", d.Name, n)
	}
}

var posterExtension = regexp.MustCompile(`/ExtensionKit/Extensions/[^/]*Poster[^/]*\.appex/`)

// wallpaperExtensions finds, in `ps -axo pid=,ppid=,command=` output, the wallpaper extension
// processes that the simulator's launchd_sim started.
func wallpaperExtensions(ps, udid string) []int {
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
		// launchd_sim's argv[0] is "launchd_sim"; its argument is the device's bootstrap plist.
		if (f[2] == "launchd_sim" || strings.HasSuffix(f[2], "/launchd_sim")) && strings.Contains(cmd, udid) {
			root = pid
		}
	}
	if root == 0 {
		return nil
	}
	var pids []int
	for pid, p := range procs {
		if p.ppid == root && posterExtension.MatchString(p.cmd) {
			pids = append(pids, pid)
		}
	}
	return pids
}
