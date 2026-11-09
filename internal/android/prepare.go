package android

// Asynchronous baseline preparation. A boot that has to take the baseline snapshot (a new AVD, a
// retake after a GPU mode, AVD configuration or agent change, a missing baseline) waits until the
// guest has settled (quiet.go), which takes minutes. The boot runs in a goroutine that holds the
// device's lifecycle lock; Boot waits for it, or with BootOptions.NoWait returns as soon as the
// boot turns out to need a baseline (or after noWaitBound) with the device in
// api.StatePreparing. Meanwhile the daemon answers other calls on the device at once with an
// *api.PreparingError (backend.Preparer), `devices` shows the phase and the time so far, a second
// boot joins the one in progress, and shutdown cancels it.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// noWaitBound is the longest a NoWait boot waits before it returns: a boot from an existing
// baseline is done in 2-6 s, and one that needs a baseline knows it within seconds (no
// baseline) or after a failed load (a retake).
var noWaitBound = 40 * time.Second

// Preparation phases, as `devices` shows them.
const (
	phaseStarting = "starting the emulator"
	phaseColdBoot = "cold boot"
	phaseSteps    = "baseline steps"
	phaseSettling = "settling"
	phaseSaving   = "saving"
)

// prepState is one boot in progress.
type prepState struct {
	start  time.Time
	cancel context.CancelFunc
	done   chan struct{} // closed when the boot has finished; dev and err are set then
	needed chan struct{} // closed when the boot turns out to take a baseline
	once   sync.Once
	// visible: the device shows as preparing (a baseline is being taken, or a NoWait caller
	// returned before the boot finished). Calls on it then fail at once.
	visible atomic.Bool
	phase   atomic.Value // string
	dev     api.Device
	err     error
}

func newPrep(cancel context.CancelFunc) *prepState {
	p := &prepState{start: time.Now(), cancel: cancel, done: make(chan struct{}), needed: make(chan struct{})}
	p.phase.Store(phaseStarting)
	return p
}

// markNeeded records that this boot takes a baseline: it shows as preparing from now on.
func (p *prepState) markNeeded() {
	p.visible.Store(true)
	p.once.Do(func() { close(p.needed) })
}

func (p *prepState) setPhase(s string) { p.phase.Store(s) }

func (p *prepState) info() *api.Preparing {
	return &api.Preparing{
		Phase:    p.phase.Load().(string),
		ElapsedS: float64(time.Since(p.start).Round(time.Second) / time.Second),
		MaxS:     prepMax().Seconds(),
		Hint:     "the device's first boot (or a retake) builds its baseline snapshot once; poll `devices` until it is booted, or call `boot` again",
	}
}

// Bounds of a boot. Everything before the snapshot save runs under prepLimit (the emulator start,
// the cold boot, the baseline steps, each with its own deadline, and the settle wait, which saves
// at the latest at 12 minutes of guest uptime); the save then has saveTimeout and is not
// cancelled. prepMax, their sum, is a hard bound: a boot that reaches it fails (no baseline is
// saved, the next boot prepares again). Typical first boots took 430-455 s.
const (
	stepTimeout = 30 * time.Second // one guest call in the baseline steps
	saveTimeout = 3 * time.Minute  // the snapshot save (3-7 s measured)
	prepLimit   = 17 * time.Minute // everything before the save
)

// prepMax is the hard bound of a preparation, shown in `devices` and errors.
func prepMax() time.Duration { return prepLimit + saveTimeout }

// setPhase records the current phase of the device's boot, if one is running.
func (d *device) setPhase(s string) {
	d.mu.Lock()
	p := d.prep
	d.mu.Unlock()
	if p != nil {
		p.setPhase(s)
	}
}

// markNeeded records that the device's running boot takes a baseline.
func (d *device) markNeeded() {
	d.mu.Lock()
	p := d.prep
	d.mu.Unlock()
	if p != nil {
		p.markNeeded()
	}
}

// preparing returns the visible preparation of d, or nil.
func (d *device) preparing() *prepState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.prep != nil && d.prep.visible.Load() {
		return d.prep
	}
	return nil
}

// Preparing implements backend.Preparer.
func (b *Backend) Preparing(id string) *api.Preparing {
	name, err := avdName(id)
	if err != nil {
		return nil
	}
	b.mu.Lock()
	d := b.devs[name]
	b.mu.Unlock()
	if d == nil {
		return nil
	}
	if p := d.preparing(); p != nil {
		return p.info()
	}
	return nil
}

// AwaitDecision implements backend.Preparer: for a boot in progress that has not yet shown
// whether it prepares a baseline, it waits until it has (or finished, or noWaitBound passed, which
// marks it visible as a NoWait boot does), then reports the preparation, if any.
func (b *Backend) AwaitDecision(ctx context.Context, id string) *api.Preparing {
	name, err := avdName(id)
	if err != nil {
		return nil
	}
	b.mu.Lock()
	d := b.devs[name]
	b.mu.Unlock()
	if d == nil {
		return nil
	}
	d.mu.Lock()
	p := d.prep
	d.mu.Unlock()
	if p == nil {
		return nil
	}
	if !p.visible.Load() {
		t := time.NewTimer(noWaitBound)
		defer t.Stop()
		select {
		case <-p.needed:
		case <-p.done:
			return nil
		case <-t.C:
			p.visible.Store(true)
		case <-ctx.Done():
			return nil
		}
	}
	if d.preparing() == p {
		return p.info()
	}
	return nil
}

// Boot boots the device (see bootSync). A boot already in progress is joined.
func (b *Backend) Boot(ctx context.Context, id string, opts api.BootOptions) (api.Device, error) {
	d, err := b.devFor(id)
	if err != nil {
		return api.Device{}, err
	}
	d.mu.Lock()
	p := d.prep
	if p == nil {
		pctx, cancel := context.WithCancel(b.background())
		p = newPrep(cancel)
		d.prep = p
		d.prepErr = ""
		d.mu.Unlock()
		go b.runBoot(pctx, d, id, opts, p)
	} else {
		d.mu.Unlock()
	}
	return b.awaitBoot(ctx, id, p, opts.NoWait)
}

func (b *Backend) runBoot(ctx context.Context, d *device, id string, opts api.BootOptions, p *prepState) {
	ctx, cancelLimit := context.WithTimeout(ctx, prepLimit)
	defer cancelLimit()
	boot := b.bootSync
	if b.bootHook != nil {
		boot = b.bootHook
	}
	dev, err := boot(ctx, d, id, opts)
	p.cancel()
	d.mu.Lock()
	p.dev, p.err = dev, err
	if d.prep == p {
		d.prep = nil
	}
	if err != nil && p.visible.Load() {
		d.prepErr = err.Error()
	}
	d.mu.Unlock()
	if err != nil && p.visible.Load() {
		logf("%s: preparing the baseline failed after %v: %v", d.name, time.Since(p.start).Round(time.Second), err)
	}
	close(p.done)
}

// awaitBoot waits for the boot p: until it is done, or with noWait until it turns out to take a
// baseline or noWaitBound passes, whichever comes first.
func (b *Backend) awaitBoot(ctx context.Context, id string, p *prepState, noWait bool) (api.Device, error) {
	if !noWait {
		select {
		case <-p.done:
			return p.dev, p.err
		case <-ctx.Done():
			return api.Device{}, ctx.Err()
		}
	}
	t := time.NewTimer(noWaitBound)
	defer t.Stop()
	select {
	case <-p.done:
		return p.dev, p.err
	case <-p.needed:
	case <-t.C:
		p.visible.Store(true)
	case <-ctx.Done():
		return api.Device{}, ctx.Err()
	}
	select {
	case <-p.done: // it finished meanwhile
		return p.dev, p.err
	default:
	}
	dev, err := b.device(id)
	if err != nil || dev.State != api.StatePreparing {
		// The AVD could not be read, or the boot finished between the two checks.
		dev = api.Device{ID: id, Name: dev.Name, Platform: api.Android, State: api.StatePreparing, Preparing: p.info()}
		if dev.Name == "" {
			dev.Name, _ = avdName(id)
		}
	}
	return dev, nil
}

// cancelPrep cancels the device's boot in progress, if any, and waits for it to stop (a save in
// progress is let finish: an interrupted save could leave a broken snapshot).
func (d *device) cancelPrep(why string) error {
	d.mu.Lock()
	p := d.prep
	d.mu.Unlock()
	if p == nil {
		return nil
	}
	logf("%s: %s: cancelling the boot in progress (%s, %v so far)", d.name, why, p.phase.Load(), time.Since(p.start).Round(time.Second))
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(2 * time.Minute):
		return errPrepStuck
	}
	d.mu.Lock()
	d.prepErr = "cancelled by " + why
	d.retook = false // a later boot may retake again
	d.mu.Unlock()
	return nil
}

type prepStuckError struct{}

func (prepStuckError) Error() string {
	return "the boot in progress did not stop within 2 minutes of being cancelled"
}

var errPrepStuck error = prepStuckError{}

// sleepCtx sleeps for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
