package indexer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Retention fixture.
//
// The sweep reads three things — the ref-view generations of a graph, the
// views pointing at them, and its own clock — and offers what the bounds no
// longer keep to the same guarded retire the coordinators use. So the fixture
// writes exactly those rows and drives the pass directly: what a build
// produced is irrelevant to which generation the bounds evict.

const retentionGraphID = "graph-retention"

type retentionFixture struct {
	t         *testing.T
	lifecycle *CheckoutLifecycle
	store     *store_sqlite.Store
	catalog   *store_sqlite.Catalog
	now       time.Time
}

func newRetentionFixture(t *testing.T, bounds RefViewRetention) *retentionFixture {
	t.Helper()
	store := builderOpenStore(t, "retention")
	now := time.Unix(1_000_000_000, 0)
	f := &retentionFixture{
		t:       t,
		store:   store,
		catalog: store.Catalog(),
		now:     now,
	}
	f.lifecycle = &CheckoutLifecycle{
		store:            store,
		catalog:          store.Catalog(),
		leases:           graphview.NewLeaseManager(),
		logger:           zap.NewNop(),
		now:              func() time.Time { return f.now },
		refViewRetention: bounds.withDefaults(),
	}
	return f
}

// publishGeneration writes one ready ref-view generation. The publish measures
// and records its payload size itself, which is the size the byte bound reads.
func (f *retentionFixture) publishGeneration(layerID string) int64 {
	f.t.Helper()
	ctx := context.Background()
	generationID, handle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      refViewOwnerKind,
		GraphID:        retentionGraphID,
		LayerID:        layerID,
		GenerationKind: CommitLayerGenerationKind,
		TreeOID:        "tree-" + layerID,
		CreatedAt:      f.now.Unix(),
	})
	if err != nil {
		f.t.Fatalf("BeginPayloadGeneration(%s): %v", layerID, err)
	}
	handle.AddBatch([]*graph.Node{{
		ID: layerID + "/file.go", Kind: graph.KindFile, Name: "file.go",
		FilePath: layerID + "/file.go", RepoPrefix: builderRepoPrefix, Language: "go",
	}}, nil)
	// The publish measures storage_bytes from the generation's file rows, so a
	// fixture that wants the byte bound to see anything has to write them.
	if err := handle.SetFileMetas(builderRepoPrefix, []graph.FileMetaRow{{
		FilePath: layerID + "/file.go", ContentHash: "hash-" + layerID, Size: 1024, NodeCount: 1,
	}}); err != nil {
		f.t.Fatalf("SetFileMetas(%s): %v", layerID, err)
	}
	if err := f.store.PublishPayloadGeneration(ctx, generationID, f.now.Unix()); err != nil {
		f.t.Fatalf("PublishPayloadGeneration(%s): %v", layerID, err)
	}
	return generationID
}

// beginGeneration writes one ref-view generation and leaves it in the building
// state — what a build in flight looks like to the sweep, and what a build
// whose process died leaves behind.
func (f *retentionFixture) beginGeneration(layerID string, createdAt time.Time) int64 {
	f.t.Helper()
	generationID, _, err := f.store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:      refViewOwnerKind,
		GraphID:        retentionGraphID,
		LayerID:        layerID,
		GenerationKind: CommitLayerGenerationKind,
		TreeOID:        "tree-" + layerID,
		CreatedAt:      createdAt.Unix(),
	})
	if err != nil {
		f.t.Fatalf("BeginPayloadGeneration(%s): %v", layerID, err)
	}
	return generationID
}

// graphBytes sums the payload sizes the publishes recorded.
func (f *retentionFixture) graphBytes() int64 {
	f.t.Helper()
	rows, err := f.catalog.ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{
		GraphID: retentionGraphID,
	})
	if err != nil {
		f.t.Fatalf("ListViewGenerations: %v", err)
	}
	var total int64
	for _, row := range rows {
		total += row.StorageBytes
	}
	return total
}

// serve points a ref view at a generation with a given last-selection time.
func (f *retentionFixture) serve(refViewID string, generationID int64, selected time.Time) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.catalog.UpsertRefView(ctx, store_sqlite.RefView{
		RefViewID:               refViewID,
		GraphID:                 retentionGraphID,
		SelectorKind:            "git_ref",
		SelectorValue:           "refs/heads/" + refViewID,
		EnrichmentProfile:       defaultEnrichmentProfile,
		DesiredTree:             "tree-" + refViewID,
		ActiveGenerationID:      generationID,
		ActiveTree:              "tree-" + refViewID,
		DesiredBuildFingerprint: "fp-" + refViewID,
		ActiveBuildFingerprint:  "fp-" + refViewID,
		State:                   store_sqlite.RefViewReady,
		ExactView:               true,
		LastResolved:            selected.Unix(),
		LastSelected:            selected.Unix(),
	}); err != nil {
		f.t.Fatalf("UpsertRefView(%s): %v", refViewID, err)
	}
}

func (f *retentionFixture) liveGenerations() map[int64]bool {
	f.t.Helper()
	rows, err := f.catalog.ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{
		GraphID: retentionGraphID,
	})
	if err != nil {
		f.t.Fatalf("ListViewGenerations: %v", err)
	}
	out := map[int64]bool{}
	for _, row := range rows {
		out[row.GenerationID] = true
	}
	return out
}

// TestRefViewRetentionEvictsLeastRecentlySelected pins the count bound: past
// the per-graph cap the oldest inactive generations go and the newest stay,
// and the generation a recently selected view is serving is never among them
// however far over the cap the graph is.
func TestRefViewRetentionEvictsLeastRecentlySelected(t *testing.T) {
	const limit = 3
	f := newRetentionFixture(t, RefViewRetention{
		RetainInactive:       24 * time.Hour,
		MaxCachedGenerations: limit,
	})

	// One view actively serving, selected a moment ago.
	served := f.publishGeneration("served")
	f.serve("served", served, f.now.Add(-time.Minute))

	// limit + 2 generations nothing points at any more — what a graph
	// accumulates as the branches it was asked about move on.
	ids := make([]int64, 0, limit+2)
	for i := range limit + 2 {
		f.now = f.now.Add(time.Minute)
		ids = append(ids, f.publishGeneration(fmt.Sprintf("stranded%d", i)))
	}

	// The graph holds 1 + limit + 2 generations, so three have to go.
	if retired := f.lifecycle.sweepRefViewRetention(context.Background()); retired != 3 {
		t.Fatalf("the sweep retired %d generations, want the 3 over the cap of %d", retired, limit)
	}
	live := f.liveGenerations()
	if !live[served] {
		t.Error("the generation a recently selected view is serving was evicted")
	}
	for i, generationID := range ids {
		want := i >= 3
		if live[generationID] != want {
			t.Errorf("generation %d (publish rank %d) live=%v, want %v",
				generationID, i, live[generationID], want)
		}
	}
}

// TestRefViewRetentionKeepsWhatFitsTheBounds pins the other direction: a graph
// inside every bound loses nothing, however many sweeps run.
func TestRefViewRetentionKeepsWhatFitsTheBounds(t *testing.T) {
	f := newRetentionFixture(t, RefViewRetention{
		RetainInactive:       24 * time.Hour,
		MaxCachedGenerations: 8,
	})
	var ids []int64
	for i := range 3 {
		name := fmt.Sprintf("keep%d", i)
		generationID := f.publishGeneration(name)
		f.serve(name, generationID, f.now.Add(-time.Minute))
		ids = append(ids, generationID)
	}
	for range 2 {
		if retired := f.lifecycle.sweepRefViewRetention(context.Background()); retired != 0 {
			t.Fatalf("a sweep inside every bound retired %d generations", retired)
		}
	}
	live := f.liveGenerations()
	for _, generationID := range ids {
		if !live[generationID] {
			t.Errorf("generation %d went while it was inside every bound", generationID)
		}
	}
}

// TestRefViewRetentionEvictsPastTheWindow pins the recency bound: a view that
// nobody has selected for longer than the window loses its payload even when
// the count and byte bounds have room.
func TestRefViewRetentionEvictsPastTheWindow(t *testing.T) {
	f := newRetentionFixture(t, RefViewRetention{
		RetainInactive:       time.Hour,
		MaxCachedGenerations: 100,
	})
	fresh := f.publishGeneration("fresh")
	f.serve("fresh", fresh, f.now.Add(-time.Minute))
	stale := f.publishGeneration("stale")
	f.serve("stale", stale, f.now.Add(-8*time.Hour))

	if retired := f.lifecycle.sweepRefViewRetention(context.Background()); retired != 1 {
		t.Fatalf("the sweep retired %d generations, want the one past the window", retired)
	}
	live := f.liveGenerations()
	if !live[fresh] {
		t.Error("the recently selected view lost its generation")
	}
	if live[stale] {
		t.Error("the view nobody has selected for eight hours kept its generation")
	}
	if _, found, err := f.catalog.GetRefView(context.Background(), "stale"); err != nil || found {
		t.Errorf("the evicted view's row survived: found=%v err=%v", found, err)
	}
	if _, found, err := f.catalog.GetRefView(context.Background(), "fresh"); err != nil || !found {
		t.Errorf("the kept view's row went with the sweep: found=%v err=%v", found, err)
	}
}

// TestRefViewRetentionNeverEvictsALeasedGeneration pins the safety rule: the
// bounds decide what should go, and the guarded retire decides what may. A
// generation a live view is reading is refused and stays.
func TestRefViewRetentionNeverEvictsALeasedGeneration(t *testing.T) {
	f := newRetentionFixture(t, RefViewRetention{
		RetainInactive:       time.Hour,
		MaxCachedGenerations: 100,
	})
	stale := f.publishGeneration("stale")
	f.serve("stale", stale, f.now.Add(-8*time.Hour))

	lease := f.lifecycle.leases.Acquire(stale)
	defer lease.Release()

	if retired := f.lifecycle.sweepRefViewRetention(context.Background()); retired != 0 {
		t.Fatalf("the sweep retired %d generations while one was leased", retired)
	}
	if !f.liveGenerations()[stale] {
		t.Error("a leased generation was swept")
	}
}

// TestRefViewRetentionHonoursTheByteBudget pins the third bound against the
// sizes the publishes actually recorded: over budget, the least recently
// selected generation goes even though the count and the window have room.
func TestRefViewRetentionHonoursTheByteBudget(t *testing.T) {
	f := newRetentionFixture(t, RefViewRetention{
		RetainInactive:       24 * time.Hour,
		MaxCachedGenerations: 100,
	})
	served := f.publishGeneration("served")
	f.serve("served", served, f.now.Add(-time.Minute))
	oldest := f.publishGeneration("oldest")
	f.now = f.now.Add(time.Minute)
	newest := f.publishGeneration("newest")

	total := f.graphBytes()
	if total <= 0 {
		t.Fatalf("the publishes recorded no payload size (%d); the byte bound has nothing to read", total)
	}
	// A budget one byte under what the graph holds: exactly one generation has
	// to go, and it must be the least recently selected one.
	f.lifecycle.refViewRetention.MaxBytesPerGraph = total - 1

	if retired := f.lifecycle.sweepRefViewRetention(context.Background()); retired != 1 {
		t.Fatalf("the sweep retired %d generations, want the one over the budget", retired)
	}
	live := f.liveGenerations()
	if live[oldest] {
		t.Error("the oldest inactive generation survived the byte budget")
	}
	if !live[newest] {
		t.Error("the newest inactive generation was evicted first")
	}
	if !live[served] {
		t.Error("the byte budget evicted a generation a recently selected view is serving")
	}
}

// TestRefViewRetentionKeepsAnInFlightBuild pins the one thing the guarded
// retire cannot decide for itself. A generation a build is still writing is
// referenced by nothing and leased by nothing, so the retire would accept it,
// seal it under its own builder and fail the build — the janitor fighting the
// selection that asked for the view. So an in-flight build is kept out of the
// candidate scan, and only a building generation past the liveness window —
// one no builder is left behind — is collected.
func TestRefViewRetentionKeepsAnInFlightBuild(t *testing.T) {
	f := newRetentionFixture(t, RefViewRetention{
		RetainInactive:       time.Hour,
		MaxCachedGenerations: 1,
	})
	served := f.publishGeneration("served")
	f.serve("served", served, f.now.Add(-time.Minute))

	// The graph is at its cap already, which is exactly when the candidate
	// loop starts handing generations to the retire.
	inFlight := f.beginGeneration("inflight", f.now)
	crashed := f.beginGeneration("crashed", f.now.Add(-2*refViewBuildLiveness))

	if retired := f.lifecycle.sweepRefViewRetention(context.Background()); retired != 1 {
		t.Fatalf("the sweep retired %d generations, want only the abandoned build's", retired)
	}
	live := f.liveGenerations()
	if !live[inFlight] {
		t.Error("the sweep collected a build that was still writing its payload")
	}
	if live[crashed] {
		t.Error("the payload of a build nothing is running survived the sweep")
	}
	if !live[served] {
		t.Error("the generation a recently selected view is serving was evicted")
	}
	row, found, err := f.catalog.GetViewGeneration(context.Background(), inFlight)
	if err != nil || !found {
		t.Fatalf("read the in-flight generation: found=%v err=%v", found, err)
	}
	if row.State != store_sqlite.ViewGenerationBuilding {
		t.Errorf("the in-flight generation is %s, want it left building", row.State)
	}
}

// TestRefViewRetentionDefaults pins the shipped bounds and the rule that a
// zero knob is unset rather than "collect everything".
func TestRefViewRetentionDefaults(t *testing.T) {
	shipped := DefaultRefViewRetention()
	if shipped.RetainInactive != 7*24*time.Hour {
		t.Errorf("RetainInactive = %s, want 7d", shipped.RetainInactive)
	}
	if shipped.MaxCachedGenerations != 32 {
		t.Errorf("MaxCachedGenerations = %d, want 32", shipped.MaxCachedGenerations)
	}
	if shipped.MaxBytesPerGraph != 5<<30 {
		t.Errorf("MaxBytesPerGraph = %d, want 5GiB", shipped.MaxBytesPerGraph)
	}
	if got := (RefViewRetention{}).withDefaults(); got != shipped {
		t.Errorf("an unset configuration = %+v, want the shipped bounds %+v", got, shipped)
	}
	partial := RefViewRetention{MaxCachedGenerations: 4}.withDefaults()
	if partial.MaxCachedGenerations != 4 {
		t.Errorf("a configured cap was overwritten: %d", partial.MaxCachedGenerations)
	}
	if partial.RetainInactive != shipped.RetainInactive || partial.MaxBytesPerGraph != shipped.MaxBytesPerGraph {
		t.Errorf("an unset bound was not filled from the defaults: %+v", partial)
	}
}
