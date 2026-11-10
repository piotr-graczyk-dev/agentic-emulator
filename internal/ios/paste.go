//go:build !windows

package ios

// Confirming a paste (docs/IOS.md "type").
//
// `type` pastes: it puts the text on the device pasteboard and presses Cmd-V. A paste can be
// late (the app's main thread is busy, or the app's own read of the pasteboard is slow on a
// loaded host) or lost (Cmd-V came while the field was still becoming focused and UIKit dropped
// it). Pasting again repairs a lost paste and doubles a late one, so agemu pastes again only
// when it can tell that no app read the paste before (at most pasteMaxTries Cmd-Vs).
//
// The signal comes from the device's pasteboard daemon, pasted. It logs "KeyCommand" each time
// an app reads a pasteboard item for a paste that a key command started, and agemu's log
// stream (logs.go) delivers one process's lines in the order it logged them. A paste counts as
// lost only when, after Cmd-V:
//   - no KeyCommand read was logged and the focused field did not change,
//   - the app's main thread answered two UI tree reads (the tree is read on it: a read waits
//     while the main thread is blocked, so the thread was free to handle the key) and at
//     least pasteMinWindow passed, and
//   - pasted logged a line at least pasteMinMargin, or as long as the last of those reads
//     took, after that read returned: every KeyCommand logged before it has been delivered.
//     agemu makes pasted log one by reading the pasteboard through the helper.
// Every one of these waits grows with the load: the tree reads take longer and the stream falls
// behind. A field that cannot be read (a secure field, a web view) is done once the read is in
// the log; earlier builds never confirmed such a paste and never repeated it. A readable field
// that an app read the paste for but that has not changed pasteReadWait later is pasted once
// more: an app can read the pasteboard for Cmd-V and still drop the paste.
//
// Without a running log capture (AGEMU_IOS_LOG_CAPTURE=0, or nothing started it yet) the
// fixed window of earlier builds decides: the field is read again at once and 150 ms later.
// Until pasted has logged one KeyCommand on the device, a paste with no read in the log is
// treated as the fixed window treats an unchanged field, after the longer wait: an iOS whose
// pasted logs no such line would otherwise make every paste look lost.

import (
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	// pasteMinWindow: a paste is never called lost sooner after Cmd-V.
	pasteMinWindow = 150 * time.Millisecond
	// pasteMinMargin is the least time between the last tree read and the pasted line that
	// proves no read was logged: an app's item read runs on a background thread, after the
	// main thread handled Cmd-V.
	pasteMinMargin = 100 * time.Millisecond
	// pasteReadWait bounds how long a readable field may take to change once an app has read
	// the pasteboard for the key. After it the paste is repeated once: an app dropped a paste it
	// had read (or rejects pastes, and drops the second one too).
	pasteReadWait = 2 * time.Second
	// pasteLogWait bounds how long agemu waits for pasted's lines to catch up. A stream that
	// lags more than this decides nothing: the paste is not repeated.
	pasteLogWait = 3 * time.Second
	// pastePoll is the pause between the field reads while waiting.
	pastePoll = 20 * time.Millisecond
	// pasteMaxTries bounds the Cmd-Vs of one type call when pasted's log decides (each after
	// the one before was known to be lost).
	pasteMaxTries = 3
	// fixedRecheck is the fallback window's second read.
	fixedRecheck = 150 * time.Millisecond
)

// pasteObserver is what the device's log capture saw of pasted.
type pasteObserver interface {
	// keyRead reports whether pasted logged an app's read for a key command at or after t.
	keyRead(t time.Time) bool
	// caughtUp reports whether a pasted line logged at or after t has been delivered, so every
	// pasted line logged before t has been delivered too.
	caughtUp(t time.Time) bool
	// poke makes pasted log a line soon; it does not wait for it.
	poke()
	// proven reports whether this device's pasted has logged a KeyCommand read since agemu
	// started watching it. Until then the absence of one proves nothing (an iOS whose pasted
	// does not log it would make every paste look lost), and the fixed window's rules apply.
	proven() bool
}

// pasteOutcome is what confirm found out about one Cmd-V.
type pasteOutcome int

const (
	pasteChanged pasteOutcome = iota // the focused field changed: the paste arrived
	pasteRead                        // an app read the pasteboard for the key; the field cannot be read
	pasteUnshown                     // an app read it, but the readable field did not change within pasteReadWait
	pasteUnknown                     // cannot tell: the field went away, or pasted's lines did not catch up
	pasteLost                        // no app read it: pasting again cannot double it
)

func (o pasteOutcome) String() string {
	return [...]string{"changed", "read", "read, not shown", "unknown", "lost"}[o]
}

// fieldState is a reading of the focused text field (ok=false: none that reads back as text).
type fieldState struct {
	value string
	ok    bool
}

// paster pastes, confirms the paste, and pastes again only for a lost paste.
type paster struct {
	set      func() error          // puts the text on the device pasteboard
	key      func() error          // presses Cmd-V
	read     func() (string, bool) // the focused field's text; ok=false: no field that reads back as text
	obs      pasteObserver         // nil: no log capture, the fixed window decides
	retrying func(why string)      // told before each repeated paste
	now      func() time.Time
	sleep    func(time.Duration)

	// What the last confirm found, for the caller's log.
	outcome pasteOutcome
	why     string
	pastes  int
}

// describe says why a paste was not confirmed, for the daemon log.
func (p *paster) describe() string {
	switch {
	case p.pastes > 1:
		return fmt.Sprintf("pasted %d times; after the last paste: %s", p.pastes, p.outcome)
	case p.outcome == pasteRead:
		return "an app read the pasteboard for the key; the focused field cannot be read"
	case p.why != "":
		return p.why
	}
	return p.outcome.String()
}

// run pastes and reports whether the focused field was seen to change after a single paste:
// only then has the app certainly read the pasteboard, so it may be put back. The field is read
// before the paste, while the pasteboard is set, so a lost paste can be told apart from one the
// field transformed (a formatted phone number, upper-casing). That read finishes before Cmd-V
// goes out, or a late read could see the pasted text and make the paste look lost.
func (p *paster) run() (confirmed bool, err error) {
	before := make(chan fieldState, 1)
	go func() {
		v, ok := p.read()
		before <- fieldState{v, ok}
	}()
	if err := p.set(); err != nil {
		<-before
		return false, err
	}
	pre := <-before
	unshownAgain := false
	for attempt := 1; ; attempt++ {
		t0 := p.now()
		if err := p.key(); err != nil {
			return false, err
		}
		out, why := p.confirm(t0, pre)
		p.outcome, p.why, p.pastes = out, why, attempt
		// The fixed window cannot tell a late paste from a lost one, so it pastes twice at
		// most. With pasted's log a paste is repeated only after the one before was known to
		// be lost, which a field that refuses pastes for a while after it got focus can need
		// more than once.
		maxPastes := 2
		if p.obs != nil && p.obs.proven() {
			maxPastes = pasteMaxTries
		}
		if out == pasteLost && attempt < maxPastes {
			p.retrying(why)
			continue
		}
		// An app can read the pasteboard for Cmd-V and still drop the paste (seen once in 100
		// pastes 0.1 s after the field got focus). A readable field that shows nothing within
		// pasteReadWait of the read is pasted once more; a paste later than that would double.
		if out == pasteUnshown && !unshownAgain && attempt < maxPastes {
			unshownAgain = true
			p.retrying(why)
			continue
		}
		// After a repeated paste the pasteboard stays as typed: if an earlier one was read
		// after all, a restore could still race its read.
		return out == pasteChanged && attempt == 1, nil
	}
}

// confirm waits until the paste sent at t0 is shown, read, or known to be lost.
func (p *paster) confirm(t0 time.Time, pre fieldState) (pasteOutcome, string) {
	if p.obs == nil {
		return p.confirmFixed(pre)
	}
	if !pre.ok && p.awaitKeyRead(t0, t0.Add(pasteMinWindow)) {
		// Nothing to see in the field: the read ends the wait without a tree read.
		return pasteRead, ""
	}
	var readAt time.Time // when the KeyCommand read was seen (a readable field)
	reads := 0
	for {
		start := p.now()
		v, ok := p.read()
		end := p.now()
		reads++
		if pre.ok {
			if !ok {
				return pasteUnknown, "the focused field went away"
			}
			if v != pre.value {
				return pasteChanged, ""
			}
		}
		if !readAt.IsZero() {
			// An app read it: wait for the field to show it.
			if end.Sub(readAt) >= pasteReadWait {
				return pasteUnshown, "an app read the pasteboard for it, but the focused field did not change within 2 s"
			}
			p.sleep(pastePoll)
			continue
		}
		if reads >= 2 && end.Sub(t0) >= pasteMinWindow {
			margin := max(pasteMinMargin, end.Sub(start))
			switch p.awaitLog(t0, end.Add(margin)) {
			case pasteRead:
				if !pre.ok {
					return pasteRead, ""
				}
				readAt = p.now()
				continue
			case pasteLost:
				if !p.obs.proven() {
					// No key read seen on this device yet: decide as the fixed window does,
					// after a longer wait (a readable field that did not change is pasted
					// again; any other is not).
					if !pre.ok {
						return pasteUnknown, "no readable text field, and pasted has not logged a key read on this device yet"
					}
					return pasteLost, "the focused field did not change, and pasted has not logged a key read on this device yet"
				}
				return pasteLost, "no app read the pasteboard for it"
			default:
				return pasteUnknown, "the device log did not catch up"
			}
		}
		p.sleep(pastePoll)
	}
}

// awaitKeyRead reports whether a KeyCommand read since t0 is delivered by until (checked at
// least once).
func (p *paster) awaitKeyRead(t0, until time.Time) bool {
	for {
		if p.obs.keyRead(t0) {
			return true
		}
		if !p.now().Before(until) {
			return false
		}
		p.sleep(5 * time.Millisecond)
	}
}

// awaitLog waits until pasted has logged a line at or after target, making it log one once
// target has passed. It returns pasteRead when a KeyCommand read since t0 was delivered first,
// pasteLost when the line came without one, and pasteUnknown after pasteLogWait.
func (p *paster) awaitLog(t0, target time.Time) pasteOutcome {
	deadline := target.Add(pasteLogWait)
	poked := false
	for {
		if p.obs.keyRead(t0) {
			return pasteRead
		}
		if p.obs.caughtUp(target) {
			// The read may have been delivered together with the line that caught up.
			if p.obs.keyRead(t0) {
				return pasteRead
			}
			return pasteLost
		}
		now := p.now()
		if now.After(deadline) {
			return pasteUnknown
		}
		if !poked && !now.Before(target) {
			p.obs.poke()
			poked = true
		}
		p.sleep(5 * time.Millisecond)
	}
}

// confirmFixed is the window without a log capture: the field is read at once and 150 ms
// later; a field that does not read back as text is not confirmed and not pasted again.
func (p *paster) confirmFixed(pre fieldState) (pasteOutcome, string) {
	if !pre.ok {
		return pasteUnknown, "no readable text field"
	}
	for _, pause := range []time.Duration{0, fixedRecheck} {
		p.sleep(pause)
		v, ok := p.read()
		if !ok {
			return pasteUnknown, "the focused field went away"
		}
		if v != pre.value {
			return pasteChanged, ""
		}
	}
	return pasteLost, "the focused field did not change within 150 ms (no device log to tell a late paste)"
}

// pasteRestoring saves the pasteboard, pastes, and puts the pasteboard back once the paste is
// confirmed: the focused field changed, so the app has read the pasteboard. An unconfirmed paste
// (a secure field, a web view or custom input that does not read back as text, or a paste that
// had to be sent twice) keeps the typed text on the pasteboard (keep), because the app may still
// be about to read it, and a restore that won that race would make the app paste the old
// content. A failed paste restores it.
func pasteRestoring(save func() error, paste func() (bool, error), restore func() error, keep func()) error {
	if err := save(); err != nil {
		return fmt.Errorf("saving the pasteboard before typing: %w", err)
	}
	confirmed, err := paste()
	if err == nil && !confirmed {
		keep()
		return nil
	}
	if rerr := restore(); rerr != nil {
		if err == nil {
			err = fmt.Errorf("typed, but restoring the pasteboard failed: %w", rerr)
		}
	}
	return err
}

// maxPasteReads bounds the KeyCommand times a device keeps.
const maxPasteReads = 16

// pasteLog is what a device's log stream delivered from pasted (guarded by deviceLogs.mu).
type pasteLog struct {
	last  time.Time   // log time of the newest pasted line
	reads []time.Time // log times of pasted's KeyCommand lines, newest last
	seen  bool        // a KeyCommand line was ever delivered
}

// isPastedLine reports whether a stream line comes from the system's pasteboard daemon: a
// process named pasted outside any app bundle (bundle is the line's bundleKey), so an app
// cannot speak for it.
func isPastedLine(ev *streamEvent, bundle string) bool {
	return bundle == "" && path.Base(ev.ProcessImagePath) == "pasted" &&
		!strings.Contains(ev.ProcessImagePath, "/Containers/Bundle/Application/")
}

// add records one pasted line logged at t.
func (pl *pasteLog) add(t time.Time, msg string) {
	if t.After(pl.last) {
		pl.last = t
	}
	if strings.TrimSpace(msg) == "KeyCommand" {
		pl.seen = true
		pl.reads = append(pl.reads, t)
		if n := len(pl.reads); n > maxPasteReads {
			pl.reads = append(pl.reads[:0], pl.reads[n-maxPasteReads:]...)
		}
	}
}

// readSince reports whether a KeyCommand read was logged at or after t.
func (pl *pasteLog) readSince(t time.Time) bool {
	for i := len(pl.reads) - 1; i >= 0; i-- {
		if !pl.reads[i].Before(t) {
			return true
		}
	}
	return false
}

// logPasteObserver reads one device's pasted lines from the stream that was running when it
// was made; once that stream is replaced or stopped it never reports caught up.
type logPasteObserver struct {
	dl    *deviceLogs
	s     *logStream
	pokeF func()
}

func (o *logPasteObserver) keyRead(t time.Time) bool {
	o.dl.mu.Lock()
	defer o.dl.mu.Unlock()
	return o.dl.paste.readSince(t)
}

func (o *logPasteObserver) caughtUp(t time.Time) bool {
	o.dl.mu.Lock()
	defer o.dl.mu.Unlock()
	return o.dl.stream == o.s && !o.dl.paste.last.Before(t)
}

func (o *logPasteObserver) poke() { o.pokeF() }

func (o *logPasteObserver) proven() bool {
	o.dl.mu.Lock()
	defer o.dl.mu.Unlock()
	return o.dl.paste.seen
}

// pasteObserverFor returns the device's pasted observer, or nil when its log stream is not
// running or has not delivered a line yet.
func (b *Backend) pasteObserverFor(udid string, poke func()) pasteObserver {
	b.mu.Lock()
	dl := b.logs[udid]
	b.mu.Unlock()
	if dl == nil {
		return nil
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	s := dl.stream
	if s == nil || dl.firstLine.IsZero() {
		return nil
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	return &logPasteObserver{dl: dl, s: s, pokeF: poke}
}
