package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openTerminalStampTestStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := Open(":memory:")
	if err != nil {
		tb.Fatalf("Open(:memory:): %v", err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("Close(): %v", err)
		}
	})
	return store
}

func terminalStampTestEdge(from, to, file string, line int) *graph.Edge {
	return &graph.Edge{
		From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
	}
}

func findTerminalStampTestEdge(t *testing.T, store *Store, identity graph.EdgeIdentity) *graph.Edge {
	t.Helper()
	for _, edge := range store.GetOutEdges(identity.From) {
		if graph.EdgeIdentityFor(edge) == identity {
			return edge
		}
	}
	t.Fatalf("edge not found: %+v", identity)
	return nil
}

func TestPersistEdgeTerminalStampsIsExactAndNarrow(t *testing.T) {
	store := openTerminalStampTestStore(t)
	stored := terminalStampTestEdge("caller", "unresolved::Missing", "a.go", 11)
	stored.Confidence = 0.91
	stored.ConfidenceLabel = "high"
	stored.Origin = "ast"
	stored.Tier = "ast"
	stored.Meta = map[string]any{
		"keep":            "preserved",
		"semantic_source": "scip",
	}
	neighbor := terminalStampTestEdge("caller", "unresolved::Missing", "a.go", 12)
	store.AddEdge(stored)
	store.AddEdge(neighbor)

	identity := graph.EdgeIdentityFor(stored)
	update := terminalStampTestEdge(identity.From, identity.To, identity.FilePath, identity.Line)
	update.Confidence = 0.01
	update.ConfidenceLabel = "wrong"
	update.Origin = "wrong"
	update.Tier = "wrong"
	update.Meta = map[string]any{
		graph.EdgeMetaResolveTerminal:       true,
		graph.EdgeMetaResolveTerminalReason: "no_definition",
		"must_not_persist":                  "ignored",
	}

	beforeRevision := store.AnalysisMutationRevision()
	stats, err := store.persistEdgeTerminalStamps([]*graph.Edge{update})
	if err != nil {
		t.Fatalf("persistEdgeTerminalStamps(): %v", err)
	}
	if stats.statements != 1 || stats.changedRows != 1 {
		t.Fatalf("stats = %+v, want statements=1 changedRows=1", stats)
	}
	if got := store.AnalysisMutationRevision(); got != beforeRevision+1 {
		t.Fatalf("analysis revision = %d, want %d", got, beforeRevision+1)
	}

	got := findTerminalStampTestEdge(t, store, identity)
	if got.Confidence != stored.Confidence || got.ConfidenceLabel != stored.ConfidenceLabel ||
		got.Origin != stored.Origin || got.Tier != stored.Tier {
		t.Fatalf("non-terminal attributes changed: got=%+v stored=%+v", got, stored)
	}
	if got.Meta["keep"] != "preserved" || got.Meta["semantic_source"] != "scip" {
		t.Fatalf("existing metadata changed: %#v", got.Meta)
	}
	if _, ok := got.Meta["must_not_persist"]; ok {
		t.Fatalf("unrelated update metadata persisted: %#v", got.Meta)
	}
	if terminal, _ := got.Meta[graph.EdgeMetaResolveTerminal].(bool); !terminal {
		t.Fatalf("terminal stamp missing: %#v", got.Meta)
	}
	if reason, _ := got.Meta[graph.EdgeMetaResolveTerminalReason].(string); reason != "no_definition" {
		t.Fatalf("terminal reason = %q, want no_definition", reason)
	}

	neighborGot := findTerminalStampTestEdge(t, store, graph.EdgeIdentityFor(neighbor))
	if _, ok := neighborGot.Meta[graph.EdgeMetaResolveTerminal]; ok {
		t.Fatalf("neighbor with different exact identity was modified: %#v", neighborGot.Meta)
	}

	idempotentRevision := store.AnalysisMutationRevision()
	stats, err = store.persistEdgeTerminalStamps([]*graph.Edge{update})
	if err != nil {
		t.Fatalf("idempotent persistEdgeTerminalStamps(): %v", err)
	}
	if stats.changedRows != 0 {
		t.Fatalf("idempotent changedRows = %d, want 0", stats.changedRows)
	}
	if got := store.AnalysisMutationRevision(); got != idempotentRevision {
		t.Fatalf("idempotent update advanced analysis revision: got %d want %d", got, idempotentRevision)
	}

	clear := terminalStampTestEdge(identity.From, identity.To, identity.FilePath, identity.Line)
	clear.Meta = map[string]any{"must_not_persist": "ignored"}
	stats, err = store.persistEdgeTerminalStamps([]*graph.Edge{clear})
	if err != nil {
		t.Fatalf("clear persistEdgeTerminalStamps(): %v", err)
	}
	if stats.changedRows != 1 {
		t.Fatalf("clear changedRows = %d, want 1", stats.changedRows)
	}
	got = findTerminalStampTestEdge(t, store, identity)
	if _, ok := got.Meta[graph.EdgeMetaResolveTerminal]; ok {
		t.Fatalf("terminal stamp survived clear: %#v", got.Meta)
	}
	if _, ok := got.Meta[graph.EdgeMetaResolveTerminalReason]; ok {
		t.Fatalf("terminal reason survived clear: %#v", got.Meta)
	}
	if got.Meta["keep"] != "preserved" || got.Meta["semantic_source"] != "scip" {
		t.Fatalf("clear changed unrelated metadata: %#v", got.Meta)
	}
}

func TestPersistEdgeTerminalStampsLastDuplicateWins(t *testing.T) {
	store := openTerminalStampTestStore(t)
	stored := terminalStampTestEdge("caller", "unresolved::Duplicate", "dup.go", 7)
	store.AddEdge(stored)
	identity := graph.EdgeIdentityFor(stored)

	first := terminalStampTestEdge(identity.From, identity.To, identity.FilePath, identity.Line)
	first.Meta = map[string]any{
		graph.EdgeMetaResolveTerminal:       true,
		graph.EdgeMetaResolveTerminalReason: "first",
	}
	last := terminalStampTestEdge(identity.From, identity.To, identity.FilePath, identity.Line)
	last.Meta = map[string]any{
		graph.EdgeMetaResolveTerminal:       true,
		graph.EdgeMetaResolveTerminalReason: "last",
	}
	stats, err := store.persistEdgeTerminalStamps([]*graph.Edge{first, last})
	if err != nil {
		t.Fatalf("persistEdgeTerminalStamps(): %v", err)
	}
	if stats.statements != 1 || stats.changedRows != 1 {
		t.Fatalf("stats = %+v, want one deduplicated statement/row", stats)
	}
	got := findTerminalStampTestEdge(t, store, identity)
	if reason, _ := got.Meta[graph.EdgeMetaResolveTerminalReason].(string); reason != "last" {
		t.Fatalf("terminal reason = %q, want last", reason)
	}

	acrossChunks := make([]*graph.Edge, edgeTerminalStampChunkSize+1)
	acrossChunks[0] = first
	for i := 1; i < edgeTerminalStampChunkSize; i++ {
		acrossChunks[i] = terminalStampTestEdge("missing", fmt.Sprintf("unresolved::%d", i), "missing.go", i)
	}
	lastAcrossChunk := terminalStampTestEdge(identity.From, identity.To, identity.FilePath, identity.Line)
	lastAcrossChunk.Meta = map[string]any{
		graph.EdgeMetaResolveTerminal:       true,
		graph.EdgeMetaResolveTerminalReason: "last-across-chunk",
	}
	acrossChunks[edgeTerminalStampChunkSize] = lastAcrossChunk
	stats, err = store.persistEdgeTerminalStamps(acrossChunks)
	if err != nil {
		t.Fatalf("cross-chunk persistEdgeTerminalStamps(): %v", err)
	}
	if stats.statements != 2 {
		t.Fatalf("cross-chunk statements = %d, want 2", stats.statements)
	}
	got = findTerminalStampTestEdge(t, store, identity)
	if reason, _ := got.Meta[graph.EdgeMetaResolveTerminalReason].(string); reason != "last-across-chunk" {
		t.Fatalf("cross-chunk terminal reason = %q, want last-across-chunk", reason)
	}
}

func TestPersistEdgeTerminalStampsBoundsStatementsAndNoMatchIsNoop(t *testing.T) {
	store := openTerminalStampTestStore(t)
	updates := make([]*graph.Edge, edgeTerminalStampChunkSize*2+1)
	for i := range updates {
		updates[i] = terminalStampTestEdge("missing", fmt.Sprintf("unresolved::%d", i), "missing.go", i+1)
		updates[i].Meta = map[string]any{graph.EdgeMetaResolveTerminal: true}
	}
	beforeRevision := store.AnalysisMutationRevision()
	stats, err := store.persistEdgeTerminalStamps(updates)
	if err != nil {
		t.Fatalf("persistEdgeTerminalStamps(): %v", err)
	}
	if stats.statements != 3 {
		t.Fatalf("statements = %d, want 3 for %d rows", stats.statements, len(updates))
	}
	if stats.changedRows != 0 {
		t.Fatalf("changedRows = %d, want 0", stats.changedRows)
	}
	if got := store.AnalysisMutationRevision(); got != beforeRevision {
		t.Fatalf("no-match update advanced analysis revision: got %d want %d", got, beforeRevision)
	}
}

var (
	terminalStampBenchmarkQuery string
	terminalStampBenchmarkArgs  []any
)

func terminalStampBenchmarkEdges(count int) []*graph.Edge {
	edges := make([]*graph.Edge, count)
	for i := range edges {
		edges[i] = terminalStampTestEdge("caller", fmt.Sprintf("unresolved::Target%d", i), "bench.go", i+1)
		edges[i].Confidence = 0.8
		edges[i].ConfidenceLabel = "high"
		edges[i].Origin = "ast"
		edges[i].Tier = "ast"
		edges[i].Meta = map[string]any{
			graph.EdgeMetaResolveTerminal:       true,
			graph.EdgeMetaResolveTerminalReason: "no_definition",
			"receiver":                          "Service",
			"signature":                         "func(context.Context) error",
			"provider":                          "tree-sitter",
			"return_usage":                      "assigned",
			"generic_args":                      []string{"T", "E"},
		}
	}
	return edges
}

func TestEdgeTerminalStampStatementUsesFewerAllocationsThanFullAttributes(t *testing.T) {
	edges := terminalStampBenchmarkEdges(75)
	narrowAllocs := testing.AllocsPerRun(20, func() {
		terminalStampBenchmarkQuery, terminalStampBenchmarkArgs = edgeTerminalStampUpdateStatement(baseViewGeneration, edges)
	})
	fullAllocs := testing.AllocsPerRun(20, func() {
		var err error
		terminalStampBenchmarkQuery, terminalStampBenchmarkArgs, err = edgeAttributeUpdateStatement(baseViewGeneration, edges)
		if err != nil {
			panic(err)
		}
	})
	if narrowAllocs >= fullAllocs {
		t.Fatalf("terminal-only allocations = %.0f, want fewer than full-attribute %.0f", narrowAllocs, fullAllocs)
	}
	t.Logf("allocations per 75-row statement: terminal-only=%.0f full-attributes=%.0f", narrowAllocs, fullAllocs)
}

func BenchmarkEdgeTerminalStampStatements(b *testing.B) {
	edges := terminalStampBenchmarkEdges(75)
	b.Run("terminal_only_75", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(len(edges)), "rows/op")
		for i := 0; i < b.N; i++ {
			terminalStampBenchmarkQuery, terminalStampBenchmarkArgs = edgeTerminalStampUpdateStatement(baseViewGeneration, edges)
		}
	})
	b.Run("full_attributes_75", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(len(edges)), "rows/op")
		for i := 0; i < b.N; i++ {
			var err error
			terminalStampBenchmarkQuery, terminalStampBenchmarkArgs, err = edgeAttributeUpdateStatement(baseViewGeneration, edges)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkPersistEdgeTerminalStamps(b *testing.B) {
	for _, tc := range []struct {
		name    string
		persist func(*Store, []*graph.Edge) error
	}{
		{
			name: "terminal_only_140",
			persist: func(store *Store, edges []*graph.Edge) error {
				_, err := store.persistEdgeTerminalStamps(edges)
				return err
			},
		},
		{
			name: "full_attributes_140",
			persist: func(store *Store, edges []*graph.Edge) error {
				_, err := store.persistEdgeAttributesBatch(edges)
				return err
			},
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			store := openTerminalStampTestStore(b)
			edges := terminalStampBenchmarkEdges(edgeTerminalStampChunkSize)
			for _, edge := range edges {
				store.AddEdge(edge)
			}
			b.ReportAllocs()
			b.ReportMetric(float64(len(edges)), "rows/op")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, edge := range edges {
					if i%2 == 0 {
						edge.Meta[graph.EdgeMetaResolveTerminal] = true
						edge.Meta[graph.EdgeMetaResolveTerminalReason] = "no_definition"
					} else {
						delete(edge.Meta, graph.EdgeMetaResolveTerminal)
						delete(edge.Meta, graph.EdgeMetaResolveTerminalReason)
					}
				}
				if err := tc.persist(store, edges); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
