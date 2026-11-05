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

// memoryPressured: macOS reports warn or critical memory pressure.
func memoryPressured() bool { return pressureLevel() >= 2 }

// memoryCritical: macOS reports critical memory pressure.
func memoryCritical() bool { return pressureLevel() >= 4 }

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
