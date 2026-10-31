//go:build !linux && !darwin

package daemon

// There is no graceful detached-process signal on Windows. Do not claim a
// verified PID handoff on unsupported platforms; compatible RPC still works.
func socketPeerProcessID(uintptr) int { return 0 }
