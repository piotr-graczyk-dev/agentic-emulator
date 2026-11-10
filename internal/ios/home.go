//go:build !windows

package ios

import (
	"context"
	"fmt"
	"time"
)

// The home button.
//
// The simulator's HID sender has no press count or "terminal press" support, so SpringBoard
// emulates the double-press detection: it logs "appears to be synthesizing home button events
// without press count/terminal support; SpringBoard is providing emulation for max count:2" and
// acts on a single press only when no second press has come, 305-323 ms after the button-up (16
// presses at load average 125-218, on the home screen and with an app in front). An app launched
// in that window comes to the front and is then sent to the background by the press. So a home
// press returns only once SpringBoard has acted on it, and reset does not press home when
// SpringBoard is already in front.

// homePressWindow is how long after a home press SpringBoard may still act on it: twice the
// longest delay measured.
const homePressWindow = 650 * time.Millisecond

const springboardBundle = "com.apple.springboard"

// homeDevice is what pressing home needs from a device (helperHome; a fake in tests).
type homeDevice interface {
	// frontmost returns the frontmost application's bundle id.
	frontmost(ctx context.Context) (string, error)
	pressHomeButton(ctx context.Context) error
	// settle waits until the screen has had no change (big: no big change) for quiet, at most
	// max.
	settle(ctx context.Context, quiet, max time.Duration, big bool) error
}

// pressHome presses the home button and returns once SpringBoard has acted on the press: with an
// app in front (or the frontmost app unknown), once SpringBoard is in front; in any case not
// before homePressWindow has passed since the press; then once the screen has had no big change
// for 48 ms (at most 1 s).
func pressHome(ctx context.Context, d homeDevice) error {
	front, ferr := d.frontmost(ctx)
	return pressHomeFrom(ctx, d, front, ferr)
}

// pressHomeFrom is pressHome with the frontmost application already read (front, or ferr when
// the read failed).
func pressHomeFrom(ctx context.Context, d homeDevice, front string, ferr error) error {
	// The window counts from just before the press is sent: a press the helper takes longer than
	// about 330 ms to deliver (extreme host load) can still be acted on after this returns.
	pressed := time.Now()
	if err := d.pressHomeButton(ctx); err != nil {
		return err
	}
	if ferr != nil || front != springboardBundle {
		if err := waitSpringBoard(ctx, d, 15*time.Second); err != nil {
			return err
		}
	}
	if err := sleepCtx(ctx, homePressWindow-time.Since(pressed)); err != nil {
		return err
	}
	return d.settle(ctx, 48*time.Millisecond, time.Second, true)
}

// goHome brings SpringBoard to the front after reset terminated every app, and waits until the
// screen has been still for 300 ms (at most 2 s): apps just terminated may still be exiting, and
// an app launched then failed to launch. SpringBoard is normally in front already; home is
// pressed only when it is not (pressHome), since a press SpringBoard acts on later sends the
// next launched app to the background.
func goHome(ctx context.Context, d homeDevice) error {
	if front, err := d.frontmost(ctx); err != nil || front != springboardBundle {
		if err := pressHomeFrom(ctx, d, front, err); err != nil {
			return err
		}
	}
	_ = d.settle(ctx, 300*time.Millisecond, 2*time.Second, false)
	return nil
}

// waitSpringBoard waits until SpringBoard is the frontmost application, at most timeout.
func waitSpringBoard(ctx context.Context, d homeDevice, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := "nothing yet"
	for {
		front, err := d.frontmost(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case front == springboardBundle:
			return nil
		default:
			last = fmt.Sprintf("frontmost is %q", front)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SpringBoard did not come to the front within %v after pressing home (%s)", timeout, last)
		}
		if err := sleepCtx(ctx, 30*time.Millisecond); err != nil {
			return err
		}
	}
}

// sleepCtx sleeps for d (nothing when d <= 0) unless ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// helperHome is a simulator's homeDevice, through the helper.
type helperHome struct {
	b    *Backend
	udid string
}

// frontmost reports the bundle id accessibility gives for the frontmost application (on iPad
// found by a hit test on the screen, so SpringBoard is recognised there too).
func (h helperHome) frontmost(ctx context.Context) (string, error) {
	var front struct {
		Pid      int    `json:"pid"`
		BundleID string `json:"bundleId"`
	}
	if err := h.b.call(ctx, "frontmost", map[string]any{"udid": h.udid, "timeoutMs": 3000}, &front); err != nil {
		return "", err
	}
	if front.BundleID == "" {
		return fmt.Sprintf("pid %d", front.Pid), nil
	}
	return front.BundleID, nil
}

func (h helperHome) pressHomeButton(ctx context.Context) error {
	return h.b.call(ctx, "key", map[string]any{"udid": h.udid, "key": "home"}, nil)
}

func (h helperHome) settle(ctx context.Context, quiet, max time.Duration, big bool) error {
	params := map[string]any{"udid": h.udid, "quietMs": quiet.Milliseconds(), "maxMs": max.Milliseconds(), "big": big}
	return h.b.call(ctx, "settle", params, nil)
}
