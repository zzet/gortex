package graphview

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Existing public RetirePayloadGeneration(inUse) callback supplies the barrier:
// it observes no pin, then a real public materialization completes before that
// stale observation is returned. No production hook or global test state.
func TestPrivateMaterializedRefSurvivesRetirementPinRace(t *testing.T) {
	store := openStackStore(t, "lease-retirement-race")
	seedStackControlPlane(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	generation, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "graph-1", LayerID: "private-historical-commit",
		CheckoutID: "wt-1", GenerationKind: "commit", TreeOID: "private-tree",
	})
	if err != nil {
		t.Fatal(err)
	}
	node := &graph.Node{ID: "repo/a.go::Retained", Kind: graph.KindFunction, Name: "Retained", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"}
	unread := &graph.Node{ID: "repo/a.go::Unread", Kind: graph.KindFunction, Name: "Unread", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"}
	handle.AddBatch([]*graph.Node{
		{ID: "repo/a.go", Kind: graph.KindFile, FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"},
		node,
		unread,
	}, nil)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: store_sqlite.OwnershipReplace}}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPayloadGeneration(ctx, generation, 750); err != nil {
		t.Fatal(err)
	}
	m := newTestMaterializer(store)
	observedUnpinned := make(chan struct{})
	resumeRetirement := make(chan struct{})
	retirementDone := make(chan error, 1)
	retirementExited := make(chan struct{})
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(resumeRetirement) }) }
	defer resume()
	t.Cleanup(func() {
		resume()
		cancel()
		<-retirementExited
	})
	calls := 0
	go func() {
		defer close(retirementExited)
		retirementDone <- store.RetirePayloadGeneration(ctx, generation, func(id int64) bool {
			inUse := m.Leases.InUse(id)
			calls++
			if calls == 1 {
				close(observedUnpinned)
				select {
				case <-resumeRetirement:
				case <-ctx.Done():
					return true
				}
			}
			return inUse
		})
	}()
	select {
	case <-observedUnpinned:
	case err := <-retirementDone:
		t.Fatalf("retirement returned before existing inUse barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("retirement did not reach inUse barrier")
	}
	view, err := m.MaterializeRefView(ctx, "graph-1", generation)
	if err != nil {
		// A correct fence-first algorithm makes the generation unservable
		// before its first inUse callback. The later reader must refuse it
		// without leaving any provisional/full ancestry pins behind.
		row, found, stateErr := store.Catalog().GetViewGeneration(ctx, generation)
		if stateErr != nil || !found || row.State != store_sqlite.ViewGenerationRetiring {
			t.Fatalf("materialization failed without established retirement fence: err=%v row=%+v found=%v state_err=%v", err, row, found, stateErr)
		}
		if m.Leases.InUse(generation) {
			t.Error("rejected materialization leaked generation pin")
		}
		resume()
		select {
		case retireErr := <-retirementDone:
			if retireErr != nil && !errors.Is(retireErr, store_sqlite.ErrPayloadGenerationInUse) {
				t.Fatalf("fence-first retirement returned unexpected error: %v", retireErr)
			}
		case <-ctx.Done():
			t.Fatal("fence-first retirement did not finish after barrier release")
		}
		return
	}
	defer view.Close()
	if !m.Leases.InUse(generation) || view.Reader.GetNode(node.ID) == nil {
		t.Fatalf("fixture did not obtain a live pinned payload view: in_use=%v generations=%v view_id=%+v node_id=%q direct_node=%+v view_node=%+v", m.Leases.InUse(generation), view.Generations(), view.ID, node.ID, handle.GetNode(node.ID), view.Reader.GetNode(node.ID))
	}
	resume()
	var retireErr error
	select {
	case retireErr = <-retirementDone:
	case <-ctx.Done():
		t.Fatal("retirement did not finish after barrier released")
	}
	t.Logf("retirement result while view remains pinned: %v; inUse callbacks=%d", retireErr, calls)
	if retireErr != nil && !errors.Is(retireErr, store_sqlite.ErrPayloadGenerationInUse) {
		t.Errorf("reader-winning retirement returned unexpected error: %v", retireErr)
	}
	_, found, readErr := store.Catalog().GetViewGeneration(ctx, generation)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !found {
		t.Error("retirement deleted generation metadata while materialized view remains pinned")
	}
	// Do not rely only on the materialized view's already-warmed node cache.
	if store.AtGeneration(generation).GetNode(unread.ID) == nil {
		t.Error("retirement removed unread generation payload while materialized view remains pinned")
	}
	if view.Reader.GetNode(node.ID) == nil {
		t.Error("retirement swept payload from a successfully materialized pinned view")
	}
	view.Close()
	if m.Leases.InUse(generation) {
		t.Error("closed view retained lease")
	}
	if found && errors.Is(retireErr, store_sqlite.ErrPayloadGenerationInUse) {
		if err := store.RetirePayloadGeneration(ctx, generation, m.Leases.InUse); err != nil {
			t.Errorf("retirement must resume after pin drains: %v", err)
		}
	}
}
