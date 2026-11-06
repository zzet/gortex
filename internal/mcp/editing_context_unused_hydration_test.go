package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type contextUnusedHydrationStore struct {
	*store_sqlite.Store
	unused map[string]bool
	seen   map[string]int
	budget time.Duration
}

func (s *contextUnusedHydrationStore) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	return s.Store.WithFileEditingContextReader(ctx, func(ctx context.Context, rows graph.FileEditingContextRows) error {
		return fn(ctx, contextUnusedHydrationRows{FileEditingContextRows: rows, store: s})
	})
}

type contextUnusedHydrationRows struct {
	graph.FileEditingContextRows
	store *contextUnusedHydrationStore
}

func (r contextUnusedHydrationRows) GetNodesByIDs(ids []string) map[string]*graph.Node {
	for _, id := range ids {
		if r.store.unused[id] {
			r.store.seen[id]++
		}
	}
	return r.FileEditingContextRows.GetNodesByIDs(ids)
}

// The no-writer hosted failure reports an exhausted whole-bundle scope, not
// proven SQLite contention. This isolates one avoidable part of that scope:
// classifying endpoints for edge kinds which editing context never consumes.
// It asserts work and exact payload parity, never a machine-speed threshold or
// attribution of the retained hosted failures to this particular operation.
func TestEditingContextSkipsUnconsumedEndpointHydration(t *testing.T) {
	store := newContractCoreEditingStore(t)
	unused := make(map[string]bool)
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := range 128 {
		id := fmt.Sprintf("repo/noise.go::Unused%03d", i)
		unused[id] = true
		nodes = append(nodes, &graph.Node{ID: id, Name: "Unused", Kind: graph.KindFunction, FilePath: "repo/noise.go", RepoPrefix: "repo", Meta: map[string]any{"unconsumed": strings.Repeat("unused payload", 64)}})
		edges = append(edges,
			&graph.Edge{From: "repo/a.go::F", To: id, Kind: graph.EdgeReferences},
			&graph.Edge{From: id, To: "repo/a.go::F", Kind: graph.EdgeReferences})
	}
	require.NoError(t, store.AddBatchChecked(nodes, edges))
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}
	want := graph.FileEditingContextOf(newContractCoreEdges(store, t.Context(), nil), "repo/a.go", kinds)
	calledBy, calls, imports := editingContextIDs(want)
	require.Equal(t, []string{"repo/b.go::G"}, calledBy)
	require.Equal(t, []string{"repo/e.go::K"}, calls)
	require.Equal(t, []string{"repo/z.go"}, imports)
	tracked := &contextUnusedHydrationStore{Store: store, unused: unused, seen: make(map[string]int)}
	got, err := fileEditingContextContext(t.Context(), newContractCoreEdges(tracked, t.Context(), nil), "repo/a.go", kinds)
	require.NoError(t, err, "real coherent SQL scope must finish before evaluating the work oracle")
	require.Equal(t, want, got, "all definitions/imports/filtered callers and callees stay exact")
	require.Positive(t, tracked.budget)
	require.LessOrEqual(t, tracked.budget, 30*time.Millisecond, "the original store scope is unchanged")
	t.Logf("REAL_CANONICAL_CONTEXT_SCOPE unused_endpoint_population=%d hydrated_unused_identities=%d original_scope_budget=%s payload_parity=true writers=0", len(unused), len(tracked.seen), tracked.budget)
	require.Empty(t, tracked.seen, "do not hydrate endpoints on non-import/non-call edges that this bundle discards")
}
