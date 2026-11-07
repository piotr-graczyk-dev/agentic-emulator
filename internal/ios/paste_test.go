package ios

import (
	"sync"
	"testing"
	"time"
)

// field simulates a focused text field that a paste changes.
type field struct {
	mu     sync.Mutex
	value  string
	pastes int
}

func (f *field) read() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.value, true
}

func (f *field) paste(text string) func() error {
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pastes++
		f.value += text
		return nil
	}
}

func TestPasteWaitsForTheBaselineBeforePasting(t *testing.T) {
	// The baseline read is slower than setting the pasteboard. If the paste went out before
	// the baseline was taken, the baseline would already hold the pasted text, the field would
	// look unchanged, and the text would be pasted twice.
	f := &field{}
	slowFirst := true
	read := func() (string, bool) {
		if slowFirst {
			slowFirst = false
			time.Sleep(100 * time.Millisecond)
		}
		return f.read()
	}
	confirmed, err := pasteVerified(func() error { return nil }, f.paste("abc"), read, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if f.pastes != 1 || f.value != "abc" || !confirmed {
		t.Fatalf("pastes=%d value=%q confirmed=%v, want one confirmed paste", f.pastes, f.value, confirmed)
	}
}

func TestPasteRetriesOnlyWhenTheFieldDidNotChange(t *testing.T) {
	lost := &field{}
	retried := false
	confirmed, err := pasteVerified(func() error { return nil }, func() error { lost.pastes++; return nil }, lost.read, func() { retried = true })
	if err != nil {
		t.Fatal(err)
	}
	if !retried || lost.pastes != 2 {
		t.Fatalf("lost paste: retried=%v pastes=%d, want a second paste", retried, lost.pastes)
	}
	if confirmed {
		t.Fatal("a second paste is not confirmed (the pasteboard must not be restored under it)")
	}

	// A field that formats its text: the paste arrived but the value is not the typed text.
	formatted := &field{}
	paste := func() error { formatted.pastes++; formatted.value = "+48 123 456 789"; return nil }
	confirmed, err = pasteVerified(func() error { return nil }, paste, formatted.read, func() { t.Fatal("retried a transformed paste") })
	if err != nil {
		t.Fatal(err)
	}
	if formatted.pastes != 1 || !confirmed {
		t.Fatalf("formatted field pasted %d times, confirmed=%v", formatted.pastes, confirmed)
	}
}

func TestPasteIntoUnreadableFieldIsNotConfirmed(t *testing.T) {
	// A secure field (or no readable text field in focus): the paste is sent once and not
	// confirmed, so the pasteboard keeps the typed text until the app has read it.
	pastes := 0
	confirmed, err := pasteVerified(func() error { return nil }, func() error { pastes++; return nil },
		func() (string, bool) { return "", false }, func() { t.Fatal("retried an unreadable field") })
	if err != nil || confirmed || pastes != 1 {
		t.Fatalf("confirmed=%v err=%v pastes=%d, want one unconfirmed paste", confirmed, err, pastes)
	}
}
