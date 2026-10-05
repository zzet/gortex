package indexer

import (
	"context"
	"fmt"
	"github.com/zzet/gortex/internal/pathkey"
	"os"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// CheckoutBatchFile binds every written or removed entry to one publication.
type CheckoutBatchFile struct {
	Path, SHA256 string
	Absent       bool
}

func validateCheckoutBatchFiles(ctx context.Context, root string, rootInfo os.FileInfo, files []CheckoutBatchFile) error {
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.Absent {
			// The rooted hash helper validates physical root and every .git component
			// even when opening the requested (absent) entry returns ENOENT.
			_, _, err := checkoutRefreshFileHash(ctx, root, rootInfo, file.Path)
			if !os.IsNotExist(err) {
				return fmt.Errorf("%w: %s must be absent in captured checkout", ErrCheckoutRefreshSuperseded, file.Path)
			}
			if _, err := os.Lstat(file.Path); !os.IsNotExist(err) {
				return fmt.Errorf("%w: %s must be absent", ErrCheckoutRefreshSuperseded, file.Path)
			}
		} else {
			_, hash, err := checkoutRefreshFileHash(ctx, root, rootInfo, file.Path)
			if err != nil || hash != file.SHA256 {
				return fmt.Errorf("%w: batch content changed at %s", ErrCheckoutRefreshSuperseded, file.Path)
			}
		}
	}
	return nil
}

// EnqueueBatchRefresh captures one snapshot for the complete transaction.
func (m *CheckoutMutation) EnqueueBatchRefresh(ctx context.Context, files []CheckoutBatchFile) (*CheckoutRefreshTicket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.prepared || !m.refreshReserved || m.fresh || m.refreshQueued {
		return nil, ErrCheckoutMutationStale
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.receiptStillCurrent(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkoutRefreshCaptureTimeout)
	defer cancel()
	ctx, lifetimeCancel := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer lifetimeCancel()
	if err := m.validateCheckout(ctx); err != nil {
		return nil, err
	}
	if err := validateCheckoutBatchFiles(ctx, m.checkout.RootPath, m.rootInfo, files); err != nil {
		return nil, err
	}
	request, err := m.coordinator.captureCheckoutRefresh(gitstate.WithUrgentSample(ctx), m.checkout, m.rootInfo, "")
	if err != nil {
		return nil, err
	}
	if request.headRef != m.headRef || request.headCommit != m.headCommit || request.headTree != m.headTree {
		return nil, ErrCheckoutRefreshSuperseded
	}
	if err := validateCheckoutBatchFiles(ctx, m.checkout.RootPath, m.rootInfo, files); err != nil {
		return nil, err
	}
	if err := m.receiptStillCurrent(); err != nil {
		return nil, err
	}
	request.batchFiles = append([]CheckoutBatchFile(nil), files...)
	ticket, err := m.coordinator.enqueueCheckoutRefresh(request, true)
	m.refreshReserved = false
	if err == nil {
		m.refreshQueued = true
	}
	return ticket, err
}

// BeginCheckoutRecovery serializes a durable journal with its original checkout,
// including when the journal's interrupted commit left its graph route pending.
// It grants no symbol-range authority and must only restore journal before-images
// or enqueue publication of proven after-images.
func (l *CheckoutLifecycle) BeginCheckoutRecovery(ctx context.Context, id, incarnation, root, rootIdentity string, generation int64, headRef, headCommit, headTree string) (*CheckoutMutation, error) {
	ctx, c, checkout, rootInfo, cancel, err := l.checkoutRefreshTarget(ctx, id, root)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if incarnation == "" || checkout.Incarnation != incarnation || rootIdentity == "" || gitstate.SamplePathEvidence(pathkey.CanonicalExistingRoot(checkout.RootPath)).RootIdentity != rootIdentity {
		return nil, fmt.Errorf("%w: durable checkout identity changed", ErrCheckoutMutationStale)
	}
	if !c.admitSourceMutation() {
		return nil, ErrCheckoutRefreshStopped
	}
	if err := acquireCycleLock(ctx, c); err != nil {
		c.releaseSourceMutation()
		return nil, err
	}
	m := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo}
	owned := true
	defer func() {
		if owned {
			m.Close()
		}
	}()
	fail := func(err error) (*CheckoutMutation, error) { m.Close(); return nil, err }
	if err := m.validateCheckout(ctx); err != nil {
		return fail(err)
	}
	sample, err := c.sampler.SampleSince(ctx, time.Now())
	if err != nil {
		return fail(err)
	}
	if headCommit != "" && (sample.HeadRef != headRef || sample.HeadCommit != headCommit || sample.HeadTree != headTree) {
		return fail(fmt.Errorf("%w: journal checkout HEAD changed", ErrCheckoutMutationStale))
	}
	m.headRef, m.headCommit, m.headTree = sample.HeadRef, sample.HeadCommit, sample.HeadTree
	route, found, err := c.catalog.GetCheckoutRoute(ctx, id)
	if err != nil {
		return fail(err)
	}
	if generation <= 0 {
		return fail(fmt.Errorf("%w: journal has no admitted output generation", ErrCheckoutMutationStale))
	}
	if found && route.DirtyGenerationID > 0 {
		generation = route.DirtyGenerationID
	}
	m.receipt, err = lifecycleOutputAuthority(l).Begin(ctx, OutputEntryCheckoutSourceMutation, OutputMutationTarget{Kind: OutputGenerationCheckout, OwnerKey: "checkout:" + id, CheckoutID: id, Incarnation: incarnation, Generation: generation})
	if err != nil {
		return fail(err)
	}
	if err := m.receiptStillCurrent(); err != nil {
		return fail(err)
	}
	m.route = route
	m.recovery = true
	owned = false
	return m, nil
}

// BatchAuthority returns the admitted output generation and pinned HEAD for the durable journal.
func (m *CheckoutMutation) BatchAuthority() (generation int64, headRef, headCommit, headTree string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.receipt != nil {
		generation = m.receipt.Target().Generation
	}
	return generation, m.headRef, m.headCommit, m.headTree
}

// PrepareRecovery withdraws a journal's checkout route only after all of its
// paths and before/after disk states have passed recovery preflight.
func (m *CheckoutMutation) PrepareRecovery(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prepared {
		return nil
	}
	if !m.recovery || m.closed || m.refreshQueued {
		return ErrCheckoutMutationStale
	}
	if err := m.receiptStillCurrent(); err != nil {
		return err
	}
	ctx, cancel := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer cancel()
	if err := m.validateCheckout(ctx); err != nil {
		return err
	}
	sample, err := m.coordinator.sampler.SampleSince(ctx, time.Now())
	if err != nil {
		return err
	}
	if sample.HeadRef != m.headRef || sample.HeadCommit != m.headCommit || sample.HeadTree != m.headTree {
		return ErrCheckoutMutationStale
	}
	c := m.coordinator
	if err := c.reserveCheckoutRefresh(); err != nil {
		return err
	}
	m.refreshReserved = true
	if m.route.State == store_sqlite.RouteActive {
		withdrawn := c.holdWithdrawnDirty(ctx, m.route)
		if err := c.clearDirtySlot(ctx, &m.route); err != nil {
			c.releaseCheckoutRefreshReservation()
			m.refreshReserved = false
			return err
		}
		c.setPreferredDirtyParent(withdrawn, "durable batch recovery")
	}
	m.prepared = true
	return nil
}
