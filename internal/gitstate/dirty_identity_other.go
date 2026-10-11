//go:build !darwin && !linux

package gitstate

import "os"

func dirtyChangeIdentity(os.FileInfo) (device, inode uint64, sec, nsec int64, ok bool) {
	return 0, 0, 0, 0, false
}
func dirtyLocalFilesystem(*os.File) bool { return false }
