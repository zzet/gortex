//go:build !darwin && !linux && !windows

package platform

// Other Unix platforms keep the kill(pid, 0) liveness semantics.
func processIsZombie(_ int) bool { return false }
