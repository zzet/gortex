package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const generationEvictionRepo = "generation-eviction-repo"

type generationEvictionRows struct {
	ids   []string
	sites []graph.SemanticBindingSite
}

func openGenerationEvictionStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "generation-eviction.sqlite"))
	if err != nil {
		tb.Fatalf("open generation eviction store: %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	return store
}

func beginGenerationEvictionHandle(tb testing.TB, store *Store, ordinal int) (int64, *Store) {
	tb.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
		OwnerKind:         "dedicated_graph",
		GraphID:           "generation-eviction-graph",
		LayerID:           fmt.Sprintf("generation-eviction-layer-%d", ordinal),
		GenerationKind:    "commit",
		TreeOID:           fmt.Sprintf("tree-%d", ordinal),
		ConfigHash:        "config",
		ExtractorVersions: `{"go":"1"}`,
		ResolverVersion:   "resolver",
		CreatedAt:         int64(100 + ordinal),
	})
	if err != nil {
		tb.Fatalf("begin payload generation %d: %v", ordinal, err)
	}
	return generationID, handle
}

func seedGenerationEvictionRows(tb testing.TB, handle *Store, _ int64, count int) generationEvictionRows {
	tb.Helper()
	nodes := make([]*graph.Node, 0, count)
	edges := make([]*graph.Edge, 0, max(0, count-1))
	rows := generationEvictionRows{
		ids:   make([]string, 0, count),
		sites: make([]graph.SemanticBindingSite, 0, count),
	}
	bindings := make([]graph.SemanticBindingType, 0, count)
	for i := range count {
		name := fmt.Sprintf("Symbol%04d", i)
		filePath := fmt.Sprintf("%s::shared/file-%04d.go", generationEvictionRepo, i)
		id := fmt.Sprintf("%s::%s", filePath, name)
		rows.ids = append(rows.ids, id)
		site := graph.SemanticBindingSite{
			RepoPrefix: generationEvictionRepo,
			FilePath:   filePath,
			Line:       i + 1,
			Name:       name,
		}
		rows.sites = append(rows.sites, site)
		bindings = append(bindings, graph.SemanticBindingType{
			Site: site, TypeName: fmt.Sprintf("type-%04d", i),
		})
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, RepoPrefix: generationEvictionRepo, Language: "go",
		})
		if i > 0 {
			edges = append(edges, &graph.Edge{
				From: rows.ids[i-1], To: id, Kind: graph.EdgeCalls,
				FilePath: nodes[i-1].FilePath, Line: i,
			})
		}
	}
	handle.AddBatch(nodes, edges)
	if err := handle.ReplaceSemanticBindingTypes(generationEvictionRepo, bindings); err != nil {
		tb.Fatalf("seed semantic binding types: %v", err)
	}
	return rows
}

func requireGenerationEvictionRows(tb testing.TB, handle *Store, rows generationEvictionRows, present bool) {
	tb.Helper()
	for _, id := range rows.ids {
		if got := handle.GetNode(id); (got != nil) != present {
			tb.Fatalf("node %s presence = %v, want %v", id, got != nil, present)
		}
	}
	bindings, err := handle.SemanticBindingTypes(rows.sites)
	if err != nil {
		tb.Fatalf("read semantic binding types: %v", err)
	}
	wantBindings := 0
	if present {
		wantBindings = len(rows.sites)
	}
	if len(bindings) != wantBindings {
		tb.Fatalf("semantic binding count = %d, want %d for generation %d", len(bindings), wantBindings, handle.ViewGeneration())
	}
}

func generationEvictionHandles(tb testing.TB, store *Store, count int) ([]int64, []*Store) {
	tb.Helper()
	generations := make([]int64, 0, count)
	handles := make([]*Store, 0, count)
	generations = append(generations, 0)
	handles = append(handles, store)
	for ordinal := 1; ordinal < count; ordinal++ {
		generationID, handle := beginGenerationEvictionHandle(tb, store, ordinal)
		generations = append(generations, generationID)
		handles = append(handles, handle)
	}
	return generations, handles
}

func TestEvictRepoDefaultsToCurrentGeneration(t *testing.T) {
	store := openGenerationEvictionStore(t)
	generations, handles := generationEvictionHandles(t, store, 4)
	rows := make([]generationEvictionRows, len(handles))
	for i, handle := range handles {
		rows[i] = seedGenerationEvictionRows(t, handle, generations[i], 4)
	}

	nodesRemoved, edgesRemoved := store.EvictRepo(generationEvictionRepo)
	if nodesRemoved != 4 || edgesRemoved != 3 {
		t.Fatalf("current-generation eviction removed nodes=%d edges=%d, want 4/3", nodesRemoved, edgesRemoved)
	}
	requireGenerationEvictionRows(t, store, rows[0], false)
	for i := 1; i < len(handles); i++ {
		requireGenerationEvictionRows(t, handles[i], rows[i], true)
	}

	nodesRemoved, edgesRemoved = store.EvictRepoAllGenerations(generationEvictionRepo)
	if nodesRemoved != 12 || edgesRemoved != 9 {
		t.Fatalf("all-generation eviction removed nodes=%d edges=%d, want 12/9", nodesRemoved, edgesRemoved)
	}
	for i, handle := range handles {
		requireGenerationEvictionRows(t, handle, rows[i], false)
	}
}

func TestPurgeRepoRemovesPayloadAcrossAllGenerations(t *testing.T) {
	store := openGenerationEvictionStore(t)
	generations, handles := generationEvictionHandles(t, store, 4)
	rows := make([]generationEvictionRows, len(handles))
	for i, handle := range handles {
		rows[i] = seedGenerationEvictionRows(t, handle, generations[i], 4)
	}

	if err := store.PurgeRepo(generationEvictionRepo); err != nil {
		t.Fatalf("purge repository: %v", err)
	}
	for i, handle := range handles {
		requireGenerationEvictionRows(t, handle, rows[i], false)
	}
}

func TestEvictRepoAllGenerationsRefusesEmptyPrefix(t *testing.T) {
	store := openGenerationEvictionStore(t)
	_, payload := beginGenerationEvictionHandle(t, store, 1)
	const nodeID = "shared/global.go::GlobalExternal"
	for _, handle := range []*Store{store, payload} {
		handle.AddNode(&graph.Node{
			ID: nodeID, Kind: graph.KindFunction, Name: "GlobalExternal",
			FilePath: "shared/global.go", RepoPrefix: "", Language: "go",
		})
	}

	nodesRemoved, edgesRemoved := store.EvictRepoAllGenerations("")
	if nodesRemoved != 0 || edgesRemoved != 0 {
		t.Fatalf("empty-prefix eviction removed nodes=%d edges=%d, want 0/0", nodesRemoved, edgesRemoved)
	}
	if err := store.PurgeRepo(""); err == nil {
		t.Fatal("PurgeRepo accepted an empty prefix")
	}
	for _, handle := range []*Store{store, payload} {
		if got := handle.GetNode(nodeID); got == nil {
			t.Fatalf("empty-prefix guard removed generation %d node", handle.ViewGeneration())
		}
	}
}

func BenchmarkRepoEvictionScope(b *testing.B) {
	const (
		generationCount = 8
		nodesPerGen     = 256
	)

	b.Run("current_generation", func(b *testing.B) {
		b.StopTimer()
		store := openGenerationEvictionStore(b)
		generations, handles := generationEvictionHandles(b, store, generationCount)
		for i := 1; i < len(handles); i++ {
			seedGenerationEvictionRows(b, handles[i], generations[i], nodesPerGen)
		}
		b.ReportAllocs()
		b.ReportMetric(1, "generations/op")
		b.ReportMetric(nodesPerGen, "nodes/op")
		for range b.N {
			seedGenerationEvictionRows(b, store, 0, nodesPerGen)
			b.StartTimer()
			nodesRemoved, _ := store.EvictRepoCurrentGeneration(generationEvictionRepo)
			b.StopTimer()
			if nodesRemoved != nodesPerGen {
				b.Fatalf("removed %d nodes, want %d", nodesRemoved, nodesPerGen)
			}
		}
	})

	b.Run("all_generations", func(b *testing.B) {
		b.StopTimer()
		store := openGenerationEvictionStore(b)
		generations, handles := generationEvictionHandles(b, store, generationCount)
		b.ReportAllocs()
		b.ReportMetric(generationCount, "generations/op")
		b.ReportMetric(generationCount*nodesPerGen, "nodes/op")
		for range b.N {
			for i, handle := range handles {
				seedGenerationEvictionRows(b, handle, generations[i], nodesPerGen)
			}
			b.StartTimer()
			nodesRemoved, _ := store.EvictRepoAllGenerations(generationEvictionRepo)
			b.StopTimer()
			if nodesRemoved != generationCount*nodesPerGen {
				b.Fatalf("removed %d nodes, want %d", nodesRemoved, generationCount*nodesPerGen)
			}
		}
	})
}
