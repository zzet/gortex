package indexer

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The deferred retirement backlog, as a level.
//
// The retired counter says how much the sweep removed and the owed counter how
// much became debt; neither says how much is waiting now, how old the oldest
// eligible debt is, or when the sweep last finished anything. A sweep that
// cannot keep pace looked, from daemon status, exactly like one with nothing
// to do. RetirementBacklog is that level: small, id-free, and cheap enough to
// ride on every status poll.

// RetirementBacklog is the deferred retirement sweep's outstanding work.
type RetirementBacklog struct {
	// Generations is how many generations are owed a retirement: every
	// coordinator's backlog and the lifecycle's owed set, parked ones
	// included.
	Generations int `json:"generations"`
	// Retiring is how many catalog generations are fenced and part-swept.
	Retiring int `json:"retiring"`
	// Parked is how many owed generations wait for a reference to go.
	Parked int `json:"parked"`
	// BytesEstimate is the stored payload size the owed generations recorded
	// at publication (a generation that never published counts zero).
	BytesEstimate int64 `json:"bytes_estimate"`
	// DebtAgeSeconds is how long eligible debt has been waiting without the
	// backlog emptying (0 when none is).
	DebtAgeSeconds float64 `json:"debt_age_s"`
	// LastRemovedAt is when the sweep last removed a generation (Unix
	// seconds, 0 when it has not since the daemon started).
	LastRemovedAt int64 `json:"last_removed_at"`
}

// Empty reports a backlog with nothing in it, which status omits.
func (b RetirementBacklog) Empty() bool {
	return b.Generations == 0 && b.Retiring == 0 && b.Parked == 0
}

// RetirementBacklog reads the deferred retirement backlog: in-memory owed
// work, plus one catalog listing for the retiring count and the sizes.
func (l *CheckoutLifecycle) RetirementBacklog(ctx context.Context) (RetirementBacklog, error) {
	if l == nil || l.catalog == nil {
		return RetirementBacklog{}, errNoCatalog
	}
	rows, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{})
	if err != nil {
		return RetirementBacklog{}, err
	}
	return l.retirementBacklogFrom(rows, time.Now()), nil
}

// retirementBacklogFrom computes the backlog against a catalog listing the
// caller already holds.
func (l *CheckoutLifecycle) retirementBacklogFrom(rows []store_sqlite.ViewGeneration, now time.Time) RetirementBacklog {
	var out RetirementBacklog
	if l == nil {
		return out
	}
	// Parks are counted against the same owed set, so a park whose
	// generation another path retired is not reported as parked work.
	owed := l.owedRetirementSet()
	out.Generations = len(owed)
	out.Parked = l.pruneRetirementParked(owed)
	for _, row := range rows {
		if row.State == store_sqlite.ViewGenerationRetiring {
			out.Retiring++
		}
		if _, ok := owed[row.GenerationID]; ok {
			out.BytesEstimate += row.StorageBytes
		}
	}
	if since := l.deferredRetirementEligibleSince.Load(); since != 0 {
		out.DebtAgeSeconds = now.Sub(time.Unix(0, since)).Seconds()
	}
	out.LastRemovedAt = l.deferredRetirementLastRemoved.Load()
	return out
}

// noteRetirementWALPause counts a pass or burst the WAL pause held back, and
// says so once per pause: before this the sweep stopped for the reclaim with
// no counter and no log line.
func (l *CheckoutLifecycle) noteRetirementWALPause(walBytes int64) {
	noteRetirementStandDown("wal_pause")
	if l.retirementWALPaused.CompareAndSwap(false, true) && l.logger != nil {
		l.logger.Info("indexer: deferred retirement paused until the WAL reclaim resets the log",
			zap.Int64("wal_bytes", walBytes), zap.Int64("pause_bytes", deferredRetirementWALPause))
	}
}

// retireOwedClass maps deferRetire's reason onto the owed counter's bounded
// vocabulary.
func retireOwedClass(why string) string {
	switch {
	case strings.HasPrefix(why, "torn"):
		return viewmetrics.OwedTorn
	case strings.HasPrefix(why, "failed"):
		return viewmetrics.OwedFailed
	default:
		return viewmetrics.OwedReleased
	}
}
