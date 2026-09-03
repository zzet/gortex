//go:build !unix

package gitstate

import "io/fs"

// pathIdentity has no volume or object identity to report on platforms
// whose stat this package does not read. Callers see the unsupported
// kind and empty tokens, and must not draw conclusions from them.
//
// A platform that gains a real implementation drops itself from this
// file's build constraint — a pathevidence_windows.go alongside a
// `!unix && !windows` constraint here — and needs no change to the seam.
func pathIdentity(string, fs.FileInfo) (volumeKind, volumeToken, identity string) {
	return VolumeKindUnsupported, "", ""
}
