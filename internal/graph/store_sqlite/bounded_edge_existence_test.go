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

func TestFindExistingEdgeEndpointsProjectsDistinctPredicatesAndReenters(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		from  = "repo/source.go::source"
		owner = "repo/owner.go::Owner"
	)
	edges := make([]*graph.Edge, 0, 2_002)
	for line := 1; line <= 2_000; line++ {
		edges = append(edges, &graph.Edge{
			From: from, To: owner, Kind: graph.EdgeMemberOf,
			FilePath: "repo/source.go", Line: line,
			Meta: map[string]any{"payload": strings.Repeat("x", 1024)},
		})
	}
	edges = append(edges,
		&graph.Edge{From: from, To: owner, Kind: graph.EdgeCalls},
		&graph.Edge{From: from, To: "repo/other.go::Other", Kind: graph.EdgeMemberOf},
	)
	store.AddBatch(nil, edges)

	member := graph.TypedEdgeEndpoint{From: from, To: owner, Kind: graph.EdgeMemberOf}
	calls := graph.TypedEdgeEndpoint{From: from, To: owner, Kind: graph.EdgeCalls}
	missing := graph.TypedEdgeEndpoint{From: from, To: "missing", Kind: graph.EdgeMemberOf}
	input := []graph.TypedEdgeEndpoint{member, member, calls, missing, {}}
	before := append([]graph.TypedEdgeEndpoint(nil), input...)
	found, err := store.FindExistingEdgeEndpoints(context.Background(), input, 4)
	if err != nil {
		t.Fatalf("bounded edge existence: %v", err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("caller predicates mutated: got %#v want %#v", input, before)
	}
	if len(found) != 2 {
		t.Fatalf("found = %#v, want exactly member_of and calls", found)
	}
	if _, ok := found[member]; !ok {
		t.Fatalf("duplicate-heavy exact endpoint missing: %#v", found)
	}
	if _, ok := found[calls]; !ok {
		t.Fatalf("kind-specific endpoint missing: %#v", found)
	}

	// The read-only transaction and cursor must be closed before return.
	newKey := graph.TypedEdgeEndpoint{From: "new-source", To: "new-target", Kind: graph.EdgeCalls}
	store.AddEdge(&graph.Edge{From: newKey.From, To: newKey.To, Kind: newKey.Kind})
	found, err = store.FindExistingEdgeEndpoints(context.Background(), []graph.TypedEdgeEndpoint{newKey}, 1)
	if err != nil {
		t.Fatalf("re-entered projection: %v", err)
	}
	if _, ok := found[newKey]; !ok {
		t.Fatalf("re-entered projection missed newly committed edge: %#v", found)
	}
}

func TestFindExistingEdgeEndpointsEnforcesHardCapAndCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	keys := make([]graph.TypedEdgeEndpoint, 0, graph.MaxBoundedEdgeExistencePredicates+1)
	for index := 0; index <= graph.MaxBoundedEdgeExistencePredicates; index++ {
		keys = append(keys, graph.TypedEdgeEndpoint{
			From: "source", To: fmt.Sprintf("target-%03d", index), Kind: graph.EdgeCalls,
		})
	}
	if found, err := store.FindExistingEdgeEndpoints(
		context.Background(), keys[:graph.MaxBoundedEdgeExistencePredicates], graph.MaxBoundedEdgeExistencePredicates,
	); err != nil || len(found) != 0 {
		t.Fatalf("exact boundary = %#v, %v", found, err)
	}
	found, err := store.FindExistingEdgeEndpoints(context.Background(), keys, graph.MaxBoundedEdgeExistencePredicates)
	var limitErr *graph.BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || len(found) != 0 || limitErr.Limit != graph.MaxBoundedEdgeExistencePredicates {
		t.Fatalf("over boundary = %#v, %v", found, err)
	}
	for _, invalidLimit := range []int{0, graph.MaxBoundedEdgeExistencePredicates + 1} {
		found, err = store.FindExistingEdgeEndpoints(context.Background(), nil, invalidLimit)
		limitErr = nil
		if !errors.As(err, &limitErr) || len(found) != 0 || limitErr.Limit != graph.MaxBoundedEdgeExistencePredicates {
			t.Fatalf("empty input with invalid limit %d = %#v, %v", invalidLimit, found, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	found, err = store.FindExistingEdgeEndpoints(ctx, keys[:1], 1)
	if !errors.Is(err, context.Canceled) || len(found) != 0 {
		t.Fatalf("cancelled projection returned partial evidence: %#v, %v", found, err)
	}
}

func TestFindExistingEdgeEndpointsPlanUsesExactIndexWithoutSorter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddEdge(&graph.Edge{From: "source", To: "target", Kind: graph.EdgeMemberOf})

	rows, err := store.db.Query(
		"EXPLAIN QUERY PLAN "+boundedEdgeExistenceQuery(1),
		"source", "target", string(graph.EdgeMemberOf), baseViewGeneration,
	)
	if err != nil {
		t.Fatalf("explain edge existence: %v", err)
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
	if strings.Contains(plan, "SCAN EDGES") || strings.Contains(plan, "ORDER BY") {
		t.Fatalf("bounded exact endpoint query scans/sorts edges:\n%s", plan)
	}
	if !strings.Contains(plan, "USING COVERING INDEX SQLITE_AUTOINDEX_EDGES_1") ||
		!strings.Contains(plan, "(FROM_ID=? AND TO_ID=? AND KIND=?)") {
		t.Fatalf("bounded exact endpoint query did not use the exact covering edge key:\n%s", plan)
	}
}
