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

// leanDevice applies the lean job set to a booted agemu simulator, or with AGEMU_IOS_LEAN=0
// undoes it, in the background: the first time on a device it takes ~11 s (64 launchctl runs),
// and the device works meanwhile. A device is checked once per daemon and again after every
// cold boot; after a cold boot of a lean device the wallpaper gallery is stopped too. Failures
// are logged: a device that is not lean still works.
func (b *Backend) leanDevice(d simDevice, cold bool) {
	if !managed(d.Name) {
		return
	}
	on := leanOn()
	b.mu.Lock()
	state, checked := b.lean[d.UDID]
	if b.leaning[d.UDID] || (checked && state == on && !cold) {
		b.mu.Unlock()
		return
	}
	b.leaning[d.UDID] = true
	b.mu.Unlock()
	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.leaning, d.UDID)
			b.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		b.applyLean(ctx, d, on)
		if on && cold {
			stopWallpaperGallery(d.UDID, d.Name)
		}
	}()
}

// applyLean makes the device's launchd overrides match on (see leanDevice).
func (b *Backend) applyLean(ctx context.Context, d simDevice, on bool) {
	start := time.Now()
	out, err := b.launchctl(ctx, d.UDID, "print-disabled", "system")
	if err != nil {
		log.Printf("ios lean %s: reading launchd overrides: %v", d.Name, err)
		return
	}
	disabled := disabledJobs(out)
	var todo []string
	for _, j := range leanJobs {
		if on != disabled[j] {
			todo = append(todo, j)
		}
	}
	// Each launchctl run in the device takes ~175 ms, so they run 8 at a time.
	var (
		mu      sync.Mutex
		changed []string
		wg      sync.WaitGroup
		slots   = make(chan struct{}, 8)
	)
	for _, j := range todo {
		wg.Add(1)
		slots <- struct{}{}
		go func(j string) {
			defer func() { <-slots; wg.Done() }()
			verb := "enable"
			if on {
				verb = "disable"
			}
			if _, err := b.launchctl(ctx, d.UDID, verb, "system/"+j); err != nil {
				log.Printf("ios lean %s: %s %s: %v", d.Name, verb, j, err)
				return
			}
			if on {
				// Stops the job if it runs now; for a job that is not loaded it only reports so.
				_, _ = b.launchctl(ctx, d.UDID, "bootout", "system/"+j)
			}
			mu.Lock()
			changed = append(changed, j)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	if len(changed) > 0 {
		verb := "turned off"
		if !on {
			verb = "turned back on (they start at the next boot)"
		}
		log.Printf("ios lean %s: %d launchd jobs %s in %.0f ms", d.Name, len(changed), verb, ms(time.Since(start)))
	}
	b.mu.Lock()
	b.lean[d.UDID] = on
	b.mu.Unlock()
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
// 3 s of the device being ready, in some boots later, hence passes at 3, 8, 20 and 45 s.
func stopWallpaperGallery(udid, name string) {
	n := 0
	for _, wait := range []time.Duration{3 * time.Second, 5 * time.Second, 12 * time.Second, 25 * time.Second} {
		time.Sleep(wait)
		out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
		if err != nil {
			log.Printf("ios lean %s: ps: %v", name, err)
			return
		}
		for _, pid := range wallpaperExtensions(string(out), udid) {
			if syscall.Kill(pid, syscall.SIGTERM) == nil {
				n++
			}
		}
	}
	if n > 0 {
		log.Printf("ios lean %s: stopped %d wallpaper gallery extensions", name, n)
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
