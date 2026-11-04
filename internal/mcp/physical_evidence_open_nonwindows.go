//go:build !windows

package mcp

import (
	"os"
	"syscall"
)

// openPhysicalEvidenceFile prevents a repository-local FIFO or device from
// blocking the MCP request if the path changes after the pre-open Lstat. The
// caller still verifies the opened handle with f.Stat before reading.
func openPhysicalEvidenceFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// The request root confines resolution; the caller still checks every relative
// parent, the named leaf and the opened regular-file descriptor before/after I/O.
func openRootPhysicalEvidenceFile(root *os.Root, relative string) (*os.File, error) {
	return root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
