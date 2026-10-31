//go:build !unix && !windows

package platform

import "time"

// ProcessCPUTime reports unavailable when no process CPU endpoint is supported.
func ProcessCPUTime() (time.Duration, error) {
	return 0, ErrProcessCPUUnavailable
}
