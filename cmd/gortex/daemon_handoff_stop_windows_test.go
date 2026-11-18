//go:build windows

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestDaemonHandoffWindowsMismatchRequiresMatchingBinary(t *testing.T) {
	err := stopDaemonForHandoff(daemonIdentity{
		version: "v0.64.10", protocol: 0, protocolKnown: true,
		err: &daemon.ProtocolMismatchError{DaemonVersion: "v0.64.10", DaemonProtocol: 0, DaemonProtocolKnown: true, ClientProtocol: daemon.ProtocolVersion},
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable on Windows") || !strings.Contains(err.Error(), "matching binary") {
		t.Fatalf("unsupervised mismatch = %v", err)
	}
	if errors.Is(err, daemon.ErrDaemonUnavailable) || daemon.ShouldFallBackToEmbedded(err) {
		t.Fatal("unsupported graceful handoff must not masquerade as an absent daemon")
	}
}
