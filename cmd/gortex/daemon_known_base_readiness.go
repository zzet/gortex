package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/indexer"
)

type startupBaseJoiner interface {
	JoinScheduled(context.Context) ([]indexer.InitialBasePublication, error)
}

type knownBasePreparation struct {
	published   int
	ownerOnly   int
	uncommitted int
	failed      int
	interrupted bool
}

func (r knownBasePreparation) outcome(err error) string {
	if err != nil {
		return "incomplete"
	}
	if r.published == 0 && r.uncommitted > 0 {
		return "skipped_uncommitted"
	}
	if r.published == 0 && r.ownerOnly > 0 {
		return "skipped_owner_only"
	}
	if r.published == 0 {
		return "not_needed"
	}
	return "prepared"
}

// joinKnownStartupBases does not schedule work or change core readiness. An
// attempted publication is not necessarily a prepared base. Any failure releases
// linked builds for the existing request/recovery path rather than making this
// optional prewarm a permanent readiness prerequisite. Successful preparation
// leaves the gate closed until the original warmup tail opens it.
func joinKnownStartupBases(ctx context.Context, publisher startupBaseJoiner, gate *indexer.ViewBuildGate) (knownBasePreparation, error) {
	var report knownBasePreparation
	if publisher == nil {
		return report, nil
	}
	outcomes, err := publisher.JoinScheduled(ctx)
	report.interrupted = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	for _, out := range outcomes {
		switch {
		case out.Err != nil:
			report.failed++
			err = errors.Join(err, fmt.Errorf("committed base %s: %w", out.RepoPrefix, out.Err))
		case out.Skipped == "no dependent checkout":
			report.ownerOnly++
		case out.Skipped == "owner has no committed tree":
			report.uncommitted++
		case out.Skipped != "" || out.GenerationID <= 0:
			report.failed++
			err = errors.Join(err, fmt.Errorf("committed base %s not prepared: skip=%q generation=%d", out.RepoPrefix, out.Skipped, out.GenerationID))
		default:
			report.published++
		}
	}
	if err != nil {
		gate.Open()
	}
	return report, err
}
