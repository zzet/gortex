package daemon

import "golang.org/x/sys/unix"

func socketPeerProcessID(fd uintptr) int {
	pid, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil {
		return 0
	}
	return pid
}
