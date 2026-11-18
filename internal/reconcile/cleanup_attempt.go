package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func newCleanupAttemptID() (string, error) {
	var id [24]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("cleanup attempt identity: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// Only graph-owning teardown needs this new execution protocol. Layer purge
// and family-only journal behavior are deliberately unchanged.
func cleanupNeedsAttempt(kind sagaKind) bool {
	return kind == sagaForgetCheckout || kind == sagaRetireGraph || kind == sagaRetirePrimaryClosure
}

// prepareCleanupExecution creates a new generic owner-teardown journal once,
// upgrades an exact legacy journal snapshot, or uses the token already returned
// by explicit authorization. It then serializes execution and reloads progress
// under the token before any phase hook. No waiter holds a SQL transaction.
func (r *Reconciler) prepareCleanupExecution(ctx context.Context, target sagaTarget) (sagaTarget, func(), error) {
	if !cleanupNeedsAttempt(target.Kind) {
		return target, func() {}, nil
	}
	if target.AttemptID == "" {
		attemptID, err := newCleanupAttemptID()
		if err != nil {
			return target, nil, err
		}
		var entry store_sqlite.CleanupEntry
		if target.journalSnapshot != nil {
			entry, err = r.catalog.UpgradeCleanupAttempt(ctx, *target.journalSnapshot, attemptID)
		} else {
			target.AttemptID = attemptID
			var payload []byte
			payload, err = json.Marshal(target)
			if err == nil {
				entry = store_sqlite.CleanupEntry{CleanupID: target.cleanupID(), OpaqueTargetIDs: string(payload), Reason: string(target.Kind), Phase: store_sqlite.CleanupPhasePending, PrimaryEpoch: target.PrimaryEpoch, LastProgress: r.now().Unix()}
				err = r.catalog.CreateCleanupAttempt(ctx, entry, attemptID)
			}
		}
		if err != nil {
			return target, nil, err
		}
		target, err = decodeSagaTarget(entry)
		if err != nil {
			return target, nil, err
		}
	}
	release, err := cleanupExecutions.acquire(ctx, target.cleanupID())
	if err != nil {
		return target, nil, err
	}
	entry, err := r.catalog.GetCleanupAttempt(ctx, target.cleanupID(), target.AttemptID)
	if err != nil {
		release()
		return target, nil, err
	}
	current, err := decodeSagaTarget(entry)
	if err != nil {
		release()
		return target, nil, err
	}
	if current.cleanupID() != target.cleanupID() || current.AttemptID != target.AttemptID || current.Kind != target.Kind {
		release()
		return target, nil, fmt.Errorf("%w: cleanup execution identity changed", store_sqlite.ErrCatalogStaleGuard)
	}
	return current, release, nil
}

func (r *Reconciler) persistCleanupEntry(ctx context.Context, entry store_sqlite.CleanupEntry, target sagaTarget) error {
	if target.AttemptID != "" {
		return r.catalog.AdvanceCleanupAttempt(ctx, entry, target.AttemptID)
	}
	return r.catalog.UpsertCleanupEntry(ctx, entry)
}
