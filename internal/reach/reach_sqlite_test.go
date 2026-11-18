package reach

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestLookupSQLiteRepeatedAndReloadedPreservesTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reach.db")
	s, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []*graph.Node{
		{ID: "seed", Kind: graph.KindFunction, Name: "seed"},
		{ID: "direct", Kind: graph.KindFunction, Name: "direct"},
		{ID: "transitive", Kind: graph.KindFunction, Name: "transitive"},
	}
	edges := []*graph.Edge{
		{From: "direct", To: "seed", Kind: graph.EdgeCalls, Confidence: 0.9},
		{From: "transitive", To: "direct", Kind: graph.EdgeCalls, Confidence: 0.8},
	}
	s.AddBatch(nodes, edges)

	assertLookup := func(store graph.Store) {
		t.Helper()
		d1, d2, d3, hit, truncated := LookupContext(context.Background(), store, "seed")
		if !hit || truncated {
			t.Fatalf("lookup status hit=%v truncated=%v, want exact hit", hit, truncated)
		}
		if got := joinIDs(d1); got != "direct" {
			t.Fatalf("d1 = %q, want direct", got)
		}
		if got := joinIDs(d2); got != "transitive" {
			t.Fatalf("d2 = %q, want transitive", got)
		}
		if len(d3) != 0 {
			t.Fatalf("d3 = %v, want empty", d3)
		}
	}

	assertLookup(s) // computes an exact, read-only result
	assertLookup(s) // repeated lazy lookup remains exact without writing
	if got := s.EdgeCount(); got != len(edges) {
		t.Fatalf("edge count after repeated lookup = %d, want %d", got, len(edges))
	}
	seed := s.GetNode("seed")
	if seed.Meta != nil {
		if _, ok := seed.Meta[MetaReachBuild]; ok {
			t.Fatal("lazy lookup must not persist a reach build")
		}
		if _, ok := seed.Meta[MetaReachD1Conf]; ok {
			t.Fatal("lazy lookup must not persist reach confidence")
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertLookup(s) // proves exact codec types survive a warm reload
	if got := s.EdgeCount(); got != len(edges) {
		t.Fatalf("edge count after reload = %d, want %d", got, len(edges))
	}
}

func TestReadCachedValidatesLegacySlicesAtomically(t *testing.T) {
	build := BuildCounter()
	n := &graph.Node{Meta: map[string]any{
		MetaReachBuild:    int64(build),
		MetaReachEpoch:    reachProcessEpoch,
		MetaReachComplete: true,
		MetaReachD1:       []any{"caller"},
		MetaReachD1Conf:   []any{0.75},
		MetaReachD1Label:  []any{"INFERRED"},
	}}
	d1, d2, d3, truncated, ok := readCached(n, build)
	if !ok || truncated || len(d1) != 1 || d1[0].ID != "caller" || d1[0].Conf != 0.75 {
		t.Fatalf("legacy cache decode = (%v,%v,%v truncated=%v ok=%v)", d1, d2, d3, truncated, ok)
	}

	// A present-but-short parallel array is an interrupted record, not an
	// intentionally empty tier. Reject the entire record atomically.
	n.Meta[MetaReachD1Conf] = []any{}
	if _, _, _, _, ok := readCached(n, build); ok {
		t.Fatal("cache with mismatched parallel tier arrays was accepted")
	}
	n.Meta[MetaReachBuild] = int64(-1)
	if _, _, _, _, ok := readCached(n, build); ok {
		t.Fatal("negative legacy generation was accepted as uint64")
	}
}

type blockingBoundedStore struct {
	graph.Store
	started chan struct{}
	once    sync.Once
}

func (s *blockingBoundedStore) GetInEdgesByNodeIDsContext(ctx context.Context, _ []string, _ int) (map[string][]*graph.Edge, bool, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return nil, true, ctx.Err()
}

func TestLookupTraversalDoesNotStarveTopologyOrResolverLocks(t *testing.T) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: "seed", Kind: graph.KindFunction, Name: "seed"})
	store := &blockingBoundedStore{Store: base, started: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _, _, _, _ = LookupContext(ctx, store, "seed")
		close(done)
	}()
	<-store.started

	// Synchronous IndexFile/edit work needs both of these gates. The lookup's
	// deliberately blocked traversal must own neither one.
	if !store.ResolveMutex().TryLock() {
		t.Fatal("reach traversal held ResolveMutex and would starve an edit")
	}
	store.ResolveMutex().Unlock()
	start := time.Now()
	finishMutation := BeginTopologyMutation(store)
	finishMutation(false)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("topology mutation waited %s behind lock-free traversal", elapsed)
	}

	<-done
}

// openSeedAndCaller opens a SQLite store holding one seed and its caller.
func openSeedAndCaller(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "seed", Kind: graph.KindFunction, Name: "seed"},
		{ID: "caller", Kind: graph.KindFunction, Name: "caller"},
	}, []*graph.Edge{{From: "caller", To: "seed", Kind: graph.EdgeCalls, Confidence: 1}})
	return store
}

// A SQLite read runs on its own snapshot, so a resolver or enrichment pass that
// holds ResolveMutex must not make an impact lookup wait for it.
func TestLookupDoesNotWaitForSQLiteResolveMutex(t *testing.T) {
	store := openSeedAndCaller(t)
	store.ResolveMutex().Lock()
	defer store.ResolveMutex().Unlock()
	started := time.Now()
	d1, _, _, hit, truncated := LookupContext(t.Context(), store, "seed")
	if elapsed := time.Since(started); elapsed > lookupTimeout/3 {
		t.Fatalf("lookup waited %s behind ResolveMutex", elapsed)
	}
	if !hit || truncated || joinIDs(d1) != "caller" {
		t.Fatalf("d1=%v hit=%v truncated=%v, want the caller as an exact hit", d1, hit, truncated)
	}
}

// A record write rewrites the seed's whole row, so on SQLite too it waits for a
// pass that holds ResolveMutex while it rewrites rows.
func TestReachRecordWritesWaitForSQLiteResolveMutex(t *testing.T) {
	store := openSeedAndCaller(t)
	mu := store.ResolveMutex()

	mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	BuildIndexCtx(ctx, store)
	cancel()
	mu.Unlock()
	if _, _, _, hit, _ := LookupCached(store, "seed"); hit {
		t.Fatal("the eager build published a record while ResolveMutex was held")
	}

	mu.Lock()
	cleared := make(chan struct{})
	go func() {
		ClearIndex(store)
		close(cleared)
	}()
	select {
	case <-cleared:
		t.Error("ClearIndex rewrote rows while ResolveMutex was held")
	case <-time.After(100 * time.Millisecond):
	}
	mu.Unlock()
	<-cleared
}
