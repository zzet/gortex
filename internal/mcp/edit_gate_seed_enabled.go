//go:build gortex_edit_gate_seed

package mcp

import (
	"context"
	"os"
	"time"
)

// Only the dedicated test build contains this switch. The caller owns the
// existing reindex-wait lap; this helper must never introduce another lap.
var editGateSeedACKEntered func()

func editGateACKSeed(ctx context.Context, s *Server) error {
	if os.Getenv("GORTEX_EDIT_GATE_SEED") != "ack-wait" {
		return nil
	}
	resumeIntent := suspendSourceMutationWriteIntent(ctx)
	defer resumeIntent()
	// The synthetic legacy wait yields its existing dispatch permit, just
	// like an ordinary publication wait. It owns no writer or extra lap.
	waiting := make(chan struct{})
	defer close(waiting)
	s.releaseDispatchSlotForWait(ctx, waiting)
	if editGateSeedACKEntered != nil {
		editGateSeedACKEntered()
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// The checkout adapter fast-ACKs an admitted ticket without a normal graph
// wait. Only a seed build records the deliberately restored legacy wait here.
// When admission/await are split, this belongs after the source path is released.
func editGateCheckoutACKSeed(ctx context.Context, s *Server) error {
	if os.Getenv("GORTEX_EDIT_GATE_SEED") != "ack-wait" {
		return nil
	}
	lap := s.beginAcknowledgementLap(ctx, editAckReindexWait)
	defer lap.finish()
	return editGateACKSeed(ctx, s)
}
