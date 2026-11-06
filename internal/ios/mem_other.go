//go:build !darwin

package ios

func memoryPressured() bool { return false }
func memoryState() string   { return "unknown" }
