package daemon

import (
	"net"
	"syscall"
)

// peerProcessID is kernel evidence for the process owning this connected
// socket. Failure only disables PID-based upgrade handoff, not normal RPC.
func peerProcessID(conn net.Conn) int {
	c, ok := conn.(syscall.Conn)
	if !ok {
		return 0
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int
	if err := raw.Control(func(fd uintptr) { pid = socketPeerProcessID(fd) }); err != nil {
		return 0
	}
	return pid
}
