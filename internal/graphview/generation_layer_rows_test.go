package graphview

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// writeRowsGeneration publishes a small generation whose rows exercise every
// ordering a row read has: several out-edges of one source on one line and on
// different lines, several kinds into one target, and edges to a target the
// generation does not carry. (A published generation cannot hold payload at a
// context path, so the serve filter is exercised by the stack fixture only.)
func writeRowsGeneration(t *testing.T, store *store_sqlite.Store) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "layer-rows",
		CheckoutID: testCheckoutID, GenerationKind: "dedicated", TreeOID: "tree-rows", CreatedAt: 900,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	const file = "repo/rows.go"
	var nodes []*graph.Node
	for i := 0; i < 12; i++ {
		nodes = append(nodes, stackSymbol(fmt.Sprintf("%s::R%d", file, i), fmt.Sprintf("R%d", i), graph.KindFunction, file, 10+i))
	}
	var edges []*graph.Edge
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeCalls, graph.EdgeReferences}
	for i := 0; i < 12; i++ {
		from := fmt.Sprintf("%s::R%d", file, i)
		for k := 0; k < 4; k++ {
			to := fmt.Sprintf("%s::R%d", file, (i+k+1)%12)
			// Lines descend within a source and repeat, so id order, line
			// order and kind order all differ.
			edges = append(edges, stackEdge(from, to, kinds[(i+k)%4], file, 50-k/2))
		}
		edges = append(edges, stackEdge(from, "unresolved::Missing", graph.EdgeCalls, file, 60))
	}
	handle.AddBatch(nodes, edges)
	if err := handle.SetFileMasks([]store_sqlite.FileMask{
		{RepoPrefix: stackRepo, FilePath: file, Mode: store_sqlite.OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 950); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}
	return generationID
}

// orderedEdges renders edges in the order they were returned.
func orderedEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, renderEdge(e))
	}
	return out
}

func sameOrdered(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s:\n preloaded %v\n sql       %v", what, got, want)
	}
}

// A layer answering from preloaded rows answers every row read exactly as
// its SQL reads do: the same rows, the same order, the same serve filter.
func TestGenerationRowsAnswerLikeTheirSQLReads(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "generation-rows")
	commit, dirty := seedRoutedStack(t, store)
	rowsGen := writeRowsGeneration(t, store)
	for _, generation := range []int64{commit, dirty, rowsGen} {
		handle := store.AtGeneration(generation)
		viaSQL, err := NewGenerationLayer(handle)
		if err != nil {
			t.Fatal(err)
		}
		preloaded, err := NewGenerationLayer(store.AtGeneration(generation))
		if err != nil {
			t.Fatal(err)
		}
		rows := loadGenerationRows(ctx, store.AtGeneration(generation))
		if rows == nil {
			t.Fatalf("generation %d: a small generation was not preloaded", generation)
		}
		preloaded.rowsRef.rows.Store(rows)

		ids := stackProbeIDs()
		for _, n := range handle.AllNodes() {
			ids = append(ids, n.ID)
		}
		for _, e := range handle.AllEdges() {
			ids = append(ids, e.From, e.To)
		}
		ids = append(ids, "repo/rows.go::Absent")
		for _, id := range ids {
			sameOrdered(t, fmt.Sprintf("gen %d NodeByID(%s)", generation, id),
				[]string{renderNode(preloaded.NodeByID(id))}, []string{renderNode(viaSQL.NodeByID(id))})
			sameOrdered(t, fmt.Sprintf("gen %d OutEdges(%s)", generation, id),
				orderedEdges(preloaded.OutEdges(id)), orderedEdges(viaSQL.OutEdges(id)))
			sameOrdered(t, fmt.Sprintf("gen %d InEdges(%s)", generation, id),
				orderedEdges(preloaded.InEdges(id)), orderedEdges(viaSQL.InEdges(id)))
			if got, want := preloaded.OwnsNodeIdentity(id), viaSQL.OwnsNodeIdentity(id); got != want {
				t.Fatalf("gen %d OwnsNodeIdentity(%s) = %v, sql %v", generation, id, got, want)
			}
		}
		got, want := preloaded.GetOutEdgesByNodeIDs(ids), viaSQL.GetOutEdgesByNodeIDs(ids)
		if len(got) != len(want) {
			t.Fatalf("gen %d GetOutEdgesByNodeIDs: %d entries, sql %d", generation, len(got), len(want))
		}
		for id := range want {
			if _, ok := got[id]; !ok {
				t.Fatalf("gen %d GetOutEdgesByNodeIDs lost the entry of %s", generation, id)
			}
			sameOrdered(t, fmt.Sprintf("gen %d GetOutEdgesByNodeIDs[%s]", generation, id),
				orderedEdges(got[id]), orderedEdges(want[id]))
		}
	}
}

// Rows are shared across requests: what a caller does to an answer never
// reaches the next caller.
func TestGenerationRowsHandOutCopies(t *testing.T) {
	store := openStackStore(t, "generation-rows-copies")
	seedRoutedStack(t, store)
	generation := writeRowsGeneration(t, store)
	layer, err := NewGenerationLayer(store.AtGeneration(generation))
	if err != nil {
		t.Fatal(err)
	}
	layer.rowsRef.rows.Store(loadGenerationRows(context.Background(), store.AtGeneration(generation)))
	id := "repo/rows.go::R1"
	first := layer.OutEdges(id)
	first[0].Kind = "mutated"
	layer.NodeByID(id).Name = "mutated"
	if layer.OutEdges(id)[0].Kind == "mutated" || layer.NodeByID(id).Name == "mutated" {
		t.Fatal("a caller's change to an answer reached the shared rows")
	}
}

// WarmRoute preloads the route's newest generation, so the first view over the
// route answers its row reads from memory, and the view reads what an
// uncached one does.
func TestWarmRoutePreloadsTheNewestGenerationsRows(t *testing.T) {
	ctx := context.Background()
	store := openStackStore(t, "generation-rows-warm")
	commit, dirty := seedRoutedStack(t, store)
	flat := openStackStore(t, "generation-rows-warm-flat")
	seedStackFlatCorpus(t, flat)
	materializer := newTestMaterializer(store)
	if _, err := materializer.WarmRoute(ctx, commit, dirty); err != nil {
		t.Fatalf("WarmRoute: %v", err)
	}
	view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout: %v", err)
	}
	defer view.Close()
	var top *GenerationLayer
	for _, source := range view.GenerationSources() {
		if source.Generation == dirty {
			top, _ = source.Layer.(*GenerationLayer)
		}
	}
	if top == nil || top.preloadedRows() == nil {
		t.Fatal("the route's newest generation was not preloaded before the first view")
	}
	assertReadersAgree(t, view.Reader, flat)
}
