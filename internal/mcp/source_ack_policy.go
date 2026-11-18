package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/config"
)

// A value snapshot belongs to the source operation, including every physical
// file and publication admission in a frontier. Configuration cannot change it
// between writes or while an admitted ticket is completing.
type sourceAckPolicy struct {
	owner     *Server
	mode      config.EditAckMode
	freshness requestFreshness
}

type sourceAckPolicyKey struct{}

func (s *Server) sourceAckPolicy(ctx context.Context) sourceAckPolicy {
	if policy, ok := ctx.Value(sourceAckPolicyKey{}).(sourceAckPolicy); ok && policy.owner == s {
		return policy
	}
	mode := s.editAckMode
	if mode == "" {
		mode = config.EditAckFull
	}
	return sourceAckPolicy{owner: s, mode: mode}
}

func (p sourceAckPolicy) local() bool {
	return p.mode == config.EditAckLocal && !p.freshness.requireFresh
}

// Explicit tier waits reuse the single freshness producer. They never turn a
// file/relations stamp into whole-patch completion, and own no source locks.
func (s *Server) awaitSourceAckTier(ctx context.Context, policy sourceAckPolicy, paths []string, published <-chan struct{}) error {
	scope := policy.freshness.scope
	if scope == "" {
		scope = "file"
	}
	deadline := policy.freshness.effectiveDeadline(time.Now(), ctx)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	resumeIntent := suspendSourceMutationWriteIntent(ctx)
	defer resumeIntent()
	label := s.freshnessFor(waitCtx, scope, nil, paths)
	if label.state == "" || label.state == "failed" || !deadline.After(time.Now()) {
		return editGateCheckoutACKSeed(ctx, s)
	}
	lap := s.beginAcknowledgementLap(ctx, editAckReindexWait)
	defer lap.finish()
	if err := editGateACKSeed(ctx, s); err != nil {
		return err
	}
	if !s.releaseDispatchSlotForWait(waitCtx, published) {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		label = s.freshnessFor(waitCtx, scope, nil, paths)
		if label.state == "" || label.state == "failed" {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
