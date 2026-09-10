package reconcile

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type capturedCleanupTestHooks struct {
	beginErr    error
	finished    map[string]int
	legacyCalls int
}

func (*capturedCleanupTestHooks) PurgeCheckoutLayers(context.Context, string, string) error {
	return nil
}
func (*capturedCleanupTestHooks) ReleaseGraph(context.Context, string) error { return nil }
func (h *capturedCleanupTestHooks) BeginGraphCleanup(context.Context, string) error {
	return h.beginErr
}
func (h *capturedCleanupTestHooks) RepositoryCleanupAttemptFinished([]string) { h.legacyCalls++ }
func (h *capturedCleanupTestHooks) BeginGraphCleanupAttempt(_ context.Context, graphID string) (func(), error) {
	return func() { h.finished[graphID]++ }, h.beginErr
}

func TestRepositoryCleanupCapturedErrorFinishWaitsForOuterAttempt(t *testing.T) {
	wantErr := errors.New("private initialization failure with durable retry state")
	hooks := &capturedCleanupTestHooks{beginErr: wantErr, finished: make(map[string]int)}
	r := &Reconciler{hooks: hooks}
	ctx, finishOuter := r.repositoryCleanupAttempt(context.Background(), "primary")
	if err := r.beginRepositoryCleanup(ctx, "primary"); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	// Every phase still calls Begin; completion is retained only once per graph
	// in the outer attempt, including when the Begin call returns an error.
	if err := r.beginRepositoryCleanup(ctx, "primary"); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	nested, finishInner := r.repositoryCleanupAttempt(ctx, "dependent")
	if err := r.beginRepositoryCleanup(nested, "dependent"); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	finishInner()
	if len(hooks.finished) != 0 || hooks.legacyCalls != 0 {
		t.Fatal("nested completion escaped before outer persistence")
	}
	finishOuter()
	if want := map[string]int{"primary": 1, "dependent": 1}; !reflect.DeepEqual(hooks.finished, want) {
		t.Fatalf("captured finishes=%v want=%v", hooks.finished, want)
	}
	if hooks.legacyCalls != 0 {
		t.Fatal("captured hooks also received an unqualified graph-ID callback")
	}
}

func TestRepositoryCleanupCapturedFinishesRemainReconcilerLocal(t *testing.T) {
	h1 := &capturedCleanupTestHooks{finished: make(map[string]int)}
	h2 := &capturedCleanupTestHooks{finished: make(map[string]int)}
	r1, r2 := &Reconciler{hooks: h1}, &Reconciler{hooks: h2}
	ctx1, finish1 := r1.repositoryCleanupAttempt(context.Background(), "same-graph")
	if err := r1.beginRepositoryCleanup(ctx1, "same-graph"); err != nil {
		t.Fatal(err)
	}
	ctx2, finish2 := r2.repositoryCleanupAttempt(ctx1, "same-graph")
	if err := r2.beginRepositoryCleanup(ctx2, "same-graph"); err != nil {
		t.Fatal(err)
	}
	finish2()
	if len(h1.finished) != 0 || h2.finished["same-graph"] != 1 {
		t.Fatal("separate Reconciler inherited another attempt's completion")
	}
	finish1()
	if h1.finished["same-graph"] != 1 || h2.finished["same-graph"] != 1 || h1.legacyCalls != 0 || h2.legacyCalls != 0 {
		t.Fatal("captured completion ownership changed")
	}
}
