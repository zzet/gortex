//go:build linux

package indexer

import "testing"

// TestIsSlowMountFSType pins the magic-number check slowWatchMount applies
// to a statfs result, independent of the host actually having a WSL2, SMB,
// or NFS mount available to probe. NFS (0x6969, NFS_SUPER_MAGIC) previously
// went undetected on any non-WSL2 host: slowWatchMount's own filesystem-type
// check was gated behind a WSL-only probe, so a native Linux host with a
// repo on an NFS mount fell through every safety net — the fsnotify backend
// then reliably failed its 5s readiness wait, and because that failure path
// returns before the adaptive poller is ever constructed, the repo ended up
// with neither fsnotify nor the poller: no update mechanism at all until a
// manual untrack+track.
func TestIsSlowMountFSType(t *testing.T) {
	cases := []struct {
		name string
		typ  int64
		want bool
	}{
		{"ext4/xfs/local (typical, unlisted)", 0xEF53, false},
		{"V9FS (WSL2 9p/drvfs)", 0x01021997, true},
		{"CIFS/SMB", 0xFF534D42, true},
		{"NFS_SUPER_MAGIC", 0x6969, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSlowMountFSType(c.typ); got != c.want {
				t.Errorf("isSlowMountFSType(%#x) = %v, want %v", c.typ, got, c.want)
			}
		})
	}
}
