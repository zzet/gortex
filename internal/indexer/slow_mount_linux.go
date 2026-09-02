//go:build linux

package indexer

import (
	"os"
	"syscall"
)

// slowWatchMount reports whether path lives on a filesystem where native
// fsnotify is unreliable or prohibitively slow — a Windows drive surfaced
// into WSL2 via 9p/drvfs (inotify events arrive late or never), an SMB/CIFS
// share (whether mounted directly or via WSL2), or an NFS mount (the kernel
// inotify backend does not reliably notice changes made by another NFS
// client, and even same-client notifications can arrive late enough to miss
// the watcher's readiness window entirely — see confirmWatchActive's 5s
// timeout). On such a mount the watcher disables fsnotify and relies on the
// adaptive poller + git hooks, which are mount-agnostic.
// GORTEX_FORCE_FSNOTIFY=1 forces native fsnotify on regardless.
func slowWatchMount(path string) bool {
	if path == "" || os.Getenv("GORTEX_FORCE_FSNOTIFY") == "1" {
		return false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return isSlowMountFSType(int64(st.Type))
}

// isSlowMountFSType is the magic-number check slowWatchMount applies to a
// statfs result. Factored out so it can be unit-tested directly against
// known-bad magic numbers without needing a live WSL2, SMB, or NFS mount in
// the test environment.
func isSlowMountFSType(fsType int64) bool {
	switch fsType {
	case 0x01021997, // V9FS_MAGIC — 9p, WSL2's drvfs transport for Windows drives
		0xFF534D42, // CIFS_MAGIC — SMB/CIFS share
		0x6969:     // NFS_SUPER_MAGIC — NFS v3/v4
		return true
	}
	return false
}
