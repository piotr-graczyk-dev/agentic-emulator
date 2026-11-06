package ios

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

// sysctlUint32 reads an integer sysctl. syscall.Sysctl returns the raw value bytes (with one
// trailing NUL dropped), little-endian on Apple Silicon and Intel.
func sysctlUint32(name string) (uint32, bool) {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return 0, false
	}
	b := []byte(s)
	for len(b) < 4 {
		b = append(b, 0)
	}
	return binary.LittleEndian.Uint32(b[:4]), true
}

// pressureLevel is macOS's memory pressure: 1 normal, 2 warn, 4 critical (what
// `memory_pressure` and Activity Monitor show).
func pressureLevel() uint32 {
	v, ok := sysctlUint32("kern.memorystatus_vm_pressure_level")
	if !ok {
		return 1
	}
	return v
}

// memoryPressured: macOS reports warn or critical memory pressure, or less than half of the
// memory is available (a second simulator booting next to a running one competes for it: on
// the test host an iPhone SE booted in 18.5 s next to a running iPhone 17 Pro, 6.3 s alone).
func memoryPressured() bool {
	if pressureLevel() >= 2 {
		return true
	}
	avail, ok := sysctlUint32("kern.memorystatus_level")
	return ok && avail < 50
}

func memoryState() string {
	names := map[uint32]string{1: "normal", 2: "warn", 4: "critical"}
	lvl := pressureLevel()
	s := names[lvl]
	if s == "" {
		s = fmt.Sprint(lvl)
	}
	if avail, ok := sysctlUint32("kern.memorystatus_level"); ok {
		s += fmt.Sprintf(", %d%% available", avail)
	}
	return "pressure " + s
}
