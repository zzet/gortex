package daemon

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

// TestProtocolFallbackEmbedded covers the recoverable-error classification the
// MCP proxy uses to decide between dialing the daemon and running the embedded
// server: only a missing daemon permits fallback; a live protocol mismatch
// must be resolved without opening another store.
func TestProtocolFallbackEmbedded(t *testing.T) {
	if !ShouldFallBackToEmbedded(ErrDaemonUnavailable) {
		t.Error("ErrDaemonUnavailable must trigger embedded fallback")
	}
	// Dial wraps a protocol_mismatch ack into ErrProtocolVersionMismatch.
	wrapped := fmt.Errorf("%w: server v1 vs client v2", ErrProtocolVersionMismatch)
	if ShouldFallBackToEmbedded(wrapped) {
		t.Error("a wrapped ErrProtocolVersionMismatch must not trigger embedded fallback")
	}
	// A failed handoff can carry both the original live-daemon refusal and
	// a later unavailable replacement. That still must not open a store.
	if ShouldFallBackToEmbedded(errors.Join(ErrDaemonUnavailable, wrapped)) {
		t.Error("a later absence must not erase an observed protocol mismatch")
	}
	if ShouldFallBackToEmbedded(errors.New("permission denied")) {
		t.Error("an unrelated error must NOT trigger fallback (would mask a real bug)")
	}
	if ShouldFallBackToEmbedded(nil) {
		t.Error("nil must not trigger fallback")
	}
}

func TestClassifyDaemonProbeError(t *testing.T) {
	missing := classifyDaemonProbeError(&net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT})
	if !errors.Is(missing, ErrDaemonUnavailable) {
		t.Fatalf("missing socket error = %v, want ErrDaemonUnavailable", missing)
	}
	if !ShouldFallBackToEmbedded(missing) {
		t.Fatal("a missing socket must permit the configured embedded fallback")
	}

	permission := classifyDaemonProbeError(&net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES})
	if !errors.Is(permission, syscall.EACCES) {
		t.Fatalf("permission error = %v, want EACCES", permission)
	}
	if ShouldFallBackToEmbedded(permission) {
		t.Fatal("a socket permission failure must not permit embedded fallback")
	}
}
