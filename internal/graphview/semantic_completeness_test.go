package graphview

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestSemanticCompletenessNeedsEveryLayerToDeclareIt pins that graph.semantic
// is never read off the seed. A generation that has no row for it (a base an
// older binary built before the capability existed, a commit build, a build
// without a semantic manager) makes the view's graph.semantic not complete,
// even when the layer above declares it complete for its own paths.
func TestSemanticCompletenessNeedsEveryLayerToDeclareIt(t *testing.T) {
	semantic := func(state store_sqlite.ProducerState) store_sqlite.ProducerCompleteness {
		return store_sqlite.ProducerCompleteness{Producer: string(CapSemantic), State: state}
	}
	complete := semantic(store_sqlite.ProducerStateComplete)
	cases := []struct {
		name          string
		commit, dirty []store_sqlite.ProducerCompleteness
		want          CapabilityState
	}{
		{"no layer declares it", nil, nil, StateUnavailable},
		{"the base is silent, the dirty layer complete", nil, []store_sqlite.ProducerCompleteness{complete}, StateUnavailable},
		{"the base complete, the dirty layer silent", []store_sqlite.ProducerCompleteness{complete}, nil, StateUnavailable},
		{"both declare it complete", []store_sqlite.ProducerCompleteness{complete}, []store_sqlite.ProducerCompleteness{complete}, StateComplete},
		{"a declared incomplete still worsts", []store_sqlite.ProducerCompleteness{complete},
			[]store_sqlite.ProducerCompleteness{semantic(store_sqlite.ProducerStateIncomplete)}, StateIncomplete},
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
			// The rule is about graph.semantic alone.
			if got := view.Completeness.State(CapSyntaxGraph); got != StateComplete {
				t.Errorf("%s = %q, want %q", CapSyntaxGraph, got, StateComplete)
			}
		})
	}
	t.Run("a ref view over a silent generation", func(t *testing.T) {
		store := openStackStore(t, "semantic-refview")
		seedStackControlPlane(t, store)
		generation := writeProducerGeneration(t, store, "commit", stackCommitLayerID, 0, 1000)
		view, err := newTestMaterializer(store).MaterializeRefView(context.Background(), testGraphID, generation)
		if err != nil {
			t.Fatalf("MaterializeRefView: %v", err)
		}
		defer view.Close()
		if got := view.Completeness.State(CapSemantic); got != StateUnavailable {
			t.Fatalf("%s = %q, want %q", CapSemantic, got, StateUnavailable)
		}
	})
}
