package reconcile

import (
	"context"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The outermost nested saga notifies only after its final phase/journal write
// has returned. A drain that finishes inside ReleaseGraph cannot race a retry
// against that same attempt's later failed-phase persistence.
type repositoryCleanupAttemptKey struct{}
type repositoryCleanupAttempt struct {
	owner    *Reconciler
	mu       sync.Mutex
	graphs   map[string]struct{}
	finishes map[string]func()
}

// A captured callback belongs to one concrete lifecycle state, not whichever
// registration happens to occupy its graph ID when the outer saga returns.
// A nonnil callback returned with an initialization error is still retained:
// the durable failed phase must wake that exact state's repair path.
type repositoryCleanupAttemptHooks interface {
	BeginGraphCleanupAttempt(context.Context, string) (func(), error)
}

func (r *Reconciler) repositoryCleanupAttempt(ctx context.Context, graphID string) (context.Context, func()) {
	if current, ok := ctx.Value(repositoryCleanupAttemptKey{}).(*repositoryCleanupAttempt); ok && current.owner == r {
		current.add(graphID)
		return ctx, func() {}
	}
	attempt := &repositoryCleanupAttempt{owner: r, graphs: make(map[string]struct{})}
	attempt.add(graphID)
	ctx = context.WithValue(ctx, repositoryCleanupAttemptKey{}, attempt)
	return ctx, func() {
		attempt.mu.Lock()
		graphs := make([]string, 0, len(attempt.graphs))
		for id := range attempt.graphs {
			graphs = append(graphs, id)
		}
		finishes := make(map[string]func(), len(attempt.finishes))
		for id, finish := range attempt.finishes {
			finishes[id] = finish
		}
		attempt.mu.Unlock()
		sort.Strings(graphs)
		if _, captured := r.hooks.(repositoryCleanupAttemptHooks); captured {
			for _, id := range graphs {
				if finish := finishes[id]; finish != nil {
					finish()
				}
			}
			return
		}
		notifier, ok := r.hooks.(interface{ RepositoryCleanupAttemptFinished([]string) })
		if !ok {
			return
		}
		notifier.RepositoryCleanupAttemptFinished(graphs)
	}
}

func (a *repositoryCleanupAttempt) add(graphID string) {
	if graphID == "" {
		return
	}
	a.mu.Lock()
	a.graphs[graphID] = struct{}{}
	a.mu.Unlock()
}

// Optional for legacy hook implementations. Persisting the current phase
// precedes closure, so any interrupted local fencing is owned by the journal.
// Closing at the first phase also fences producers before dependent checkout
// teardown, rather than waiting until the last graph-release phase.
func (r *Reconciler) beginRepositoryCleanup(ctx context.Context, graphID string) error {
	if graphID == "" {
		return nil
	}
	if hook, ok := r.hooks.(repositoryCleanupAttemptHooks); ok {
		finish, err := hook.BeginGraphCleanupAttempt(ctx, graphID)
		if attempt, ok := ctx.Value(repositoryCleanupAttemptKey{}).(*repositoryCleanupAttempt); ok && attempt.owner == r && finish != nil {
			attempt.mu.Lock()
			if attempt.finishes == nil {
				attempt.finishes = make(map[string]func())
			}
			if attempt.finishes[graphID] == nil {
				attempt.finishes[graphID] = finish
			}
			attempt.graphs[graphID] = struct{}{}
			attempt.mu.Unlock()
		}
		return err
	}
	hook, ok := r.hooks.(interface {
		BeginGraphCleanup(context.Context, string) error
	})
	if !ok {
		return nil
	}
	return hook.BeginGraphCleanup(ctx, graphID)
}

// RepositoryCleanupPending checks the authoritative journal, including outer
// primary/family sagas and the demotion transition that outlives graph release.
// A graph-row deletion alone does not authorize reopening its owner prefix.
func (r *Reconciler) RepositoryCleanupPending(ctx context.Context, graphID, checkoutID, incarnation, familyID string) (bool, error) {
	entries, err := r.catalog.ListCleanupEntries(ctx)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Phase == store_sqlite.CleanupPhaseDone {
			continue
		}
		target, err := decodeSagaTarget(entry)
		if err != nil {
			return false, err
		}
		if target.GraphID == graphID && graphID != "" {
			return true, nil
		}
		if target.CheckoutID == checkoutID && target.Incarnation == incarnation && checkoutID != "" {
			return true, nil
		}
		if target.FamilyID == familyID && familyID != "" && (target.Kind == sagaForgetFamily || target.Kind == sagaRetirePrimaryClosure) {
			return true, nil
		}
	}
	if checkoutID != "" {
		checkout, found, err := r.catalog.GetCheckout(ctx, checkoutID)
		if err != nil {
			return false, err
		}
		if found && checkout.Incarnation == incarnation && checkout.ActiveIntentTransitionID != "" {
			return true, nil
		}
	}
	return false, nil
}
