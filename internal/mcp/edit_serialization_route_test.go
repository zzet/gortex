package mcp

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/indexer"
)

func TestResolveSupersededFailedReceiptsPreservesCheckoutPublicationTruth(t *testing.T) {
	t.Parallel()

	server := &Server{}
	selectedPath := filepath.Join(t.TempDir(), "selected.go")
	otherPath := filepath.Join(t.TempDir(), "selected.go")
	failure := errors.New("route publication failed")

	receipt := func(id, path, checkoutID string, generation uint64) *mutationReceipt {
		r := &mutationReceipt{
			id:         id,
			path:       path,
			generation: generation,
			checkoutID: checkoutID,
			completed:  true,
			result: indexer.MutationResult{
				RequestedGeneration: generation,
				CheckoutID:          checkoutID,
				Err:                 failure,
			},
		}
		server.mutationReceipts.Store(id, r)
		return r
	}

	sameCheckout := receipt("same-checkout", selectedPath, "checkout-a", 1)
	otherCheckout := receipt("other-checkout", selectedPath, "checkout-b", 1)
	otherFile := receipt("other-file", otherPath, "checkout-a", 1)

	server.resolveSupersededFailedReceipts(selectedPath, 2, indexer.MutationResult{
		RequestedGeneration: 2,
		AppliedGeneration:   4,
		CheckoutID:          "checkout-a",
		PublishedRouteEpoch: 9,
		Reindexed:           true,
	})

	assertResult := func(r *mutationReceipt) indexer.MutationResult {
		t.Helper()
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.result
	}

	healed := assertResult(sameCheckout)
	if healed.Err != nil || !healed.Reindexed {
		t.Fatalf("same-checkout receipt was not healed: %+v", healed)
	}
	if healed.CheckoutID != "checkout-a" || healed.PublishedRouteEpoch != 9 {
		t.Fatalf("healed receipt lost route publication truth: %+v", healed)
	}
	if healed.RequestedGeneration != 1 || healed.AppliedGeneration != 4 {
		t.Fatalf("healed receipt has wrong generations: %+v", healed)
	}

	for name, untouched := range map[string]*mutationReceipt{
		"different checkout": otherCheckout,
		"different file":     otherFile,
	} {
		t.Run(name, func(t *testing.T) {
			result := assertResult(untouched)
			if !errors.Is(result.Err, failure) || result.Reindexed {
				t.Fatalf("unrelated receipt was healed: %+v", result)
			}
		})
	}
}
