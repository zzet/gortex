package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveIncomingForNames is the receipt-exact eviction companion: it must
// reach pending references parked under BOTH stub forms (bare and
// repo-prefixed) for a name no frontier file declares anymore.
func TestResolveIncomingForNamesRebindsBothStubForms(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	prefixed := &graph.Edge{From: "repo/d.go::CallerB", To: "repo::" + graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/d.go", Line: 4}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/d.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repo/d.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare, prefixed})

	r := New(g)
	stats := r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if bare.To != "repo/b.go::Target" {
		t.Fatalf("bare-stub edge target = %q, want repo/b.go::Target", bare.To)
	}
	if prefixed.To != "repo/b.go::Target" {
		t.Fatalf("prefixed-stub edge target = %q, want repo/b.go::Target", prefixed.To)
	}
	if stats == nil {
		t.Fatal("nil stats")
	}
}

func TestResolveIncomingForNamesEmptyInputsAreNoOps(t *testing.T) {
	r := New(graph.New())
	if stats := r.ResolveIncomingForNames(nil, []string{"repo"}); stats == nil {
		t.Fatal("nil stats for empty names")
	}
	if stats := r.ResolveIncomingForNames([]string{""}, nil); stats == nil {
		t.Fatal("nil stats for blank name")
	}
}

// Member references park under the wildcard stub forms
// (`unresolved::*.<Name>` and `<repo>::unresolved::*.<Name>`) — the other two
// of graph.UnresolvedNameCandidateIDs' four name-owned forms. The names pass
// must enumerate them too, or member references outside every receipt file
// frontier stay pending forever once the whole-graph fallback is gone.
func TestResolveIncomingForNamesRebindsWildcardMemberStubs(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	prefixed := &graph.Edge{From: "repo/d.go::CallerB", To: "repo::" + graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/d.go", Line: 4}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/d.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repo/d.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::T.Target", Kind: graph.KindMethod, Name: "Target", QualName: "T.Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare, prefixed})

	r := New(g)
	r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if bare.To != "repo/b.go::T.Target" {
		t.Fatalf("bare wildcard-stub edge target = %q, want repo/b.go::T.Target", bare.To)
	}
	if prefixed.To != "repo/b.go::T.Target" {
		t.Fatalf("prefixed wildcard-stub edge target = %q, want repo/b.go::T.Target", prefixed.To)
	}
}

// A wildcard member stub with two same-name method candidates on different
// receivers must stay parked: the names pass runs resolveEdge with its gates
// unchanged, so a still-ambiguous member reference binds no differently than
// it would on any other pass. Paired with the wildcard-rebind test above,
// this pins that enumerating the wildcard forms adds reach, not laxity.
func TestResolveIncomingForNamesAmbiguousWildcardStaysUnresolved(t *testing.T) {
	pending := &graph.Edge{From: "repo/c.go::Caller", To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/x/a.go::X.Target", Kind: graph.KindMethod, Name: "Target", QualName: "X.Target", FilePath: "repo/x/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/y/b.go::Y.Target", Kind: graph.KindMethod, Name: "Target", QualName: "Y.Target", FilePath: "repo/y/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{pending})

	r := New(g)
	r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if !graph.IsUnresolvedTarget(pending.To) {
		t.Fatalf("ambiguous member reference bound to %q, must stay parked", pending.To)
	}
}

// incomingNamesCountingStore records every GetInEdgesByNodeIDs batch so a
// test can assert how often the pending frontier is materialized.
type incomingNamesCountingStore struct {
	graph.Store
	inEdgeBatches [][]string
}

func (s *incomingNamesCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inEdgeBatches = append(s.inEdgeBatches, append([]string(nil), ids...))
	return s.Store.GetInEdgesByNodeIDs(ids)
}

// The successful path must not pay for the pending frontier twice: the
// probe's materialized read is the same batch the resolution helper needs,
// so the stub-key batch may hit the store exactly once. On SQLite the
// duplicate read doubled both time and allocations at scale.
func TestResolveIncomingForNamesHitPathReadsPendingEdgesOnce(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare})

	store := &incomingNamesCountingStore{Store: g}
	r := New(store)
	r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})

	if bare.To != "repo/b.go::Target" {
		t.Fatalf("hit-path edge target = %q, want repo/b.go::Target", bare.To)
	}
	stubBatches := 0
	for _, batch := range store.inEdgeBatches {
		for _, id := range batch {
			if id == graph.UnresolvedMarker+"Target" {
				stubBatches++
				break
			}
		}
	}
	if stubBatches != 1 {
		t.Fatalf("stub-key frontier materialized %d times, want exactly 1", stubBatches)
	}
}

// The receipt consumers call the names pass on every apply, usually with no
// pending edge parked under any requested name. That call must cost a probe,
// not a graph-wide index build - this benchmark is the regression guard for
// the probe-first fast path.
// BenchmarkResolveIncomingForNamesPending guards the hit path: pending
// edges parked under the requested name must be materialized from the store
// exactly once per call. The two candidates keep every edge legitimately
// ambiguous, so each iteration repeats the same full read-and-refuse work
// instead of draining the frontier on the first pass.
func BenchmarkResolveIncomingForNamesPending(b *testing.B) {
	g := graph.New()
	nodes := []*graph.Node{
		{ID: "repo/x/a.go::X.Target", Kind: graph.KindMethod, Name: "Target", QualName: "X.Target", FilePath: "repo/x/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/y/b.go::Y.Target", Kind: graph.KindMethod, Name: "Target", QualName: "Y.Target", FilePath: "repo/y/b.go", RepoPrefix: "repo", Language: "go"},
	}
	edges := make([]*graph.Edge, 0, 2000)
	for i := 0; i < 2000; i++ {
		caller := fmt.Sprintf("repo/c%d.go::Caller%d", i, i)
		nodes = append(nodes, &graph.Node{
			ID: caller, Kind: graph.KindFunction, Name: fmt.Sprintf("Caller%d", i),
			FilePath: fmt.Sprintf("repo/c%d.go", i), RepoPrefix: "repo", Language: "go",
		})
		edges = append(edges, &graph.Edge{
			From: caller, To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls,
			FilePath: fmt.Sprintf("repo/c%d.go", i), Line: 3,
		})
	}
	g.AddBatch(nodes, edges)
	r := New(g)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ResolveIncomingForNames([]string{"Target"}, []string{"repo"})
	}
}

func BenchmarkResolveIncomingForNamesNoPending(b *testing.B) {
	g := graph.New()
	nodes := make([]*graph.Node, 0, 2000)
	for i := 0; i < 2000; i++ {
		nodes = append(nodes, &graph.Node{
			ID:         fmt.Sprintf("repo/f%d.go::Fn%d", i, i),
			Kind:       graph.KindFunction,
			Name:       fmt.Sprintf("Fn%d", i),
			FilePath:   fmt.Sprintf("repo/f%d.go", i),
			RepoPrefix: "repo",
			Language:   "go",
		})
	}
	g.AddBatch(nodes, nil)
	r := New(g)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ResolveIncomingForNames([]string{"NoSuchName"}, []string{"repo"})
	}
}

// The no-pending fast path still pays one probe read whose batch is
// len(names) x 4 stub forms x len(prefixes), and the single-name benchmark
// above cannot see that. This is the shape the receipt consumers actually
// produced before the name pass narrowed to evicted names: a whole batch of
// added definitions, each contributing a Name and a QualName, against every
// tracked repository prefix. It exists so the probe's cost stays a function of
// the evictions and not of the batch.
func BenchmarkResolveIncomingForNamesNoPendingManyNames(b *testing.B) {
	g := graph.New()
	nodes := make([]*graph.Node, 0, 4000)
	edges := make([]*graph.Edge, 0, 4000)
	for i := 0; i < 4000; i++ {
		caller := fmt.Sprintf("repo/c%d.go::Caller%d", i, i)
		nodes = append(nodes, &graph.Node{
			ID: caller, Kind: graph.KindFunction, Name: fmt.Sprintf("Caller%d", i),
			FilePath: fmt.Sprintf("repo/c%d.go", i), RepoPrefix: "repo", Language: "go",
		})
		edges = append(edges, &graph.Edge{
			From: caller, To: graph.UnresolvedMarker + fmt.Sprintf("Pending%d", i), Kind: graph.EdgeCalls,
			FilePath: fmt.Sprintf("repo/c%d.go", i), Line: 3,
		})
	}
	g.AddBatch(nodes, edges)
	r := New(g)

	names := make([]string, 0, 2000)
	for i := 0; i < 1000; i++ {
		names = append(names, fmt.Sprintf("Added%d", i), fmt.Sprintf("pkg.Added%d", i))
	}
	prefixes := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		prefixes = append(prefixes, fmt.Sprintf("repo%d", i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ResolveIncomingForNames(names, prefixes)
	}
}

// The batched file frontier is the third stub-key builder, and it owns the
// incoming leg of every incremental resolve. A definition a frontier file
// declares must have its pending references reached under all four name-owned
// forms, exactly as the names pass does: the wildcard member forms are how a
// member reference with an unknown receiver parks, and a file pass that
// enumerates only the bare forms strands them until the next whole-graph
// resolve. This is what lets the names pass narrow to evicted names.
func TestResolveFilesAndIncomingRebindsWildcardMemberStubs(t *testing.T) {
	bare := &graph.Edge{From: "repo/c.go::CallerA", To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	prefixed := &graph.Edge{From: "repo/d.go::CallerB", To: "repo::" + graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/d.go", Line: 4}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::CallerA", Kind: graph.KindFunction, Name: "CallerA", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/d.go::CallerB", Kind: graph.KindFunction, Name: "CallerB", FilePath: "repo/d.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/b.go::T.Target", Kind: graph.KindMethod, Name: "Target", QualName: "T.Target", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{bare, prefixed})

	New(g).ResolveFilesAndIncoming([]string{"repo/b.go"})

	if bare.To != "repo/b.go::T.Target" {
		t.Fatalf("bare wildcard-stub edge target = %q, want repo/b.go::T.Target", bare.To)
	}
	if prefixed.To != "repo/b.go::T.Target" {
		t.Fatalf("prefixed wildcard-stub edge target = %q, want repo/b.go::T.Target", prefixed.To)
	}
}

// The laxity control for the widened file frontier, mirroring the names-pass
// pair: two same-name methods on different receivers keep a wildcard member
// stub parked. Enumerating the wildcard forms adds reach, not laxity.
func TestResolveFilesAndIncomingAmbiguousWildcardStaysUnresolved(t *testing.T) {
	pending := &graph.Edge{From: "repo/c.go::Caller", To: graph.UnresolvedMarker + "*.Target", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 3}
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/c.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/x/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/x/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/x/a.go::X.Target", Kind: graph.KindMethod, Name: "Target", QualName: "X.Target", FilePath: "repo/x/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/y/b.go::Y.Target", Kind: graph.KindMethod, Name: "Target", QualName: "Y.Target", FilePath: "repo/y/b.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{pending})

	New(g).ResolveFilesAndIncoming([]string{"repo/x/a.go"})

	if pending.To != graph.UnresolvedMarker+"*.Target" {
		t.Fatalf("ambiguous wildcard-stub edge target = %q, want it to stay parked", pending.To)
	}
}
