//go:build unix

package gitstate

import (
	"fmt"
	"io/fs"
	"syscall"
)

// pathIdentity derives volume and object identity from a Unix stat.
// The device number identifies the mounted filesystem; device plus
// inode identifies the object on it. Both are formatted as opaque
// decimal strings — callers compare them, they never interpret them.
//
// The path is unused: a Unix stat already carries both numbers, so the
// identity here is derived from the object, never from how it was
// spelled.
func pathIdentity(_ string, fi fs.FileInfo) (volumeKind, volumeToken, identity string) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return VolumeKindUnsupported, "", ""
	}
	volumeToken = fmt.Sprintf("%d", st.Dev)
	identity = fmt.Sprintf("%s:%s:%d", VolumeKindUnixDev, volumeToken, st.Ino)
	return VolumeKindUnixDev, volumeToken, identity
}
