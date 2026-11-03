//go:build windows

package mcp

import "os"

// Windows repository paths do not expose POSIX FIFOs. The caller performs
// Lstat before open and f.Stat immediately afterward to reject non-regular
// filesystem objects and replacement races.
func openPhysicalEvidenceFile(path string) (*os.File, error) {
	return os.Open(path)
}

// The request root confines resolution; the caller still checks every relative
// parent, the named leaf and the opened regular-file descriptor before/after I/O.
func openRootPhysicalEvidenceFile(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY, 0)
}
