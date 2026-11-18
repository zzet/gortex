package platform

import (
	"os"
	"testing"
)

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(self) = false, want true")
	}
	// Non-positive PIDs are never valid processes.
	if ProcessAlive(0) {
		t.Error("ProcessAlive(0) = true, want false")
	}
	if ProcessAlive(-1) {
		t.Error("ProcessAlive(-1) = true, want false")
	}
}

func TestProcessExists(t *testing.T) {
	if !ProcessExists(os.Getpid()) {
		t.Error("ProcessExists(self) = false, want true")
	}
	for _, pid := range []int{0, -1} {
		if ProcessExists(pid) {
			t.Errorf("ProcessExists(%d) = true, want false", pid)
		}
	}
}

func TestShutdownSignals(t *testing.T) {
	if len(ShutdownSignals()) == 0 {
		t.Error("ShutdownSignals() returned no signals")
	}
}

func TestDetachSysProcAttr(t *testing.T) {
	if DetachSysProcAttr() == nil {
		t.Error("DetachSysProcAttr() = nil, want non-nil SysProcAttr")
	}
}

func TestTerminateAndKillGuardNonPositivePID(t *testing.T) {
	// pid <= 0 must be a guarded no-op on every platform — callers rely
	// on this when a PID file is missing or malformed.
	if err := TerminateProcess(0); err != nil {
		t.Errorf("TerminateProcess(0) = %v, want nil", err)
	}
	if err := KillProcess(-1); err != nil {
		t.Errorf("KillProcess(-1) = %v, want nil", err)
	}
}
