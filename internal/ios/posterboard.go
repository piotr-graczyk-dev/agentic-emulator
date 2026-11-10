//go:build !windows

package ios

// PosterBoard's wallpaper gallery, kept from starting at boot. PosterBoard keeps the time it last
// refreshed each wallpaper provider's suggestions (its "dynamic descriptors") in TransientInfo
// files in the device's tmp directory, and those of one boot were gone at the next. So at every
// boot it finds them all expired (iOS 26.5: an 8-hour interval) and starts every provider, one
// extension process each (14 on an iPhone 17 Pro, the lock screen's among them), to fetch them
// again, and then renders the gallery's snapshots. lean.go stops those extensions once they run,
// but by then they have rendered. PosterBoard has a switch that turns all of this off: its keynote
// (demo) mode, KEYNOTE_MODE in its preferences. With it, PosterBoard's code reads every
// provider's refresh frequency as never, skips its prewarm, and keeps its stored gallery instead of
// asking for a new one. A lean simulator turns it on once PosterBoard has built its gallery for
// the running iOS build, so later boots start only the lock screen's provider. The wallpaper
// gallery still opens from the lock screen, drawn from the stored gallery, and starts a provider
// when one of its wallpapers is opened. PosterBoard's data store migration, which runs when the
// iOS build changes, deletes the mode (its log text says so; not tried), and agemu sets it again
// once PosterBoard has rebuilt the gallery. AGEMU_IOS_LEAN=0 deletes it.

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// posterBoardPrefs is PosterBoard's preferences domain that holds the mode and its update log.
	posterBoardPrefs = "com.apple.PosterBoard.unprotectedUserDefaults"
	// posterBoardStash is the domain where PosterBoard notes the iOS build it last refreshed its
	// providers for (stashedBuildVersionString).
	posterBoardStash = "com.apple.PosterBoard"
	keynoteModeKey   = "KEYNOTE_MODE"
)

// posterBoardState is what PosterBoard's preferences say about its gallery.
type posterBoardState struct {
	// frozen: KEYNOTE_MODE is on.
	frozen bool
	// lastUpdate is the newest entry of PBFExecuteUpdateHistory (PosterBoard keeps the last 5):
	// "<local time> | prewarm | <seconds>s | <strategy> | <options> | <options> | SUCCESS", or
	// "| ERROR: <reason>" at the end when it failed or was skipped.
	lastUpdate string
	// snapshotsBuild (SnapshotsBuildVersion) is the iOS build PosterBoard migrated its data store
	// to; stashedBuild (stashedBuildVersionString) the build it last refreshed its providers for.
	// They differ after an iOS update until PosterBoard has refreshed all providers.
	snapshotsBuild, stashedBuild string
}

// galleryBuilt reports whether PosterBoard has built its gallery for the running iOS build in this
// boot: its newest update succeeded and is not the one that was newest before the boot (before),
// and it has refreshed the providers for the build of its data store. Only an update of this
// boot counts, because after an iOS update the newest entry before the boot belongs to the old
// build, which PosterBoard is still replacing at the first look.
func galleryBuilt(s posterBoardState, before string) bool {
	return strings.HasSuffix(s.lastUpdate, " | SUCCESS") && s.lastUpdate != before &&
		s.stashedBuild != "" && s.stashedBuild == s.snapshotsBuild
}

// posterBoardPrefsEnv reads and sets PosterBoard's preferences: in a booted device
// (devicePosterBoard), or a fake in tests. state reads both domains (the build stash only when
// the mode is off); frozen reads only whether the mode is on.
type posterBoardPrefsEnv interface {
	state() (posterBoardState, error)
	frozen() (bool, error)
	setFrozen(on bool) error
}

// freezeGalleryStep looks at PosterBoard once during a cold boot's gallery passes and turns its
// keynote mode on once galleryBuilt. It returns true when nothing is left to do in this boot: the
// mode is on (now or from an earlier boot), or a read or write failed (logged; the next boot tries
// again). Until then the passes go on (see nextGalleryPass).
func freezeGalleryStep(env posterBoardPrefsEnv, before, name string) bool {
	s, err := env.state()
	if err != nil {
		log.Printf("ios lean %s: reading PosterBoard's preferences: %v (the wallpaper gallery starts at the next boot again)", name, err)
		return true
	}
	if s.frozen {
		return true
	}
	if !galleryBuilt(s, before) {
		return false
	}
	if err := env.setFrozen(true); err != nil {
		log.Printf("ios lean %s: turning PosterBoard's keynote mode on: %v", name, err)
		return true
	}
	if now, err := env.state(); err != nil || !now.frozen {
		log.Printf("ios lean %s: PosterBoard's keynote mode did not turn on (%v)", name, err)
		return true
	}
	log.Printf("ios lean %s: PosterBoard's keynote mode is on: from the next boot its wallpaper gallery does not start (gallery built for iOS build %s)", name, s.stashedBuild)
	return true
}

// unfreezeGallery turns PosterBoard's keynote mode off (AGEMU_IOS_LEAN=0): from the next boot it
// refreshes and prewarms its gallery as usual.
// It reads one preferences domain (one spawn in the device) when the mode is off, as it is on
// every device that was never lean.
func unfreezeGallery(env posterBoardPrefsEnv, name string) error {
	on, err := env.frozen()
	if err != nil || !on {
		return err
	}
	if err := env.setFrozen(false); err != nil {
		return err
	}
	if on, err = env.frozen(); err != nil {
		return err
	}
	if on {
		return fmt.Errorf("PosterBoard's %s is still on after deleting it", keynoteModeKey)
	}
	log.Printf("ios lean %s: PosterBoard's keynote mode is off: its wallpaper gallery starts from the next boot", name)
	return nil
}

// devicePosterBoard is the posterBoardPrefsEnv of a booted simulator. It goes through the
// device's own `defaults`, so the device's preferences daemon sees every change.
type devicePosterBoard struct {
	b    *Backend
	udid string
}

func (p devicePosterBoard) defaults(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(p.b.bg, 15*time.Second)
	defer cancel()
	return p.b.spawn(ctx, p.udid, "usr/bin/defaults", args...)
}

func (p devicePosterBoard) state() (posterBoardState, error) {
	out, err := p.defaults("export", posterBoardPrefs, "-")
	if err != nil {
		return posterBoardState{}, err
	}
	s, err := parsePosterBoardPrefs([]byte(out))
	if err != nil || s.frozen {
		return s, err
	}
	if out, err = p.defaults("export", posterBoardStash, "-"); err != nil {
		return s, err
	}
	stash, err := plistStrings([]byte(out))
	s.stashedBuild = stash["stashedBuildVersionString"]
	return s, err
}

func (p devicePosterBoard) frozen() (bool, error) {
	out, err := p.defaults("export", posterBoardPrefs, "-")
	if err != nil {
		return false, err
	}
	s, err := parsePosterBoardPrefs([]byte(out))
	return s.frozen, err
}

func (p devicePosterBoard) setFrozen(on bool) error {
	args := []string{"delete", posterBoardPrefs, keynoteModeKey}
	if on {
		args = []string{"write", posterBoardPrefs, keynoteModeKey, "-bool", "YES"}
	}
	// The helper does not return the exit status: state() checks the result.
	_, err := p.defaults(args...)
	return err
}

// galleryUpdateBeforeBoot reads the newest PosterBoard update entry from the preferences file of
// a device that is shut down (dataPath: its data directory); "" when there is none yet.
func galleryUpdateBeforeBoot(dataPath string) string {
	if dataPath == "" {
		return ""
	}
	file := filepath.Join(dataPath, "Library/Preferences", posterBoardPrefs+".plist")
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	out, err := exec.Command("plutil", "-convert", "xml1", "-o", "-", file).Output()
	if err != nil {
		log.Printf("ios lean: reading %s: %v", file, err)
		return ""
	}
	s, _ := parsePosterBoardPrefs(out)
	return s.lastUpdate
}

// parsePosterBoardPrefs reads the posterBoardPrefs domain from an XML property list.
func parsePosterBoardPrefs(data []byte) (posterBoardState, error) {
	var s posterBoardState
	err := plistTop(data, func(key string, v any) {
		switch key {
		case keynoteModeKey:
			s.frozen, _ = v.(bool)
		case "SnapshotsBuildVersion":
			s.snapshotsBuild, _ = v.(string)
		case "PBFExecuteUpdateHistory":
			if h, ok := v.([]string); ok && len(h) > 0 {
				s.lastUpdate = h[len(h)-1]
			}
		}
	})
	return s, err
}

// plistStrings returns the string values of an XML property list's top-level dictionary.
func plistStrings(data []byte) (map[string]string, error) {
	m := map[string]string{}
	err := plistTop(data, func(key string, v any) {
		if s, ok := v.(string); ok {
			m[key] = s
		}
	})
	return m, err
}

// plistTop decodes the top-level dictionary of an XML property list (as `defaults export` and
// `plutil -convert xml1` write it) and calls f for each key with its value: a string, a bool, or
// the strings of an array ([]string). Values of other types are skipped (f is not called).
func plistTop(data []byte, f func(key string, v any)) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Up to the top-level <dict>.
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("property list without a dictionary: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			break
		}
	}
	key := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("property list: %v", err)
		}
		switch t := tok.(type) {
		case xml.EndElement: // </dict>
			return nil
		case xml.StartElement:
			if t.Name.Local == "key" {
				if err := dec.DecodeElement(&key, &t); err != nil {
					return err
				}
				continue
			}
			v, err := plistValue(dec, t)
			if err != nil {
				return err
			}
			if v != nil {
				f(key, v)
			}
		}
	}
}

// plistValue decodes the value element start: a string, a bool, the strings of an array, or nil
// (skipped) for anything else.
func plistValue(dec *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string":
		var s string
		err := dec.DecodeElement(&s, &start)
		return s, err
	case "true", "false":
		return start.Name.Local == "true", dec.Skip()
	case "array":
		strs := []string{}
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				return nil, io.ErrUnexpectedEOF
			} else if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return strs, nil
			case xml.StartElement:
				if t.Name.Local != "string" {
					if err := dec.Skip(); err != nil {
						return nil, err
					}
					continue
				}
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, err
				}
				strs = append(strs, s)
			}
		}
	}
	return nil, dec.Skip()
}
