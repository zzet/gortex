package gitstate

import (
	"os"
	"syscall"
)

// Change evidence is necessary but not sufficient for digest reuse: the caller
// also requires a local descriptor filesystem and a quiet stamp.
func dirtyChangeIdentity(info os.FileInfo) (device, inode uint64, sec, nsec int64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ctimespec.Sec <= 0 || st.Ctimespec.Nsec <= 0 || st.Ctimespec.Nsec >= 1e9 {
		return 0, 0, 0, 0, false
	}
	return uint64(st.Dev), st.Ino, st.Ctimespec.Sec, st.Ctimespec.Nsec, true
}
func dirtyLocalFilesystem(file *os.File) bool {
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(file.Fd()), &stat); err != nil {
		return false
	}
	var name []byte
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return string(name) == "apfs"
}
