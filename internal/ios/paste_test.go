package ios

import (
	"sync"
	"testing"
	"time"
)

// pasteSim simulates a device for the paster on a virtual clock: a focused field, the app's
// handling of each Cmd-V (lost, or read by the app after readDelay and shown in the field after
// showDelay), pasted's log as the stream delivers it (logLag behind), and how long a tree read
// takes.
type pasteSim struct {
	mu  sync.Mutex
	now time.Time

	value    string // the field's text
	readable bool   // the field reads back as text (false: a secure field, a web view)
	text     string // what one paste inserts

	lose      int           // this many Cmd-Vs are dropped (no app reads them)
	readDrop  int           // this many Cmd-Vs are read by the app and then dropped
	readDelay time.Duration // Cmd-V to the app's KeyCommand read
	showDelay time.Duration // Cmd-V to the field showing the text
	logLag    time.Duration // a pasted line is delivered this long after it was logged
	treeRead  time.Duration // one focused-field read
	noLog     bool          // the stream delivers nothing more (stalled)
	unproven  bool          // pasted has not logged a KeyCommand on this device yet

	pastes  int
	reads   int // focused-field (tree) reads
	pending []pendingPaste
	logged  []loggedLine // pasted's lines: log time and whether it is a KeyCommand read
	pokes   []time.Time
	retries []string
}

type pendingPaste struct{ shownAt time.Time }

type loggedLine struct {
	at  time.Time
	key bool
}

func newPasteSim() *pasteSim {
	return &pasteSim{
		now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), readable: true, text: "abc",
		readDelay: 5 * time.Millisecond, showDelay: 10 * time.Millisecond,
		logLag: 15 * time.Millisecond, treeRead: 30 * time.Millisecond,
	}
}

func (s *pasteSim) advance(d time.Duration) {
	s.now = s.now.Add(d)
	keep := s.pending[:0]
	for _, p := range s.pending {
		if !p.shownAt.After(s.now) {
			s.value += s.text
		} else {
			keep = append(keep, p)
		}
	}
	s.pending = keep
}

func (s *pasteSim) paster() *paster {
	return &paster{
		set: func() error { return nil },
		key: func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.pastes++
			if s.lose > 0 {
				s.lose--
				return nil
			}
			s.logged = append(s.logged, loggedLine{at: s.now.Add(s.readDelay), key: true})
			if s.readDrop > 0 {
				s.readDrop--
				return nil
			}
			s.pending = append(s.pending, pendingPaste{shownAt: s.now.Add(s.showDelay)})
			return nil
		},
		read: func() (string, bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.reads++
			s.advance(s.treeRead)
			return s.value, s.readable
		},
		obs:      simObserver{s},
		retrying: func(why string) { s.retries = append(s.retries, why) },
		now: func() time.Time {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.now
		},
		sleep: func(d time.Duration) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.advance(d)
		},
	}
}

// simObserver is the stream's view of pasted: a line counts once now >= its time + logLag.
type simObserver struct{ s *pasteSim }

func (o simObserver) delivered(l loggedLine) bool {
	return !o.s.noLog && !l.at.Add(o.s.logLag).After(o.s.now)
}

func (o simObserver) keyRead(t time.Time) bool {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	for _, l := range o.s.logged {
		if l.key && !l.at.Before(t) && o.delivered(l) {
			return true
		}
	}
	return false
}

func (o simObserver) caughtUp(t time.Time) bool {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	for _, l := range o.s.logged {
		if !l.at.Before(t) && o.delivered(l) {
			return true
		}
	}
	return false
}

func (o simObserver) proven() bool {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	return !o.s.unproven
}

func (o simObserver) poke() {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	o.s.pokes = append(o.s.pokes, o.s.now)
	o.s.logged = append(o.s.logged, loggedLine{at: o.s.now})
}

func TestPasteConfirmedByTheField(t *testing.T) {
	s := newPasteSim()
	confirmed, err := s.paster().run()
	if err != nil || !confirmed || s.pastes != 1 || s.value != "abc" {
		t.Fatalf("confirmed=%v err=%v pastes=%d value=%q, want one confirmed paste", confirmed, err, s.pastes, s.value)
	}
	if len(s.pokes) != 0 {
		t.Fatalf("poked pasted %d times for a paste the field showed at once", len(s.pokes))
	}
}

func TestPasteWaitsForTheBaselineBeforePasting(t *testing.T) {
	// The baseline read is slower than setting the pasteboard. If the paste went out before
	// the baseline was taken, the baseline would already hold the pasted text, the field would
	// look unchanged, and the text would be pasted twice.
	s := newPasteSim()
	p := s.paster()
	read := p.read
	first := true
	p.read = func() (string, bool) {
		if first {
			first = false
			time.Sleep(50 * time.Millisecond) // real time: the set below finishes first
		}
		return read()
	}
	confirmed, err := p.run()
	if err != nil || !confirmed || s.pastes != 1 || s.value != "abc" {
		t.Fatalf("confirmed=%v err=%v pastes=%d value=%q, want one confirmed paste", confirmed, err, s.pastes, s.value)
	}
}

func TestLatePasteThatWasReadIsNotPastedAgain(t *testing.T) {
	// The app read the pasteboard at once, but the field shows the text only after 900 ms (a
	// slow paste pipeline, the r4 e2e's double paste in Settings at load 203). The fixed window
	// pasted again here.
	s := newPasteSim()
	s.showDelay = 900 * time.Millisecond
	confirmed, err := s.paster().run()
	if err != nil || s.pastes != 1 || s.value != "abc" || len(s.retries) != 0 {
		t.Fatalf("err=%v pastes=%d value=%q retries=%v, want one paste", err, s.pastes, s.value, s.retries)
	}
	if !confirmed {
		t.Fatal("the field changed after one paste: confirmed")
	}
}

func TestLateReadBehindASlowLogIsNotPastedAgain(t *testing.T) {
	// The read and the field are both late and the stream lags: the decision waits until
	// pasted's line after the margin is delivered, and the read is delivered before it.
	s := newPasteSim()
	s.readDelay, s.showDelay, s.logLag = 400*time.Millisecond, 1500*time.Millisecond, 700*time.Millisecond
	s.treeRead = 250 * time.Millisecond
	_, err := s.paster().run()
	if err != nil || s.pastes != 1 || len(s.retries) != 0 {
		t.Fatalf("err=%v pastes=%d retries=%v, want one paste", err, s.pastes, s.retries)
	}
}

func TestLostPasteIsPastedAgainOnce(t *testing.T) {
	s := newPasteSim()
	s.lose = 1
	confirmed, err := s.paster().run()
	if err != nil || s.pastes != 2 || s.value != "abc" || len(s.retries) != 1 {
		t.Fatalf("err=%v pastes=%d value=%q retries=%v, want a second paste", err, s.pastes, s.value, s.retries)
	}
	if confirmed {
		t.Fatal("a second paste is not confirmed (the pasteboard must not be restored under it)")
	}
	if len(s.pokes) == 0 {
		t.Fatal("the lost decision must wait for a pasted line after the margin")
	}

	// Lost twice (a field that refuses pastes for a while): a third paste lands.
	s = newPasteSim()
	s.lose = 2
	if _, err := s.paster().run(); err != nil || s.pastes != 3 || s.value != "abc" {
		t.Fatalf("err=%v pastes=%d value=%q, want the third paste to land", err, s.pastes, s.value)
	}
	// Never read (no field takes it): pasteMaxTries pastes, then give up.
	s = newPasteSim()
	s.lose = 10
	if _, err := s.paster().run(); err != nil || s.pastes != pasteMaxTries {
		t.Fatalf("err=%v pastes=%d, want %d pastes at most", err, s.pastes, pasteMaxTries)
	}
}

func TestLostDecisionWaitsForTheMainThreadAndTheMargin(t *testing.T) {
	// Instant tree reads: the decision still waits pasteMinWindow after Cmd-V and pokes pasted
	// only once the margin after the last read has passed.
	s := newPasteSim()
	s.lose = 1
	s.treeRead = 0
	p := s.paster()
	var keyAt []time.Time
	key := p.key
	p.key = func() error {
		keyAt = append(keyAt, p.now())
		return key()
	}
	if _, err := p.run(); err != nil {
		t.Fatal(err)
	}
	if len(keyAt) != 2 || len(s.pokes) == 0 {
		t.Fatalf("keys=%d pokes=%d", len(keyAt), len(s.pokes))
	}
	if gap := s.pokes[0].Sub(keyAt[0]); gap < pasteMinWindow+pasteMinMargin {
		t.Fatalf("poked %v after Cmd-V, want at least %v", gap, pasteMinWindow+pasteMinMargin)
	}
	if gap := keyAt[1].Sub(keyAt[0]); gap < pasteMinWindow+pasteMinMargin {
		t.Fatalf("pasted again %v after the first Cmd-V, want at least %v", gap, pasteMinWindow+pasteMinMargin)
	}
}

// racingObserver delivers the KeyCommand read in the same batch as the line that catches up:
// the first keyRead misses it, caughtUp then sees the batch.
type racingObserver struct{ calls int }

func (o *racingObserver) keyRead(time.Time) bool {
	o.calls++
	return o.calls > 1
}
func (o *racingObserver) caughtUp(time.Time) bool { return true }
func (o *racingObserver) poke()                   {}
func (o *racingObserver) proven() bool            { return true }

func TestReadDeliveredWithTheCatchUpLineIsARead(t *testing.T) {
	s := newPasteSim()
	p := s.paster()
	p.obs = &racingObserver{}
	if out := p.awaitLog(s.now, s.now); out != pasteRead {
		t.Fatalf("awaitLog = %v, want read", out)
	}
}

func TestStalledLogDecidesNothing(t *testing.T) {
	s := newPasteSim()
	s.lose = 1
	s.noLog = true
	confirmed, err := s.paster().run()
	if err != nil || confirmed || s.pastes != 1 || len(s.retries) != 0 {
		t.Fatalf("confirmed=%v err=%v pastes=%d retries=%v, want one unconfirmed paste", confirmed, err, s.pastes, s.retries)
	}
}

func TestUnprovenLogFollowsTheFixedRules(t *testing.T) {
	// Before pasted has logged a KeyCommand on the device, no read in the log proves nothing:
	// an unreadable field is not pasted again, a readable one at most once more.
	s := newPasteSim()
	s.unproven, s.readable, s.lose = true, false, 1
	if confirmed, err := s.paster().run(); err != nil || confirmed || s.pastes != 1 {
		t.Fatalf("unreadable: confirmed=%v err=%v pastes=%d, want one paste", confirmed, err, s.pastes)
	}
	s = newPasteSim()
	s.unproven, s.lose = true, 2
	if _, err := s.paster().run(); err != nil || s.pastes != 2 {
		t.Fatalf("readable: err=%v pastes=%d, want two pastes at most", err, s.pastes)
	}
}

func TestUnreadableFieldIsConfirmedByTheReadOnly(t *testing.T) {
	// A secure field or a web view: a read ends the wait, unconfirmed (the pasteboard keeps the
	// typed text); a lost paste is now pasted again.
	s := newPasteSim()
	s.readable = false
	confirmed, err := s.paster().run()
	if err != nil || confirmed || s.pastes != 1 || len(s.retries) != 0 {
		t.Fatalf("read: confirmed=%v err=%v pastes=%d retries=%v, want one unconfirmed paste", confirmed, err, s.pastes, s.retries)
	}
	if s.reads != 1 {
		t.Fatalf("%d field reads, want only the one before the paste (the log showed the read)", s.reads)
	}
	s = newPasteSim()
	s.readable = false
	s.lose = 1
	confirmed, err = s.paster().run()
	if err != nil || confirmed || s.pastes != 2 {
		t.Fatalf("lost: confirmed=%v err=%v pastes=%d, want a second, unconfirmed paste", confirmed, err, s.pastes)
	}
}

func TestReadButDroppedPasteIsPastedOnceMore(t *testing.T) {
	// The app read the pasteboard for Cmd-V and dropped the paste (seen on the device 0.1 s
	// after the field got focus): pasteReadWait after the read it is pasted once more.
	s := newPasteSim()
	s.readDrop = 1
	p := s.paster()
	start := s.now
	confirmed, err := p.run()
	if err != nil || confirmed || s.pastes != 2 || s.value != "abc" {
		t.Fatalf("confirmed=%v err=%v pastes=%d value=%q, want a second paste that lands", confirmed, err, s.pastes, s.value)
	}
	if took := s.now.Sub(start); took < pasteReadWait {
		t.Fatalf("pasted again after %v, want at least %v after the read", took, pasteReadWait)
	}
}

func TestReadButUnchangedFieldEndsAfterOneMorePaste(t *testing.T) {
	// A field that never shows a paste it read (it rejects pastes): one more paste, then the
	// call ends pasteReadWait after that one's read.
	s := newPasteSim()
	s.showDelay = time.Hour
	p := s.paster()
	start := s.now
	confirmed, err := p.run()
	if err != nil || confirmed || s.pastes != 2 {
		t.Fatalf("confirmed=%v err=%v pastes=%d, want two pastes", confirmed, err, s.pastes)
	}
	if took := s.now.Sub(start); took < 2*pasteReadWait || took > 2*pasteReadWait+time.Second {
		t.Fatalf("took %v, want about %v", took, 2*pasteReadWait)
	}
}

func TestFieldTransformingThePasteIsConfirmed(t *testing.T) {
	// A field that formats its text: the paste arrived but the value is not the typed text.
	s := newPasteSim()
	s.text = "+48 123 456 789"
	confirmed, err := s.paster().run()
	if err != nil || !confirmed || s.pastes != 1 {
		t.Fatalf("confirmed=%v err=%v pastes=%d", confirmed, err, s.pastes)
	}
}

func TestWithoutALogTheFixedWindowDecides(t *testing.T) {
	// No log capture: the field is read at once and 150 ms later, and an unreadable field is
	// pasted once, unconfirmed.
	s := newPasteSim()
	s.lose = 2
	p := s.paster()
	p.obs = nil
	confirmed, err := p.run()
	if err != nil || confirmed || s.pastes != 2 {
		t.Fatalf("lost: confirmed=%v err=%v pastes=%d, want a second paste and no third", confirmed, err, s.pastes)
	}
	s = newPasteSim()
	s.readable = false
	p = s.paster()
	p.obs = nil
	confirmed, err = p.run()
	if err != nil || confirmed || s.pastes != 1 {
		t.Fatalf("unreadable: confirmed=%v err=%v pastes=%d, want one paste", confirmed, err, s.pastes)
	}
	s = newPasteSim()
	s.showDelay = 400 * time.Millisecond // later than the window: the old double paste
	p = s.paster()
	p.obs = nil
	if _, err := p.run(); err != nil || s.pastes != 2 {
		t.Fatalf("late: err=%v pastes=%d, want the fixed window's second paste", err, s.pastes)
	}
}

func TestPasteLogCountsOnlyPastedKeyReads(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	sys := &streamEvent{ProcessImagePath: "/Library/Developer/CoreSimulator/Volumes/iOS_23F77/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.5.simruntime/Contents/Resources/RuntimeRoot/usr/libexec/pasted"}
	app := &streamEvent{ProcessImagePath: "/Users/x/Library/Developer/CoreSimulator/Devices/U/data/Containers/Bundle/Application/C/Evil.app/pasted"}
	if !isPastedLine(sys, bundleKey(sys.ProcessImagePath)) {
		t.Fatal("the system's pasted is not recognized")
	}
	if isPastedLine(app, bundleKey(app.ProcessImagePath)) {
		t.Fatal("an app's executable named pasted speaks for the pasteboard daemon")
	}
	var pl pasteLog
	pl.add(at, "Paste allowed silently.")
	if pl.readSince(at) || !pl.last.Equal(at) {
		t.Fatalf("a non-read line counted as a read (or last not kept): %+v", pl)
	}
	pl.add(at.Add(time.Millisecond), "KeyCommand")
	if !pl.readSince(at) || pl.readSince(at.Add(2*time.Millisecond)) {
		t.Fatalf("readSince: %+v", pl)
	}
	for i := 0; i < 3*maxPasteReads; i++ {
		pl.add(at.Add(time.Duration(i+2)*time.Millisecond), "KeyCommand")
	}
	if len(pl.reads) != maxPasteReads {
		t.Fatalf("kept %d reads, want %d", len(pl.reads), maxPasteReads)
	}
}

func TestStreamFeedsThePasteObserver(t *testing.T) {
	// The real ingest path: pasted's lines reach the observer, an app's line does not count,
	// and a replaced stream never reports caught up.
	b, dl, s := testCapture(t)
	pasted := testRT + "/usr/libexec/pasted"
	if b.pasteObserverFor(testUDID, func() {}) != nil {
		t.Fatal("an observer before the stream's first line")
	}
	ingestLine(t, b, dl, s, line(t0, "Default", 7, testApp+"/AgemuTestApp", testApp+"/AgemuTestApp", "", "", "KeyCommand"))
	obs := b.pasteObserverFor(testUDID, func() {})
	if obs == nil {
		t.Fatal("no observer for a running stream")
	}
	if obs.keyRead(t0) || obs.caughtUp(t0) {
		t.Fatal("an app's own line counted as pasted's")
	}
	if obs.proven() {
		t.Fatal("proven before pasted logged a KeyCommand")
	}
	ingestLine(t, b, dl, s, line(t0.Add(50*time.Millisecond), "Info", 8, pasted, pasted, "com.apple.Pasteboard", "Pasteboard", "BundleID (null) from team (null) is requesting pasteboard com.apple.UIKit.pboard.general"))
	if obs.keyRead(t0) || !obs.caughtUp(t0) || obs.proven() {
		t.Fatal("a pasted line that is not a key read")
	}
	ingestLine(t, b, dl, s, line(t0.Add(100*time.Millisecond), "Default", 8, pasted, pasted, "com.apple.Pasteboard", "Pasteboard", "KeyCommand"))
	if !obs.keyRead(t0) || !obs.proven() || obs.caughtUp(t0.Add(time.Second)) || obs.keyRead(t0.Add(time.Second)) {
		t.Fatal("observer does not reflect the delivered KeyCommand")
	}
	dl.mu.Lock()
	dl.stream = &logStream{done: make(chan struct{})}
	dl.mu.Unlock()
	if obs.caughtUp(t0) {
		t.Fatal("an observer of a replaced stream reports caught up")
	}
}
