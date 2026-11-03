package main

import (
	"context"
	"strings"
	"testing"
)

func TestSmokeAcceptsExplicitACKPolicyBeforeOwnedInputs(t *testing.T) {
	for _, mode := range []string{"local", "full"} {
		err := smokeCommand(context.Background(), []string{"--edit-ack", mode})
		if err == nil || !strings.Contains(err.Error(), "version 2 smoke requires fixed two-build inputs") {
			t.Fatalf("explicit policy rejected before normal input validation: mode=%s err=%v", mode, err)
		}
	}
}

func TestSmokeRejectsUnknownExplicitACKPolicyBeforeOwnedInputs(t *testing.T) {
	err := smokeCommand(context.Background(), []string{"--edit-ack", "unknown"})
	if err == nil || !strings.Contains(err.Error(), "smoke edit ACK mode") {
		t.Fatalf("unknown policy did not fail closed at selection: %v", err)
	}
}
