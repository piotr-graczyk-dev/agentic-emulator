package android

// Interrupted first boots. When the very first cold boot of a new AVD is interrupted about 10 s
// in, twice in a row (a gRPC power-off, SIGTERM or agemu's shutdown; agemu before 2026-11-08
// too), the guest's /data is left so that system_server crash-loops (zygote restarted 279 times
// in 3 min) and adbd never starts: every later boot fails with "device offline" (4 of 4 in round
// 6's re-check; one interruption, two at 20 s or early kills did no harm). One boot with
// -wipe-data repairs it.
//
// agemu repairs it by itself, and only where nothing can be lost:
//   - Before the first launch of an AVD that no emulator has ever started (no userdata overlay,
//     no baseline, no completed-boot record), it writes firstBootMarker into the AVD folder.
//   - When a boot completes (sys.boot_completed=1, or the agent from a baseline answers), it
//     writes bootedRecord, then removes the marker.
//   - A boot of an AVD that has the marker, no completed-boot record and no baseline adds
//     -wipe-data: that guest never finished booting, so nothing was installed in it.
//
// An AVD that ever completed a boot under this agemu has the record; one from an older agemu, or
// started by any other tool, has a userdata overlay and is never marked. Without the marker
// nothing is ever wiped.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	firstBootMarker = "agemu-first-boot-incomplete"
	bootedRecord    = "agemu-booted"
	userdataOverlay = "userdata-qemu.img.qcow2" // created by the emulator's first start
)

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// everStarted reports whether an emulator has ever started the AVD (it creates the userdata
// overlay on its first start).
func (a avdInfo) everStarted() bool { return exists(filepath.Join(a.Dir, userdataOverlay)) }

func (a avdInfo) hasBootedRecord() bool { return exists(filepath.Join(a.Dir, bootedRecord)) }

func (a avdInfo) hasFirstBootMarker() bool { return exists(filepath.Join(a.Dir, firstBootMarker)) }

// markFirstBoot writes the first-boot marker before the first launch of an AVD that has never been
// started, booted or baselined. It reports whether the AVD is (now) marked.
func markFirstBoot(a avdInfo) (bool, error) {
	if a.hasFirstBootMarker() {
		return true, nil
	}
	if a.everStarted() || a.hasBootedRecord() || a.hasBaseline() {
		return false, nil
	}
	err := writeFileAtomic(filepath.Join(a.Dir, firstBootMarker), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"))
	return err == nil, err
}

// firstBootRepair: AGEMU_ANDROID_FIRSTBOOT_REPAIR=0 turns the repair off (the marker is still
// kept, so turning it back on repairs the AVD at its next boot).
func firstBootRepair() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGEMU_ANDROID_FIRSTBOOT_REPAIR"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// needsWipe reports whether the AVD's first boot was started and never completed: the marker is
// there, and there is neither a completed-boot record nor a baseline. Only then is -wipe-data
// added; every other AVD keeps its data.
func needsWipe(a avdInfo) bool {
	return a.hasFirstBootMarker() && !a.hasBootedRecord() && !a.hasBaseline()
}

// recordBooted records that the AVD completed a boot: the record first, then the marker goes, so
// a crash in between leaves an AVD that is never wiped. It reports whether a marker was removed.
func recordBooted(a avdInfo) (bool, error) {
	if !a.hasBootedRecord() {
		if err := writeFileAtomic(filepath.Join(a.Dir, bootedRecord), []byte(time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
			return false, err
		}
	}
	if !a.hasFirstBootMarker() {
		return false, nil
	}
	if err := os.Remove(filepath.Join(a.Dir, firstBootMarker)); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

// writeFileAtomic writes data to a temporary file in the same folder and renames it into place.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
