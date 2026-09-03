package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFindIncomingSourcesBoundedCountsDistinctRelevantRowsAndReenters(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const targetID = "repo/target.go::target"
	edges := make([]*graph.Edge, 0, 32)
	for index := 0; index < 8; index++ {
		sourceID := fmt.Sprintf("repo/source-%02d.go::source", index)
		for line := 1; line <= 3; line++ {
			edges = append(edges, &graph.Edge{From: sourceID, To: targetID, Kind: graph.EdgeCalls, Line: line})
		}
		edges = append(edges, &graph.Edge{From: fmt.Sprintf("repo/noise-%02d.go::noise", index), To: targetID, Kind: graph.EdgeReferences})
	}
	store.AddBatch(nil, edges)

	page, err := store.FindIncomingSourcesBounded(context.Background(), []string{targetID}, graph.EdgeCalls, 8)
	if err != nil {
		t.Fatalf("bounded incoming projection: %v", err)
	}
	if page.Truncated[targetID] || len(page.Sources[targetID]) != 8 {
		t.Fatalf("projection = %#v, want eight distinct CALLS sources", page)
	}

	// The projection must close its cursor and read transaction before return;
	// this write and second read would otherwise expose store re-entry trouble.
	store.AddEdge(&graph.Edge{From: "repo/source-08.go::source", To: targetID, Kind: graph.EdgeCalls})
	page, err = store.FindIncomingSourcesBounded(context.Background(), []string{targetID, "missing"}, graph.EdgeCalls, 8)
	if err != nil {
		t.Fatalf("re-entered saturated projection: %v", err)
	}
	if !page.Truncated[targetID] || len(page.Sources[targetID]) != 0 || page.Truncated["missing"] {
		t.Fatalf("re-entered projection = %#v", page)
	}
}

func TestFindIncomingSourcesBoundedDuplicateHeavySentinel(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const targetID = "target"
	edges := make([]*graph.Edge, 0, 2_001)
	for line := 1; line <= 2_000; line++ {
		edges = append(edges, &graph.Edge{From: "one-source", To: targetID, Kind: graph.EdgeCalls, Line: line})
	}
	store.AddBatch(nil, edges)
	page, err := store.FindIncomingSourcesBounded(context.Background(), []string{targetID}, graph.EdgeCalls, 1)
	if err != nil || page.Truncated[targetID] || len(page.Sources[targetID]) != 1 {
		t.Fatalf("duplicate-heavy projection = %#v, %v; duplicates consumed sentinel", page, err)
	}
	store.AddEdge(&graph.Edge{From: "second-source", To: targetID, Kind: graph.EdgeCalls})
	page, err = store.FindIncomingSourcesBounded(context.Background(), []string{targetID}, graph.EdgeCalls, 1)
	if err != nil || !page.Truncated[targetID] || len(page.Sources[targetID]) != 0 {
		t.Fatalf("second distinct source did not saturate: %#v, %v", page, err)
	}
}

func TestFindIncomingSourcesBoundedRejectsImpossibleLimitAndCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddEdge(&graph.Edge{From: "source", To: "target", Kind: graph.EdgeCalls})

	maxInt := int(^uint(0) >> 1)
	page, err := store.FindIncomingSourcesBounded(context.Background(), []string{"target"}, graph.EdgeCalls, maxInt)
	var limitErr *graph.BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || len(page.Sources) != 0 {
		t.Fatalf("max-int projection = %#v, %v; want typed empty failure", page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err = store.FindIncomingSourcesBounded(ctx, []string{"target"}, graph.EdgeCalls, 8)
	if !errors.Is(err, context.Canceled) || len(page.Sources) != 0 {
		t.Fatalf("cancelled projection = %#v, %v", page, err)
	}

	layer := graph.NewOverlayLayer()
	layer.AddNode("repo/overlay.go", &graph.Node{ID: "overlay", Name: "overlay", Kind: graph.KindFunction, FilePath: "repo/overlay.go"})
	ids := []string{"overlay", "target"}
	if nodes, err := graph.NewOverlaidView(store, layer).GetNodesByIDsContext(ctx, ids); !errors.Is(err, context.Canceled) || len(nodes) != 0 {
		t.Fatalf("cancelled SQLite-backed overlay refetch = %#v, %v", nodes, err)
	}
}

type contextualSQLiteNodeSpy struct {
	*Store
	contextCalls int
	legacyCalls  int
}

func (spy *contextualSQLiteNodeSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	spy.legacyCalls++
	return spy.Store.GetNodesByIDs(ids)
}

func (spy *contextualSQLiteNodeSpy) GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error) {
	spy.contextCalls++
	return map[string]*graph.Node{"base": {ID: "base", Name: "base", Kind: graph.KindFunction}}, context.Canceled
}

func TestOverlaidViewContextualExactRefetchUsesSQLiteContextPathAndFailsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{ID: "base", Name: "base", Kind: graph.KindFunction})
	spy := &contextualSQLiteNodeSpy{Store: store}
	layer := graph.NewOverlayLayer()
	layer.AddNode("repo/overlay.go", &graph.Node{
		ID: "repo/overlay.go::overlay", Name: "overlay", Kind: graph.KindFunction, FilePath: "repo/overlay.go",
	})
	ids := []string{"repo/overlay.go::overlay", "base"}
	before := append([]string(nil), ids...)
	nodes, err := graph.NewOverlaidView(spy, layer).GetNodesByIDsContext(context.Background(), ids)
	if !errors.Is(err, context.Canceled) || len(nodes) != 0 {
		t.Fatalf("contextual SQLite error returned partial overlay nodes: %#v, %v", nodes, err)
	}
	if spy.contextCalls != 1 || spy.legacyCalls != 0 || !reflect.DeepEqual(ids, before) {
		t.Fatalf("dispatch/caller slice = contextual:%d legacy:%d ids:%v want:%v", spy.contextCalls, spy.legacyCalls, ids, before)
	}
}

func TestFindIncomingSourcesBoundedPlanAvoidsOrderBySorter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddEdge(&graph.Edge{From: "source", To: "target", Kind: graph.EdgeCalls})

	rows, err := store.db.Query(
		"EXPLAIN QUERY PLAN "+findIncomingSourcesBoundedSQL,
		"target", graph.EdgeCalls, baseViewGeneration, 17,
	)
	if err != nil {
		t.Fatalf("explain incoming projection: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query plan rows: %v", err)
	}
	plan := strings.ToUpper(strings.Join(details, "\n"))
	if strings.Contains(plan, "ORDER BY") {
		t.Fatalf("bounded incoming query materializes an ORDER BY sorter:\n%s", plan)
	}
	if !strings.Contains(plan, "EDGES_BY_TO") {
		t.Fatalf("bounded incoming query did not use target/kind index:\n%s", plan)
	}
}
