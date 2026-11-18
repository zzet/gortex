package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
)

func checkoutBatchHash(t *testing.T, path string) string {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

func TestCheckoutBatchRefreshBindsWholeSetAndCompletesAfterLeaseRelease(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc BatchedHelper() {}\n")
	builderWriteFile(t, f.worktree, "extra.go", "package fixture\n\nfunc BatchExtra() {}\n")
	files := []CheckoutBatchFile{{Path: filepath.Join(f.worktree, "helper.go"), SHA256: checkoutBatchHash(t, filepath.Join(f.worktree, "helper.go"))}, {Path: filepath.Join(f.worktree, "extra.go"), SHA256: checkoutBatchHash(t, filepath.Join(f.worktree, "extra.go"))}}
	committedCtx, cancel := context.WithCancel(t.Context())
	cancel()
	// A completed disk transaction must publish even after its client cancels.
	ticket, err := m.EnqueueBatchRefresh(committedCtx, files)
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	if ticket.CheckoutID != f.checkoutID || ticket.Ticket.Path != f.worktree {
		t.Fatalf("wrong root authority: %+v", ticket)
	}
	if _, err := m.EnqueueBatchRefresh(t.Context(), files); !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("second admission: %v", err)
	}
	m.Close()
	through := c.checkoutRefreshHighWater()
	out := c.reconcile(t.Context())
	c.completeCheckoutRefreshTickets(t.Context(), through, out)
	result := <-ticket.Ticket.Done
	if !result.Reindexed || result.Err != nil || result.AppliedGeneration == 0 {
		t.Fatalf("batch did not publish: %+v cycle=%+v", result, out)
	}
}

func TestCheckoutBatchRefreshRejectsChangedSecondFileAndEscapingAbsence(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc BatchHelper() {}\n")
	path := filepath.Join(f.worktree, "helper.go")
	for _, files := range [][]CheckoutBatchFile{
		{{Path: path, SHA256: checkoutBatchHash(t, path)}, {Path: path, SHA256: "wrong"}},
		{{Path: filepath.Join(f.primary, "absent.go"), Absent: true}},
		{{Path: filepath.Join(f.worktree, ".git", "absent"), Absent: true}},
	} {
		if _, err := m.EnqueueBatchRefresh(t.Context(), files); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
			t.Fatalf("unguarded set admitted: %v", err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.worktree, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnqueueBatchRefresh(t.Context(), []CheckoutBatchFile{{Path: filepath.Join(f.worktree, "escape", "absent.go"), Absent: true}}); !errors.Is(err, ErrCheckoutRefreshSuperseded) {
		t.Fatalf("escaping absence admitted: %v", err)
	}
}

func TestCheckoutBatchRefreshRejectsSupersededOutputAuthority(t *testing.T) {
	f, l, authority := newCheckoutMutationAuthorityFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	newer, err := authority.Begin(t.Context(), OutputEntryCheckoutSourceMutation, m.Receipt().Target())
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Abandon()
	if _, err := m.EnqueueBatchRefresh(t.Context(), nil); !errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("superseded batch admitted: %v", err)
	}
}

func TestCheckoutBatchRecoveryValidatesJournalIdentityAndPreservesRouteUntilPrepared(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	generation, _, _, _ := m.BatchAuthority()
	id, inc := m.Identity()
	m.Close()
	before := f.route()
	rootIdentity := gitstate.SamplePathEvidence(f.worktree).RootIdentity
	for _, bad := range []struct{ inc, root string }{{"new-incarnation", rootIdentity}, {inc, "replaced-root"}} {
		recovered, err := l.BeginCheckoutRecovery(t.Context(), id, bad.inc, f.worktree, bad.root, generation, "", "", "")
		if recovered != nil {
			recovered.Close()
		}
		if !errors.Is(err, ErrCheckoutMutationStale) {
			t.Fatalf("identity mismatch admitted: %v", err)
		}
	}
	recovered, err := l.BeginCheckoutRecovery(t.Context(), id, inc, f.worktree, rootIdentity, generation, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.route() != before {
		t.Fatal("recovery admission changed route before preflight")
	}
	recovered.Close()
	if f.route() != before {
		t.Fatal("recovery without write changed route")
	}
}

func TestCheckoutBatchRecoveryAdmissionPanicReleasesResources(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	checkout, _, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	catalog := c.catalog
	c.catalog = nil
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		m, _ := l.BeginCheckoutRecovery(context.Background(), f.checkoutID, checkout.Incarnation, f.worktree, gitstate.SamplePathEvidence(f.worktree).RootIdentity, f.route().DirtyGenerationID, "", "", "")
		if m != nil {
			m.Close()
		}
	}()
	c.catalog = catalog
	if panicked == nil {
		t.Fatal("fault injection did not panic")
	}
	if !c.cycleMu.TryLock() {
		t.Fatal("panic stranded cycle lease")
	}
	c.cycleMu.Unlock()
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	if active != 0 {
		t.Fatalf("panic leaked %d source admissions", active)
	}
}
