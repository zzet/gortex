package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Drive real checked admission against changed bytes; merely constructing a
// view_read_only message would not prove that the refusal has a valid witness.
func staleMutationRefusal(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, l *CheckoutLifecycle) ([]byte, error) {
	t.Helper()
	original, err := os.ReadFile(filepath.Join(f.worktree, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Reused fixtures may already be on the sampled path. Forcing it again
	// recreates the existing helper's reftable sentinel; natural unsupported
	// HEAD layouts likewise already exercise the checked fallback.
	if c.sampler.CaptureHeadEvidence().Usable() {
		forceSampledMutationAdmission(t, f, c)
	}
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ExternalRefusalChange() {}\n")
	lease, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("real stale admission = %v", err)
	}
	var witness *checkoutMutationRefusalError
	if !errors.As(err, &witness) || witness.witness.slot == nil {
		t.Fatal("stale admission lost its guarded identity")
	}
	return original, err
}

func TestCheckoutMutationRefusalOnlyFinalStaleAndSuccessClears(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	at := time.Unix(1200, 0)
	l.now = func() time.Time { return at }
	original, err := staleMutationRefusal(t, f, c, l)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("intermediate admission retry published a refusal")
	}
	for _, excluded := range []error{ErrCheckoutMutationBusy, ErrCheckoutMutationPending, errors.Join(err, ErrCheckoutMutationBusy), errors.Join(err, ErrCheckoutMutationPending)} {
		l.RecordCheckoutMutationRefusal(t.Context(), excluded)
		if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
			t.Fatalf("transient %v armed native allowance", excluded)
		}
	}
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if reason, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); !marked || reason == "" || len(reason) > 80 {
		t.Fatalf("final stale mark = %q/%v", reason, marked)
	}
	l.mutationRefusalMu.Lock()
	stamp := l.mutationRefusals[f.checkoutID].mark.at
	l.mutationRefusalMu.Unlock()
	if !stamp.Equal(at) {
		t.Fatalf("mark timestamp=%v", stamp)
	}
	for _, target := range []struct{ id, root string }{{f.primaryID, f.primary}, {f.checkoutID, f.primary}, {"other-checkout", f.worktree}} {
		if _, marked := l.CheckoutMutationRefusal(t.Context(), target.id, target.root); marked {
			t.Fatal("refusal escaped checkout identity")
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, marked := l.CheckoutMutationRefusal(cancelled, f.checkoutID, f.worktree); marked {
		t.Fatal("unavailable catalog supplied a verdict")
	}
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); !marked {
		t.Fatal("cancelled probe discarded the established refusal")
	}
	if writeErr := os.WriteFile(filepath.Join(f.worktree, "helper.go"), original, 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	lease, admissionErr := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if admissionErr != nil {
		t.Fatal(admissionErr)
	}
	lease.Close()
	// The old error can finish after this successful request returns.
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("late stale error rearmed a successful admission")
	}
}

func TestCheckoutMutationRefusalFreshRouteAndRetirementRejectOldWitness(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	_, err := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	before := f.route()
	if cycle := c.reconcile(t.Context()); cycle.Err != nil {
		t.Fatal(cycle.Err)
	}
	if f.route() == before {
		t.Fatal("fixture did not publish a genuinely new route")
	}
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("published route inherited the old refusal")
	}
	// A replacement slot with identical strings is still a different lifetime;
	// the delayed error must not acquire it through map-key equality.
	l.forgetMutationRefusal(f.checkoutID)
	checkout, found, readErr := l.catalog.GetCheckout(t.Context(), f.checkoutID)
	if readErr != nil || !found {
		t.Fatalf("checkout: %v/%v", found, readErr)
	}
	newWitness := l.mutationRefusalWitness(checkout, f.route().RouteEpoch)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if newWitness.slot.mark != nil {
		t.Fatal("retired witness changed a replacement slot")
	}
}

func TestCheckoutMutationRefusalConcurrentOldPublishAndClear(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	_, err := staleMutationRefusal(t, f, c, l)
	var refused *checkoutMutationRefusalError
	if !errors.As(err, &refused) {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 25 {
				l.RecordCheckoutMutationRefusal(t.Context(), err)
				_, _ = l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree)
			}
		})
	}
	l.clearMutationRefusalOnAdmission(refused.witness, nil)
	workers.Wait()
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("older publishers overwrote the later success")
	}
}

func TestCheckoutMutationRefusalIncarnationAndFullRouteIdentity(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	_, err := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	route := f.route()
	// Catalog upsert can reuse an epoch. Even then a different state/route is
	// not the one whose admission failed.
	route.State = store_sqlite.RoutePending
	if writeErr := l.catalog.UpsertCheckoutRoute(t.Context(), route); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("same epoch hid a changed route identity")
	}
	checkout, found, readErr := l.catalog.GetCheckout(t.Context(), f.checkoutID)
	if readErr != nil || !found {
		t.Fatal(readErr)
	}
	checkout.Incarnation += "-replacement"
	next := l.mutationRefusalWitness(checkout, route.RouteEpoch)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if next.slot.mark != nil {
		t.Fatal("old incarnation armed the new lifetime")
	}
}

func TestCheckoutMutationRefusalByteHandoffSuccessClears(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	original, err := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if writeErr := os.WriteFile(filepath.Join(f.worktree, "helper.go"), original, 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	lease, admissionErr := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	if admissionErr != nil {
		t.Fatal(admissionErr)
	}
	lease.Close()
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("successful public byte admission retained or rearmed refusal")
	}
}

func TestCheckoutMutationRefusalFailureThenSameEpochReplacementBeforeCapture(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	checkout, found, err := l.catalog.GetCheckout(t.Context(), f.checkoutID)
	if err != nil || !found {
		t.Fatal(err)
	}
	before := f.route()
	witness := l.mutationRefusalWitness(checkout, before.RouteEpoch)
	// Admission observed before, then failed. Delay the OUTER error capture
	// until a same-epoch catalog replacement has already landed.
	failed := withMutationRefusalRoute(ErrCheckoutMutationStale, before)
	replacementID, _, beginErr := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: f.graphID, CheckoutID: f.checkoutID,
		LayerID: "same-epoch-refusal-replacement", GenerationKind: DirtyLayerGenerationKind,
		BaseGenerationID: before.CommitGenerationID, CreatedAt: time.Now().Unix(),
	})
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if err := f.store.PublishPayloadGeneration(t.Context(), replacementID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	replaced := before
	replaced.DirtyGenerationID = replacementID
	if err := l.catalog.UpsertCheckoutRoute(t.Context(), replaced); err != nil {
		t.Fatal(err)
	}
	captured := l.captureMutationRefusalError(failed, witness)
	var receipt *checkoutMutationRefusalError
	if !errors.As(captured, &receipt) || receipt.witness.route != before {
		t.Fatal("outer capture reread and replaced the actual admission route")
	}
	l.RecordCheckoutMutationRefusal(t.Context(), captured)
	l.mutationRefusalMu.Lock()
	marked := witness.slot.mark != nil
	l.mutationRefusalMu.Unlock()
	if marked {
		t.Fatal("old failure acquired same-epoch replacement identity")
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "ready" {
		t.Fatalf("replacement is an active healthy route, not a pending-route shortcut: %s", state)
	}
	unknown := l.captureMutationRefusalError(ErrCheckoutMutationStale, witness)
	if errors.As(unknown, &receipt) {
		t.Fatal("pre-route failure invented observed route evidence")
	}
}

func TestCheckoutMutationProbeHealthyAddsZeroCatalogReads(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	probe := l.CaptureCheckoutMutationProbe()
	if _, _, known := l.currentMutationRefusalRoute(t.Context(), f.checkoutID, f.worktree); !known {
		t.Fatal("fixture must establish the existing exact/servable route proof")
	}
	// No catalog is available during the added state checks. Any attempted
	// fallback catalog read would abstain instead of returning ready. Check a
	// repeated healthy census, rather than asserting elapsed wall time.
	catalog := l.catalog
	l.catalog = nil
	defer func() { l.catalog = catalog }()
	for call := range 1000 {
		if state, reason := l.CheckoutMutationStateForProbe(t.Context(), f.checkoutID, f.worktree, probe, true); state != "ready" || reason != "" {
			t.Fatalf("healthy census call%d introduced catalog dependency: %s/%s", call, state, reason)
		}
	}
	if state, _ := l.CheckoutMutationStateForProbe(t.Context(), f.checkoutID, f.worktree, probe, false); state != "" {
		t.Fatal("inexact/unservable proof authorized ready")
	}
}

func TestCheckoutMutationProbeRevalidatesChangesDuringResolution(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	original, old := staleMutationRefusal(t, f, c, l)
	beforeRefusal := l.CaptureCheckoutMutationProbe()
	l.RecordCheckoutMutationRefusal(t.Context(), old)
	if state, _ := l.CheckoutMutationStateForProbe(t.Context(), f.checkoutID, f.worktree, beforeRefusal, true); state != "read_only" {
		t.Fatalf("refusal during existing probe was hidden: %s", state)
	}
	beforeAdmission := l.CaptureCheckoutMutationProbe()
	if err := os.WriteFile(filepath.Join(f.worktree, "helper.go"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	lease, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if state, _ := l.CheckoutMutationStateForProbe(t.Context(), f.checkoutID, f.worktree, beforeAdmission, true); state != "ready" {
		t.Fatalf("successful admission during resolution did not clear: %s", state)
	}
	beforeReplacement := l.CaptureCheckoutMutationProbe()
	l.forgetMutationRefusal(f.checkoutID)
	_, newer := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), newer)
	l.RecordCheckoutMutationRefusal(t.Context(), old)
	if state, _ := l.CheckoutMutationStateForProbe(t.Context(), f.checkoutID, f.worktree, beforeReplacement, true); state != "read_only" {
		t.Fatalf("old resolution cleared replacement refusal: %s", state)
	}
}

func TestCheckoutMutationSelectedRouteCoversCoordinatorClosingWithoutReread(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(t.Context(), f.checkoutID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	route, known := view.CheckoutMutationRoute()
	if !known || route != f.route() {
		t.Fatal("materializer did not retain its actual pinned route")
	}
	checkout, found, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
	if err != nil || !found {
		t.Fatal(err)
	}
	l.coordMu.Lock()
	l.coordinatorClosing = true
	l.coordMu.Unlock()
	// Compatibility API has no selected proof: preserve its original refusal
	// semantics but do not invent a later route identity for hook relaxation.
	lease, unproven := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(unproven, ErrCheckoutMutationStale) || l.RecordCheckoutMutationRefusal(t.Context(), unproven) {
		t.Fatal("ordinary pre-route failure acquired an invented identity")
	}
	lease, refusal := l.BeginCheckoutMutationAtView(t.Context(), f.checkoutID, f.worktree, checkout.Incarnation, route)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(refusal, ErrCheckoutMutationStale) || !l.RecordCheckoutMutationRefusal(t.Context(), refusal) {
		t.Fatalf("selected exact route did not cover coordinator closing: %v", refusal)
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "read_only" {
		t.Fatalf("modern coverage would override legacy fallback with %q", state)
	}
	l.coordMu.Lock()
	l.coordinatorClosing = false
	l.coordMu.Unlock()
	lease, err = l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if l.RecordCheckoutMutationRefusal(t.Context(), refusal) {
		t.Fatal("old selected-route refusal rearmed after success")
	}
	if state, _ := l.CheckoutMutationState(t.Context(), f.checkoutID, f.worktree); state != "ready" {
		t.Fatal("success did not restore authoritative ready")
	}
}
