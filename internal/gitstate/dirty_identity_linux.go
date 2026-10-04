package gitstate

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func dirtyChangeIdentity(info os.FileInfo) (device, inode uint64, sec, nsec int64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ctim.Sec <= 0 || st.Ctim.Nsec <= 0 || st.Ctim.Nsec >= 1e9 {
		return 0, 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), int64(st.Ctim.Sec), int64(st.Ctim.Nsec), true
}
func dirtyLocalFilesystem(file *os.File) bool {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return false
	}
	// Local-kernel clocks are only one prerequisite. Quiet-time and clock
	// checks still apply; nanosecond field width alone is not a guarantee.
	switch uint32(stat.Type) {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.TMPFS_MAGIC:
		return true
	default:
		return false
	}
}
