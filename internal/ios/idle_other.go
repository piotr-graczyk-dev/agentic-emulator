//go:build !darwin

package ios

import "errors"

func procDirty(pid int) (uint32, error) { return 0, errors.New("dirty tracking needs macOS") }

func procUsageOf(pid int) (procUsage, error) {
	return procUsage{}, errors.New("resource usage needs macOS")
}
