package graphview

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestSemanticCompletenessFollowsTheOrdinaryRule pins that graph.semantic
// composes like every other capability: a layer's silence reads as complete,
// and a layer that declares it incomplete makes the view incomplete. Every
// generation the builders write declares it (complete only when the type
// checker's rows cover its Go files), and a base an older binary built is
// re-parsed at upgrade because the capability joined the vocabulary, so no
// silent generation is left to read as holding rows it lacks.
func TestSemanticCompletenessFollowsTheOrdinaryRule(t *testing.T) {
	semantic := func(state store_sqlite.ProducerState) store_sqlite.ProducerCompleteness {
		return store_sqlite.ProducerCompleteness{Producer: string(CapSemantic), State: state, Reason: "test"}
	}
	complete := semantic(store_sqlite.ProducerStateComplete)
	incomplete := semantic(store_sqlite.ProducerStateIncomplete)
	cases := []struct {
		name          string
		commit, dirty []store_sqlite.ProducerCompleteness
		want          CapabilityState
	}{
		{"no layer declares it", nil, nil, StateComplete},
		{"both declare it complete", []store_sqlite.ProducerCompleteness{complete}, []store_sqlite.ProducerCompleteness{complete}, StateComplete},
		{"the base declares it incomplete", []store_sqlite.ProducerCompleteness{incomplete}, []store_sqlite.ProducerCompleteness{complete}, StateIncomplete},
		{"the dirty layer declares it incomplete", []store_sqlite.ProducerCompleteness{complete}, []store_sqlite.ProducerCompleteness{incomplete}, StateIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openStackStore(t, "semantic-completeness")
			seedStackControlPlane(t, store)
			commit := writeProducerGeneration(t, store, "commit", stackCommitLayerID, 0, 1000, tc.commit...)
			dirty := writeProducerGeneration(t, store, "dirty", stackDirtyLayerID, commit, 3000, tc.dirty...)
			routeStack(t, store, commit, dirty, store_sqlite.RouteActive)

			view, err := newTestMaterializer(store).MaterializeCheckout(context.Background(), testCheckoutID)
			if err != nil {
				t.Fatalf("MaterializeCheckout: %v", err)
			}
			defer view.Close()
			if got := view.Completeness.State(CapSemantic); got != tc.want {
				t.Fatalf("%s = %q, want %q", CapSemantic, got, tc.want)
			}
		})
	}
	t.Run("a ref view over a generation that declares it incomplete", func(t *testing.T) {
		store := openStackStore(t, "semantic-refview")
		seedStackControlPlane(t, store)
		generation := writeProducerGeneration(t, store, "commit", stackCommitLayerID, 0, 1000, incomplete)
		view, err := newTestMaterializer(store).MaterializeRefView(context.Background(), testGraphID, generation)
		if err != nil {
			t.Fatalf("MaterializeRefView: %v", err)
		}
		defer view.Close()
		if got := view.Completeness.State(CapSemantic); got != StateIncomplete {
			t.Fatalf("%s = %q, want %q", CapSemantic, got, StateIncomplete)
		}
	})
}
