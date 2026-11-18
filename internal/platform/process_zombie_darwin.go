package platform

import "golang.org/x/sys/unix"

// SZOMB from Darwin's sys/proc.h; x/sys/unix exposes P_stat but not its states.
const darwinProcessZombie = 5

func processIsZombie(pid int) bool {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	// A restricted or unavailable sysctl must preserve the kill probe's result.
	return err == nil && info.Proc.P_stat == darwinProcessZombie
}
