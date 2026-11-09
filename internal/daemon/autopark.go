package daemon

// Auto-park (AGEMU_AUTO_PARK=<duration>, off by default): the daemon parks a device that has
// had no call for that long, and the next call resumes it as after `agemu park` (wake).
//
// Which devices: only those this daemon has served a call on since it started (a device booted
// by hand, or driven by another daemon or a person, is never parked by it), and only while
// they are booted and agemu's (Android: a managed AVD; iOS: a simulator named agemu-*),
// not booting, not in standby, not parked already.
//
// When: a call counts from its start to its end, so nothing is parked while a call on the
// device is in flight (a long wait, an install, a boot, a reset, a profile switch, an MCP
// session's pending call). Every call on the device counts, logs and crashes included: they
// do not resume a parked device (they read what was captured), but an agent that polls them
// while its app works in the background keeps the device running. Calls without a device
// (devices, profiles, create) do not count.
//
// Races: the parker marks the device (gate) before it parks it, and a call that arrives
// meanwhile waits for the park to finish (milliseconds) and then resumes the device through
// wake, so no call ever acts on a device that is being stopped. A device whose call count is
// not zero is never gated. Device ids are canonical (Service.canonical), so two spellings of
// one id share one count.
//
// A profile switch (set_profile) can act on another device than the one it names (iOS boots
// the simulator of the profile's device type, which may be one this daemon parked): while one
// is in flight the parker parks no device of that platform, and a switch waits for a park in
// progress on that platform to finish. Before the switch the daemon resumes the devices of that
// platform it parked itself (the target may be one of them), and the backend resumes a parked
// target before it boots it (an agent's park).
//
// Shutdown: Close resumes the devices the parker parked (not those an agent parked), so a
// daemon that exits leaves none of its own parks behind. A daemon killed with SIGKILL leaves
// them parked; the next daemon resumes them on the first call, as any parked device.

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// minAutoPark is the shortest idle time AGEMU_AUTO_PARK accepts: below it an agent's think
// time between two steps would park and resume the device over and over.
const minAutoPark = 10 * time.Second

// autoParkTimeout bounds one park and each resume at daemon shutdown.
const autoParkTimeout = 30 * time.Second

// parseAutoPark reads AGEMU_AUTO_PARK: empty, "0" or "off" is off; otherwise a Go duration
// ("10m", "90s") or a number of seconds, at least minAutoPark.
func parseAutoPark(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	switch v {
	case "", "0", "off", "no", "false":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.ParseFloat(v, 64)
		if nerr != nil {
			return 0, fmt.Errorf("AGEMU_AUTO_PARK=%q is not a duration (10m, 90s) or a number of seconds", v)
		}
		d = time.Duration(n * float64(time.Second))
	}
	if d < minAutoPark {
		return 0, fmt.Errorf("AGEMU_AUTO_PARK=%q is shorter than %v", v, minAutoPark)
	}
	return d, nil
}

// autoParkTick is how often the parker looks for idle devices: a park comes between after
// and after+tick of idleness (1-10 s; 3 s for one minute).
func autoParkTick(after time.Duration) time.Duration {
	t := after / 20
	if t < time.Second {
		t = time.Second
	}
	if t > 10*time.Second {
		t = 10 * time.Second
	}
	return t
}

// devActivity is one device's call activity (Service.act, under Service.actMu).
type devActivity struct {
	inflight int       // calls on the device that have started and not ended
	last     time.Time // when the latest call started or ended
	// gate is set while the parker parks the device and closed when it is done; a call that
	// starts meanwhile waits for it.
	gate chan struct{}
	// handled: the parker has dealt with this idle stretch (parked the device or found it not
	// parkable); the next call starts a new one.
	handled bool
	// auto: the parker parked the device and no call has resumed or parked it since.
	auto bool
}

// platformOf returns the platform prefix of a device id.
func platformOf(id string) api.Platform {
	p, _, _ := strings.Cut(id, ":")
	return api.Platform(p)
}

// autoParker is the running parker (Service.ap); nil when auto-park is off.
type autoParker struct {
	after, tick time.Duration
	stop        context.CancelFunc
	done        chan struct{}
}

// startAutoPark starts the parker; NewService calls it when AGEMU_AUTO_PARK is set.
func (s *Service) startAutoPark(after, tick time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.ap = &autoParker{after: after, tick: tick, stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.ap.done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.autoParkIdle(ctx)
		}
	}()
	log.Printf("auto-park: devices this daemon serves are parked after %v without a call (AGEMU_AUTO_PARK)", after)
}

// beginCall counts a call on a device (endCall ends it). It waits while the parker parks the
// device, so the call's wake sees it parked and resumes it.
func (s *Service) beginCall(ctx context.Context, id, method string) error {
	if s.ap == nil {
		return nil
	}
	switching := method == "set_profile"
	plat := platformOf(id)
	for {
		s.actMu.Lock()
		a := s.activity[id]
		if a == nil {
			a = &devActivity{}
			s.activity[id] = a
		}
		g := a.gate
		if g == nil && switching {
			// A switch may act on any device of its platform: none may be being parked.
			for other, oa := range s.activity {
				if oa.gate != nil && platformOf(other) == plat {
					g = oa.gate
					break
				}
			}
		}
		if g != nil {
			s.actMu.Unlock()
			select {
			case <-g:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if switching {
			s.switching[plat]++
		}
		a.inflight++
		a.last = time.Now()
		a.handled = false
		if method != "logs" && method != "crashes" {
			a.auto = false // the call resumes the device (wake) or parks it itself
		}
		s.actMu.Unlock()
		return nil
	}
}

// resumeAutoParked resumes, before a profile switch on the platform, every device of it the
// parker parked: the switch may boot any of them. Its caller already counts the switch, so no
// new park starts on the platform meanwhile.
func (s *Service) resumeAutoParked(ctx context.Context, plat api.Platform) error {
	if s.ap == nil {
		return nil
	}
	s.actMu.Lock()
	var ids []string
	for id, a := range s.activity {
		if a.auto && platformOf(id) == plat {
			ids = append(ids, id)
			a.auto = false
			a.last = time.Now()
			a.handled = false
		}
	}
	s.actMu.Unlock()
	for _, id := range ids {
		b, err := s.backendFor(id)
		pk, ok := b.(backend.Parker)
		if err != nil || !ok || !pk.Parked(id) {
			continue
		}
		l := s.deviceLock(id)
		l.Lock()
		_, err = pk.Resume(ctx, id)
		l.Unlock()
		if err != nil {
			return fmt.Errorf("resuming the auto-parked %s before the profile switch: %w", id, err)
		}
		log.Printf("auto-park %s: resumed before a profile switch", id)
	}
	return nil
}

func (s *Service) endCall(id, method string) {
	if s.ap == nil {
		return
	}
	s.actMu.Lock()
	if method == "set_profile" {
		s.switching[platformOf(id)]--
	}
	if a := s.activity[id]; a != nil {
		a.inflight--
		a.last = time.Now()
		a.handled = false
	}
	s.actMu.Unlock()
}

// touch counts a device a call returned (an iOS profile switch answers with another
// simulator) as used now, so that the parker serves it too.
func (s *Service) touch(id string) {
	if s.ap == nil || id == "" {
		return
	}
	s.actMu.Lock()
	a := s.activity[id]
	if a == nil {
		a = &devActivity{}
		s.activity[id] = a
	}
	a.last = time.Now()
	a.handled = false
	s.actMu.Unlock()
}

// autoParkIdle parks each device whose idle stretch reached the threshold.
func (s *Service) autoParkIdle(ctx context.Context) {
	now := time.Now()
	type cand struct {
		id   string
		last time.Time
	}
	var cands []cand
	s.actMu.Lock()
	for id, a := range s.activity {
		if a.inflight == 0 && a.gate == nil && !a.handled && now.Sub(a.last) >= s.ap.after {
			cands = append(cands, cand{id, a.last})
		}
	}
	s.actMu.Unlock()
	for _, c := range cands {
		if ctx.Err() != nil {
			return
		}
		s.autoParkOne(ctx, c.id, c.last)
	}
}

// autoParkOne parks one idle device if it may be parked (see the top of this file). last is
// the device's last activity when it was picked: a call since then cancels the park.
func (s *Service) autoParkOne(ctx context.Context, id string, last time.Time) {
	b, err := s.backendFor(id)
	pk, isParker := b.(backend.Parker)
	why := ""
	switch {
	case err != nil || !isParker:
		why = "its backend cannot park"
	case pk.Parked(id):
		why = "parked already"
	default:
		why = s.notParkable(ctx, b, id)
	}
	s.actMu.Lock()
	a := s.activity[id]
	if a == nil || a.inflight != 0 || a.gate != nil || !a.last.Equal(last) {
		s.actMu.Unlock()
		return // a call came meanwhile: a new idle stretch
	}
	if s.switching[platformOf(id)] > 0 {
		s.actMu.Unlock()
		return // a profile switch may be acting on it; look again on the next tick
	}
	a.handled = true
	if why == notListed {
		// An id no backend lists (a mistyped one, a deleted device): forget it rather than keep
		// a List per idle stretch for it. A later call on it adds it back.
		delete(s.activity, id)
	}
	if why != "" {
		s.actMu.Unlock()
		return
	}
	gate := make(chan struct{})
	a.gate = gate
	s.actMu.Unlock()

	parked, locked := false, false
	// Every call holds its count while it holds the device lock, so the lock is free here;
	// TryLock only guards against a holder outside a call.
	if l := s.deviceLock(id); l.TryLock() {
		locked = true
		pctx, cancel := context.WithTimeout(ctx, autoParkTimeout)
		start := time.Now()
		if err := pk.Park(pctx, id); err != nil {
			log.Printf("auto-park %s: %v", id, err)
		} else {
			parked = true
			log.Printf("auto-park %s: parked after %v without a call (park took %v)", id, start.Sub(last).Round(100*time.Millisecond), time.Since(start).Round(time.Millisecond))
		}
		cancel()
		l.Unlock()
	}

	s.actMu.Lock()
	a.gate = nil
	a.auto = parked
	if !locked {
		a.handled = false // the lock was held: try again on the next tick
	}
	s.actMu.Unlock()
	close(gate)
}

// notListed is notParkable's answer for an id its backend does not list.
const notListed = "it is not listed"

// notParkable returns why the parker must leave the device alone, or "" if it may park it:
// it must be listed by its backend as booted (not booting, standby or shut down) and be one of
// agemu's own devices.
func (s *Service) notParkable(ctx context.Context, b backend.Backend, id string) string {
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	devs, err := b.List(lctx)
	if err != nil {
		return "listing failed: " + err.Error()
	}
	for _, d := range devs {
		if d.ID != id {
			continue
		}
		if d.State != "booted" {
			return "it is " + d.State
		}
		// Android lists only managed AVDs; iOS lists every simulator with
		// AGEMU_IOS_ALL_DEVICES, and acts on any with AGEMU_IOS_ALLOW_FOREIGN.
		if d.Platform == api.IOS && !strings.HasPrefix(d.Name, "agemu-") {
			return "it is not an agemu simulator"
		}
		return ""
	}
	return notListed
}

// stopAutoPark stops the parker and resumes the devices it parked, in parallel, each within
// autoParkTimeout. Close calls it before the backends close.
func (s *Service) stopAutoPark() {
	if s.ap == nil {
		return
	}
	s.ap.stop()
	<-s.ap.done
	s.actMu.Lock()
	var ids []string
	for id, a := range s.activity {
		if a.auto {
			ids = append(ids, id)
			a.auto = false
		}
	}
	s.actMu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		b, err := s.backendFor(id)
		pk, ok := b.(backend.Parker)
		if err != nil || !ok || !pk.Parked(id) {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), autoParkTimeout)
			defer cancel()
			l := s.deviceLock(id)
			l.Lock()
			defer l.Unlock()
			if _, err := pk.Resume(ctx, id); err != nil {
				log.Printf("auto-park %s: resuming at daemon shutdown: %v", id, err)
				return
			}
			log.Printf("auto-park %s: resumed at daemon shutdown", id)
		}(id)
	}
	wg.Wait()
}

// autoParkFromEnv reads AGEMU_AUTO_PARK for NewService; a bad value is logged and leaves
// auto-park off.
func autoParkFromEnv() time.Duration {
	d, err := parseAutoPark(os.Getenv("AGEMU_AUTO_PARK"))
	if err != nil {
		log.Printf("auto-park off: %v", err)
		return 0
	}
	return d
}
