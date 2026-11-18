//go:build !windows

package platform

import (
	"errors"
	"syscall"
	"testing"
)

// A process owned by another user can't be signalled (kill returns EPERM)
// but still exists: ProcessExists must report it, while ProcessAlive keeps
// treating it as not reachable.
func TestProcessExistsOtherUser(t *testing.T) {
	if err := syscall.Kill(1, 0); !errors.Is(err, syscall.EPERM) {
		t.Skipf("kill(1, 0) = %v; needs a PID 1 owned by another user", err)
	}
	if !ProcessExists(1) {
		t.Error("ProcessExists(1) = false, want true for a process owned by another user")
	}
	if ProcessAlive(1) {
		t.Error("ProcessAlive(1) = true, want false for a process the caller cannot signal")
	}
}
