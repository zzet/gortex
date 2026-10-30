package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This reproduces the observed boundary without a product latch: admission
// succeeds, then an ordinary different-tree HEAD move is refused by Prepare.
func latePrepareRefusal(t *testing.T) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle, *CheckoutMutation, string, error) {
	t.Helper()
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	head := builderGit(t, f.worktree, "rev-parse", "HEAD")
	before, err := os.ReadFile(filepath.Join(f.worktree, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	index := builderGit(t, f.worktree, "ls-files", "--stage")
	builderWriteFile(t, f.primary, "independent-motion.txt", "B\n")
	builderGit(t, f.primary, "add", "independent-motion.txt")
	builderGit(t, f.primary, "commit", "-m", "independent tree B")
	next := builderGit(t, f.primary, "rev-parse", "HEAD")
	builderGit(t, f.worktree, "reset", "--soft", next)
	err = m.Prepare(t.Context())
	if !errors.Is(err, ErrCheckoutMutationStale) || !strings.Contains(err.Error(), "HEAD changed") {
		t.Fatalf("real precommit HEAD refusal = %v", err)
	}
	var proof *checkoutMutationRefusalError
	if !errors.As(err, &proof) || proof.witness.rootInfo == nil || proof.witness.route != m.route {
		t.Fatalf("active lease lost exact precommit proof: %v", err)
	}
	if after, readErr := os.ReadFile(filepath.Join(f.worktree, "helper.go")); readErr != nil || string(after) != string(before) {
		t.Fatalf("Prepare changed source: %q, %v", after, readErr)
	}
	if after := builderGit(t, f.worktree, "ls-files", "--stage"); after != index {
		t.Fatal("Prepare changed index")
	}
	if m.prepared || f.route() != m.route {
		t.Fatal("precommit refusal withdrew the route")
	}
	return f, c, l, m, head, err
}

func TestCheckoutMutationPrepareRefusalCarriesProofWithoutPublishing(t *testing.T) {
	f, _, l, m, head, err := latePrepareRefusal(t)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("Prepare alone published policy without a whole-operation no-bytes proof")
	}
	if !l.RecordCheckoutMutationRefusal(t.Context(), err) {
		t.Fatal("authenticated final precommit refusal was not recorded")
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "read_only" {
		t.Fatalf("indexed target state = %q", state)
	}
	for _, target := range []struct{ id, root string }{{f.primaryID, f.primary}, {f.checkoutID, f.primary}, {"unrelated", f.worktree}} {
		if _, marked := l.CheckoutMutationRefusal(t.Context(), target.id, target.root); marked {
			t.Fatal("precommit refusal escaped its checkout")
		}
	}
	m.Close()
	builderGit(t, f.worktree, "reset", "--soft", head)
	next, admitErr := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if admitErr != nil {
		t.Fatal(admitErr)
	}
	defer next.Close()
	if l.RecordCheckoutMutationRefusal(t.Context(), err) {
		t.Fatal("old lease overwrote newer successful admission")
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "ready" {
		t.Fatalf("recovery did not clear precommit refusal: %q", state)
	}
}

func TestCheckoutMutationPrepareRefusalRejectsJoinedCancellationAndTransient(t *testing.T) {
	f, _, l, _, _, err := latePrepareRefusal(t)
	ioFailure := &checkoutMutationSampleFailure{err: fmt.Errorf("%w: sample checkout: %w", ErrCheckoutMutationStale, os.ErrPermission)}
	for _, excluded := range []error{context.Canceled, context.DeadlineExceeded, ErrCheckoutMutationBusy, ErrCheckoutMutationPending, ErrCheckoutByteAnchorUnavailable, ioFailure} {
		if l.RecordCheckoutMutationRefusal(t.Context(), errors.Join(err, excluded)) {
			t.Fatalf("joined %v authorized fallback", excluded)
		}
	}
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("excluded errors left a refusal mark")
	}
}

func TestCheckoutMutationPrepareSampleFailureHasNoRefusalProof(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	gitFile := filepath.Join(f.worktree, ".git")
	original, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if restoreErr := os.WriteFile(gitFile, original, 0o644); restoreErr != nil {
			t.Errorf("restore Git entry: %v", restoreErr)
		}
	}()
	// The admitted physical root/catalog still match, but Git cannot sample
	// this checkout. Unknown source state must not authorize native fallback.
	if err := os.WriteFile(gitFile, []byte("gitdir: missing-git-admin-directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.Prepare(t.Context())
	var failedSample *checkoutMutationSampleFailure
	if !errors.Is(err, ErrCheckoutMutationStale) || !errors.As(err, &failedSample) {
		t.Fatalf("sampling failure lost its existing Stale contract: %v", err)
	}
	var proof *checkoutMutationRefusalError
	if errors.As(err, &proof) || l.RecordCheckoutMutationRefusal(t.Context(), err) {
		t.Fatal("failed source sample manufactured native fallback proof")
	}
}

func TestCheckoutMutationPrepareRefusalCannotAdoptSameEpochFreshRoute(t *testing.T) {
	f, c, l, m, _, err := latePrepareRefusal(t)
	before := f.route()
	m.Close()
	if cycle := c.reconcile(t.Context()); cycle.Err != nil {
		t.Fatal(cycle.Err)
	}
	next := f.route()
	// A full, newly built route must not inherit the old lease even if an
	// upsert reuses its epoch. Never fill the error's proof from this row.
	next.RouteEpoch = before.RouteEpoch
	if next == before {
		t.Fatal("fixture failed to publish a genuinely new full route identity")
	}
	if writeErr := f.catalog.UpsertCheckoutRoute(t.Context(), next); writeErr != nil {
		t.Fatal(writeErr)
	}
	if l.RecordCheckoutMutationRefusal(t.Context(), err) {
		t.Fatal("precommit error adopted fresh same-epoch route")
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "ready" {
		t.Fatalf("fresh route state = %q", state)
	}
}

func TestCheckoutMutationPrepareRefusalRejectsReplacementLifetime(t *testing.T) {
	f, _, l, _, _, err := latePrepareRefusal(t)
	checkout, found, readErr := f.catalog.GetCheckout(t.Context(), f.checkoutID)
	if readErr != nil || !found {
		t.Fatal(readErr)
	}
	checkout.Incarnation += "-replacement"
	newSlot := l.mutationRefusalWitness(checkout, f.route().RouteEpoch)
	if l.RecordCheckoutMutationRefusal(t.Context(), err) || newSlot.slot.mark != nil {
		t.Fatal("old lease crossed replacement refusal-slot lifetime")
	}
}

func TestCheckoutMutationPrepareRefusalRejectsReplacedPhysicalRoot(t *testing.T) {
	f, _, l, m, _, err := latePrepareRefusal(t)
	if !l.RecordCheckoutMutationRefusal(t.Context(), err) {
		t.Fatal("initial mark not established")
	}
	m.Close()
	original := f.worktree + "-original"
	if renameErr := os.Rename(f.worktree, original); renameErr != nil {
		t.Fatal(renameErr)
	}
	defer func() {
		_ = os.Rename(f.worktree, f.worktree+"-replacement")
		if restoreErr := os.Rename(original, f.worktree); restoreErr != nil {
			t.Errorf("restore root: %v", restoreErr)
		}
	}()
	if copyErr := os.CopyFS(f.worktree, os.DirFS(original)); copyErr != nil {
		t.Fatal(copyErr)
	}
	if l.RecordCheckoutMutationRefusal(t.Context(), err) {
		t.Fatal("old root object authorized its replacement")
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "" {
		t.Fatalf("replacement borrowed old mark or claimed healthy authority: %q", state)
	}
}

func TestCheckoutMutationPreparedOrClosedLeaseHasNoNewRefusalProof(t *testing.T) {
	for _, stage := range []string{"prepared", "closed"} {
		t.Run(stage, func(t *testing.T) {
			f, _, l := newCheckoutMutationFixture(t)
			m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if stage == "closed" {
				m.Close()
			} else {
				if err := m.Prepare(t.Context()); err != nil {
					t.Fatal(err)
				}
				// Repeated Prepare is idempotent; it cannot manufacture a
				// first-snapshot refusal proof after route withdrawal.
			}
			err = m.Prepare(t.Context())
			var proof *checkoutMutationRefusalError
			if errors.As(err, &proof) || l.RecordCheckoutMutationRefusal(t.Context(), err) {
				t.Fatal("non-active first-write lease manufactured proof")
			}
		})
	}
}
