//go:build !unix && !windows

package platform

import (
	"errors"
	"testing"
)

func TestProcessCPUTimeUnsupportedIsUnavailable(t *testing.T) {
	cpu, err := ProcessCPUTime()
	if cpu != 0 || !errors.Is(err, ErrProcessCPUUnavailable) {
		t.Fatalf("unsupported CPU must be unavailable: %s,%v", cpu, err)
	}
}
