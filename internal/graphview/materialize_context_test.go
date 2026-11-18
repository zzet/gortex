package graphview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestNewGenerationLayerContextRejectsCanceledContext(t *testing.T) {
	store := openStackStore(t, "generation-layer-canceled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewGenerationLayerContext(ctx, store.AtGeneration(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewGenerationLayerContext error = %v, want context.Canceled", err)
	}
}

func TestMaterializeCheckoutCancellationAfterLeaseReleasesLease(t *testing.T) {
	store := openStackStore(t, "materialize-canceled")
	commit, dirty := seedRoutedStack(t, store)
	materializer := newTestMaterializer(store)

	entered := make(chan struct{})
	materializer.newGenerationLayer = func(ctx context.Context, _ *store_sqlite.Store) (*GenerationLayer, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
		if view != nil {
			view.Close()
		}
		done <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("materialization did not reach generation construction")
	}
	for _, generationID := range []int64{commit, dirty} {
		if !materializer.Leases.InUse(generationID) {
			t.Fatalf("generation %d was not leased before generation construction", generationID)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MaterializeCheckout error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MaterializeCheckout did not return after cancellation")
	}

	if err := materializer.Leases.WaitDrain(context.Background(), commit, dirty); err != nil {
		t.Fatalf("WaitDrain after canceled materialization: %v", err)
	}
	for _, generationID := range []int64{commit, dirty} {
		if materializer.Leases.InUse(generationID) {
			t.Fatalf("generation %d is still leased after canceled materialization", generationID)
		}
	}
}
