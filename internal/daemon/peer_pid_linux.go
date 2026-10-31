package daemon

import "golang.org/x/sys/unix"

func socketPeerProcessID(fd uintptr) int {
	cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0
	}
	return int(cred.Pid)
}
