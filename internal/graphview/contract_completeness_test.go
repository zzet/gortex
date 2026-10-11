package graphview

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestContractCompletenessDoesNotUseSemanticFileFollowup(t *testing.T) {
	store := openStackStore(t, "contract-completeness")
	ctx := context.Background()
	makeGeneration := func(base int64, state store_sqlite.ProducerState) int64 {
		t.Helper()
		id, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
			OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: stackDirtyLayerID,
			CheckoutID: testCheckoutID, GenerationKind: "dirty", BaseGenerationID: base,
			TreeOID: "contract-deletion", CreatedAt: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Deletion removes graph payload, but cleanup of its old external owners
		// and consumers is still contract work. No semantic path remains owed.
		if err := handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: stackGoneFile, Mode: store_sqlite.OwnershipDelete}}); err != nil {
			t.Fatal(err)
		}
		if err := handle.SetProducerState(store_sqlite.ProducerCompleteness{Producer: string(CapContracts), State: state, Reason: ReasonDeferredToFollowup}); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishPayloadGeneration(ctx, id, 1001); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending := makeGeneration(0, store_sqlite.ProducerStateIncomplete)
	unrelatedComplete := makeGeneration(pending, store_sqlite.ProducerStateComplete)
	c, err := newTestMaterializer(store).completeness([]*store_sqlite.Store{store.AtGeneration(pending), store.AtGeneration(unrelatedComplete)})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.State(CapContracts); got != StateIncomplete {
		t.Fatalf("deleted owner debt = %s, want incomplete", got)
	}
	if err := c.Evaluate([]CapabilityID{CapSyntaxGraph, CapResolutionLocal, CapIncomingEdges, CapSourceSnapshot}, nil); err != nil {
		t.Fatalf("contract debt blocked ordinary graph reads: %v", err)
	}
	if err := c.Evaluate([]CapabilityID{CapContracts}, nil); CodeOf(err) != CodeRequiredCapabilityIncomplete {
		t.Fatalf("contract consumer refusal = %v", err)
	}
}

func TestContractCompletenessDoesNotCertifySilentBaseline(t *testing.T) {
	store := openStackStore(t, "contract-unknown-baseline")
	commit := writeProducerGeneration(t, store, "commit", stackCommitLayerID, 0, 1000)
	dirty := writeProducerGeneration(t, store, "dirty", stackDirtyLayerID, commit, 2000,
		store_sqlite.ProducerCompleteness{Producer: string(CapContracts), State: store_sqlite.ProducerStateComplete})
	c, err := newTestMaterializer(store).completeness([]*store_sqlite.Store{store.AtGeneration(commit), store.AtGeneration(dirty)})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.State(CapContracts); got != StateUnavailable {
		t.Fatalf("silent baseline readiness=%s, want unavailable", got)
	}
}
