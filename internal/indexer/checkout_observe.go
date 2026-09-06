package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

const checkoutObservationTimeout = 250 * time.Millisecond

// ObserveCheckoutPath gives a first request in a newly added worktree its
// automatic identity. Only an already known Git family can be observed: this
// is not explicit tracking, and never creates a dedicated graph or config
// entry. Metadata work has a short context budget; any resulting graph build
// belongs to the lifecycle's existing asynchronous activation path.
//
// found=false, err=nil means the path has no known family to serve it from.
// A busy/error outcome carries no permission to fall through to another graph.
// Explicit selectors must provide an authorizer for the known primary prefix;
// it runs outside locks and before any catalog observation or build activation.
func (l *CheckoutLifecycle) ObserveCheckoutPath(ctx context.Context, path string, authorize ...func(string) error) (checkout store_sqlite.Checkout, found bool, err error) {
	if l == nil || l.catalog == nil || l.rec == nil || path == "" || !filepath.IsAbs(path) {
		return checkout, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutObservationTimeout)
	defer cancel()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
		}
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || retryableCheckoutRefreshError(err)) {
			err = fmt.Errorf("%w: selected checkout discovery is pending: %w", ErrCheckoutMutationBusy, err)
		}
	}()
	l.coordMu.Lock()
	closing := l.coordinatorClosing
	l.coordMu.Unlock()
	if closing {
		return checkout, false, ErrCheckoutRefreshStopped
	}
	inv, err := gitstate.Inventory(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return checkout, false, ctx.Err()
		}
		return checkout, false, nil // Not a Git checkout; ordinary scope resolution may continue.
	}
	familyID := FamilyIDFor(inv.CommonDir)
	family, known, err := l.catalog.GetRepositoryFamily(ctx, familyID)
	if err != nil || !known {
		return checkout, false, err
	}
	if !sameMutationRoot(family.CommonDirIdentity, inv.CommonDir) {
		return checkout, false, ErrCheckoutMutationStale
	}
	var selected *gitstate.WorktreeRecord
	for i := range inv.Records {
		record := &inv.Records[i]
		gitDir := inv.CommonDir
		if !record.IsMain {
			gitDir = filepath.Join(inv.CommonDir, "worktrees", record.AdminName)
		}
		if !record.Bare && record.AdminName != "" && sameMutationRoot(gitDir, inv.GitDir) && checkoutObservationContains(path, record.Path) {
			selected = record
			break
		}
	}
	if selected == nil {
		return checkout, false, fmt.Errorf("%w: Git did not identify the selected working copy", ErrCheckoutMutationStale)
	}
	primary, err := l.observationPrimary(ctx, familyID)
	if err != nil {
		return checkout, false, err
	}
	for _, allow := range authorize {
		if allow != nil {
			if err := allow(primary.RepoPrefix); err != nil {
				return checkout, false, err
			}
		}
	}
	entry, err := l.rec.ObserveCheckout(ctx, familyID, selected.Path, inv)
	if err != nil {
		return checkout, false, err
	}
	if !entry.Durable || entry.CheckoutID == "" {
		return checkout, false, fmt.Errorf("%w: %s", ErrCheckoutNotTracked, entry.Detail)
	}
	checkout, found, err = l.catalog.GetCheckout(ctx, entry.CheckoutID)
	if err != nil || !found {
		return checkout, found, err
	}
	if checkout.Incarnation != entry.Incarnation || !sameMutationRoot(checkout.RootPath, selected.Path) {
		return store_sqlite.Checkout{}, false, ErrCheckoutMutationStale
	}
	currentPrimary, err := l.observationPrimary(ctx, familyID)
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	if currentPrimary.GraphID != primary.GraphID || currentPrimary.RepoPrefix != primary.RepoPrefix {
		return store_sqlite.Checkout{}, false, ErrCheckoutMutationStale
	}
	if checkout.State == store_sqlite.CheckoutStateReady && checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic {
		l.ActivateCheckout(checkout.CheckoutID, "first request observed checkout")
	}
	return checkout, true, nil
}

func (l *CheckoutLifecycle) observationPrimary(ctx context.Context, familyID string) (store_sqlite.DedicatedGraph, error) {
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return store_sqlite.DedicatedGraph{}, err
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase && graph.GraphID != "" {
			return graph, nil
		}
	}
	return store_sqlite.DedicatedGraph{}, fmt.Errorf("%w: known family has no primary graph", ErrCheckoutNotTracked)
}

// Verify both lexical containment and physical root identity. Case folding is
// an identity policy, not proof that two differently spelled directories on a
// case-sensitive volume are the same checkout.
func checkoutObservationContains(path, root string) bool {
	path, root = pathkey.CanonicalExistingRoot(path), pathkey.CanonicalExistingRoot(root)
	if !pathkey.HasPathPrefix(path, root) {
		return false
	}
	for !pathkey.EqualPaths(path, root) {
		parent := filepath.Dir(path)
		if pathkey.EqualPaths(parent, path) {
			return false
		}
		path = parent
	}
	selected, selectedErr := os.Stat(root)
	matched, matchedErr := os.Stat(path)
	return selectedErr == nil && matchedErr == nil && selected.IsDir() && os.SameFile(selected, matched)
}
