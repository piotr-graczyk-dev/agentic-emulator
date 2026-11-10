//go:build !windows

package ios

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// fakeHome is a homeDevice: fronts are the successive frontmost answers (the last repeats).
type fakeHome struct {
	fronts   []string
	reads    int
	pressed  time.Time
	settled  time.Time
	calls    []string
	frontErr error
}

func (f *fakeHome) frontmost(ctx context.Context) (string, error) {
	f.calls = append(f.calls, "front")
	if f.frontErr != nil {
		return "", f.frontErr
	}
	s := f.fronts[min(f.reads, len(f.fronts)-1)]
	f.reads++
	return s, nil
}

func (f *fakeHome) pressHomeButton(ctx context.Context) error {
	f.calls = append(f.calls, "press")
	f.pressed = time.Now()
	return nil
}

func (f *fakeHome) settle(ctx context.Context, quiet, max time.Duration, big bool) error {
	f.calls = append(f.calls, fmt.Sprintf("settle %v big=%v", quiet, big))
	f.settled = time.Now()
	return nil
}

func TestPressHomeOnTheHomeScreenWaitsOutSpringBoardsDoublePressWindow(t *testing.T) {
	f := &fakeHome{fronts: []string{springboardBundle}}
	if err := pressHome(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.calls) != "[front press settle 48ms big=true]" {
		t.Fatalf("calls = %v", f.calls)
	}
	if d := f.settled.Sub(f.pressed); d < homePressWindow {
		t.Fatalf("returned %v after the press; SpringBoard may act on it up to %v later", d, homePressWindow)
	}
}

func TestPressHomeWithAnAppInFrontWaitsForSpringBoard(t *testing.T) {
	f := &fakeHome{fronts: []string{"com.example.app", "com.example.app", "com.example.app", springboardBundle}}
	if err := pressHome(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	want := "[front press front front front settle 48ms big=true]"
	if fmt.Sprint(f.calls) != want {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if d := f.settled.Sub(f.pressed); d < homePressWindow {
		t.Fatalf("returned %v after the press, before the window ended", d)
	}
	// Unknown frontmost app: treated like an app in front.
	f = &fakeHome{frontErr: fmt.Errorf("no frontmost application")}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := pressHome(ctx, f); err == nil {
		t.Fatal("SpringBoard never came to the front: want an error")
	}
}

func TestResetPressesHomeOnlyWhenSpringBoardIsNotInFront(t *testing.T) {
	f := &fakeHome{fronts: []string{springboardBundle}}
	start := time.Now()
	if err := goHome(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.calls) != "[front settle 300ms big=false]" {
		t.Fatalf("SpringBoard in front: calls = %v, want no press", f.calls)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("took %v", time.Since(start))
	}
	// An app in front: the press reuses goHome's frontmost read (one read before the press).
	f = &fakeHome{fronts: []string{"com.example.app", springboardBundle}}
	if err := goHome(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	want := "[front press front settle 48ms big=true settle 300ms big=false]"
	if fmt.Sprint(f.calls) != want {
		t.Fatalf("app in front: calls = %v, want %v", f.calls, want)
	}
}
