package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openBoundedAdjacencyTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteBoundedAdjacencyFiltersBeforeLimitAndReenters(t *testing.T) {
	store := openBoundedAdjacencyTestStore(t)
	const source = "repo/source.go::source"
	edges := make([]*graph.Edge, 0, 304)
	for index := 0; index < 300; index++ {
		edges = append(edges, &graph.Edge{
			From: source, To: fmt.Sprintf("noise-%03d", index), Kind: graph.EdgeReferences,
			FilePath: "repo/source.go", Line: 1_000 + index,
			Meta: map[string]any{"payload": strings.Repeat("x", 1024)},
		})
	}
	first := &graph.Edge{From: source, To: "target", Kind: graph.EdgeCalls, FilePath: "repo/source.go", Line: 10}
	second := &graph.Edge{From: source, To: "other", Kind: graph.EdgeCalls, FilePath: "generated.go", Line: 20}
	third := &graph.Edge{From: "repo/other.go::caller", To: "target", Kind: graph.EdgeCalls, FilePath: "repo/other.go", Line: 30}
	edges = append(edges, first, second, third)
	store.AddBatch(nil, edges)

	sources := []string{source, source, ""}
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeCalls, ""}
	sourcesBefore := append([]string(nil), sources...)
	kindsBefore := append([]graph.EdgeKind(nil), kinds...)
	outgoing, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), sources, kinds, 2)
	if err != nil {
		t.Fatalf("outgoing projection: %v", err)
	}
	if !reflect.DeepEqual(sources, sourcesBefore) || !reflect.DeepEqual(kinds, kindsBefore) {
		t.Fatalf("caller inputs mutated: sources=%#v kinds=%#v", sources, kinds)
	}
	wantOutgoing := []graph.EdgeIdentity{graph.EdgeIdentityFor(first), graph.EdgeIdentityFor(second)}
	sort.Slice(wantOutgoing, func(i, j int) bool { return wantOutgoing[i].To < wantOutgoing[j].To })
	if !reflect.DeepEqual(outgoing.ByEndpoint[source], wantOutgoing) || outgoing.Truncated[source] {
		t.Fatalf("outgoing = %#v, want %#v complete", outgoing, wantOutgoing)
	}

	incoming, err := store.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{"target"}, []graph.EdgeKind{graph.EdgeCalls}, 2)
	if err != nil {
		t.Fatalf("incoming projection: %v", err)
	}
	wantIncoming := []graph.EdgeIdentity{graph.EdgeIdentityFor(first), graph.EdgeIdentityFor(third)}
	sort.Slice(wantIncoming, func(i, j int) bool { return wantIncoming[i].From < wantIncoming[j].From })
	if !reflect.DeepEqual(incoming.ByEndpoint["target"], wantIncoming) || incoming.Truncated["target"] {
		t.Fatalf("incoming = %#v, want %#v complete", incoming, wantIncoming)
	}

	site := graph.EdgeSourceSite{From: source, Line: 10}
	sites := []graph.EdgeSourceSite{site, site}
	sitesBefore := append([]graph.EdgeSourceSite(nil), sites...)
	siteProjection, err := store.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), sites, []graph.EdgeKind{graph.EdgeCalls}, 1)
	if err != nil {
		t.Fatalf("site projection: %v", err)
	}
	if !reflect.DeepEqual(sites, sitesBefore) {
		t.Fatalf("caller sites mutated: %#v", sites)
	}
	if got := siteProjection.BySite[site]; !reflect.DeepEqual(got, []graph.EdgeIdentity{graph.EdgeIdentityFor(first)}) || siteProjection.Truncated[site] {
		t.Fatalf("site = %#v, want exact source+line identity", siteProjection)
	}

	// Every cursor and the read-only transaction must be closed before return.
	newEdge := &graph.Edge{From: "new-source", To: "new-target", Kind: graph.EdgeCalls}
	store.AddEdge(newEdge)
	reentered, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{newEdge.From}, []graph.EdgeKind{graph.EdgeCalls}, 1)
	if err != nil || len(reentered.ByEndpoint[newEdge.From]) != 1 {
		t.Fatalf("re-entered projection = %#v, %v", reentered, err)
	}
}

func TestSQLiteBoundedAdjacencyHardEnvelopesAndAggregateCap(t *testing.T) {
	store := openBoundedAdjacencyTestStore(t)
	tooManyKeys := make([]string, graph.MaxBoundedAdjacencyKeys+1)
	tooManyKinds := make([]graph.EdgeKind, graph.MaxBoundedAdjacencyKinds+1)
	for index := range tooManyKeys {
		tooManyKeys[index] = "duplicate"
	}
	for index := range tooManyKinds {
		tooManyKinds[index] = graph.EdgeCalls
	}
	for _, run := range []func() error{
		func() error {
			_, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), tooManyKeys, []graph.EdgeKind{graph.EdgeCalls}, 1)
			return err
		},
		func() error {
			_, err := store.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{"target"}, tooManyKinds, 1)
			return err
		},
		func() error {
			_, err := store.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), nil, nil, 0)
			return err
		},
		func() error {
			_, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), nil, nil, math.MaxInt)
			return err
		},
	} {
		var limitErr *graph.BoundedLocalizationLimitError
		if err := run(); !errors.As(err, &limitErr) {
			t.Fatalf("error = %v, want typed bounded limit", err)
		}
	}

	edges := make([]*graph.Edge, 0, 65*graph.MaxBoundedAdjacencyRowsPerKey)
	sources := make([]string, 0, 65)
	for sourceIndex := 0; sourceIndex < 65; sourceIndex++ {
		source := fmt.Sprintf("source-%02d", sourceIndex)
		sources = append(sources, source)
		for edgeIndex := 0; edgeIndex < graph.MaxBoundedAdjacencyRowsPerKey; edgeIndex++ {
			edges = append(edges, &graph.Edge{
				From: source, To: fmt.Sprintf("target-%02d-%03d", sourceIndex, edgeIndex),
				Kind: graph.EdgeCalls, FilePath: source + ".go", Line: edgeIndex,
			})
		}
	}
	store.AddBatch(nil, edges)
	exact, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), sources[:64], []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || len(exact.ByEndpoint) != 64 {
		t.Fatalf("exact aggregate cap = %d endpoints, %v", len(exact.ByEndpoint), err)
	}
	over, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), sources, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	var limitErr *graph.BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "SQLite adjacency matching identities" || len(over.ByEndpoint) != 0 || len(over.Truncated) != 0 {
		t.Fatalf("aggregate overflow = %#v, %v", over, err)
	}
}

func TestSQLiteBoundedAdjacencyPerKeySentinels(t *testing.T) {
	store := openBoundedAdjacencyTestStore(t)
	edges := make([]*graph.Edge, 0, 4*graph.MaxBoundedAdjacencyRowsPerKey+2)
	for index := 0; index < graph.MaxBoundedAdjacencyRowsPerKey; index++ {
		edges = append(edges, &graph.Edge{From: "exact-source", To: fmt.Sprintf("exact-%03d", index), Kind: graph.EdgeCalls, Line: 10})
	}
	for index := 0; index <= graph.MaxBoundedAdjacencyRowsPerKey; index++ {
		edges = append(edges, &graph.Edge{From: "overflow-source", To: fmt.Sprintf("overflow-%03d", index), Kind: graph.EdgeCalls, Line: 20})
	}
	for index := 0; index < graph.MaxBoundedAdjacencyRowsPerKey; index++ {
		edges = append(edges, &graph.Edge{From: fmt.Sprintf("exact-caller-%03d", index), To: "exact-target", Kind: graph.EdgeCalls})
	}
	for index := 0; index <= graph.MaxBoundedAdjacencyRowsPerKey; index++ {
		edges = append(edges, &graph.Edge{From: fmt.Sprintf("overflow-caller-%03d", index), To: "overflow-target", Kind: graph.EdgeCalls})
	}
	store.AddBatch(nil, edges)

	projection, err := store.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{"exact-source"}, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || len(projection.ByEndpoint["exact-source"]) != graph.MaxBoundedAdjacencyRowsPerKey || projection.Truncated["exact-source"] {
		t.Fatalf("exact endpoint boundary = %#v, %v", projection, err)
	}
	projection, err = store.FindOutgoingEdgeIdentitiesBounded(context.Background(), []string{"overflow-source"}, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || !projection.Truncated["overflow-source"] || len(projection.ByEndpoint["overflow-source"]) != 0 {
		t.Fatalf("endpoint sentinel leaked rows = %#v, %v", projection, err)
	}

	exactSite := graph.EdgeSourceSite{From: "exact-source", Line: 10}
	siteProjection, err := store.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), []graph.EdgeSourceSite{exactSite}, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || len(siteProjection.BySite[exactSite]) != graph.MaxBoundedAdjacencyRowsPerKey || siteProjection.Truncated[exactSite] {
		t.Fatalf("exact site boundary = %#v, %v", siteProjection, err)
	}
	overflowSite := graph.EdgeSourceSite{From: "overflow-source", Line: 20}
	siteProjection, err = store.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), []graph.EdgeSourceSite{overflowSite}, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || !siteProjection.Truncated[overflowSite] || len(siteProjection.BySite[overflowSite]) != 0 {
		t.Fatalf("site sentinel leaked rows = %#v, %v", siteProjection, err)
	}

	projection, err = store.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{"exact-target"}, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || len(projection.ByEndpoint["exact-target"]) != graph.MaxBoundedAdjacencyRowsPerKey || projection.Truncated["exact-target"] {
		t.Fatalf("exact incoming boundary = %#v, %v", projection, err)
	}
	projection, err = store.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{"overflow-target"}, []graph.EdgeKind{graph.EdgeCalls}, graph.MaxBoundedAdjacencyRowsPerKey)
	if err != nil || !projection.Truncated["overflow-target"] || len(projection.ByEndpoint["overflow-target"]) != 0 {
		t.Fatalf("incoming sentinel leaked rows = %#v, %v", projection, err)
	}
}

type cancelSQLiteAdjacencyAfterChecksContext struct {
	context.Context
	remaining int
}

func (ctx *cancelSQLiteAdjacencyAfterChecksContext) Err() error {
	if ctx.remaining == 0 {
		return context.Canceled
	}
	ctx.remaining--
	return nil
}

func TestSQLiteBoundedAdjacencyMidScanCancellationReturnsNoPartial(t *testing.T) {
	store := openBoundedAdjacencyTestStore(t)
	const source = "cancel-source"
	edges := make([]*graph.Edge, 0, graph.MaxBoundedAdjacencyRowsPerKey)
	for index := 0; index < graph.MaxBoundedAdjacencyRowsPerKey; index++ {
		edges = append(edges, &graph.Edge{From: source, To: fmt.Sprintf("target-%03d", index), Kind: graph.EdgeCalls, Line: index})
	}
	store.AddBatch(nil, edges)
	rows, err := store.db.Query(boundedOutgoingAdjacencySQL(1), source, string(graph.EdgeCalls), baseViewGeneration, graph.MaxBoundedAdjacencyRowsPerKey+1)
	if err != nil {
		t.Fatalf("open rows: %v", err)
	}
	total := 0
	ctx := &cancelSQLiteAdjacencyAfterChecksContext{Context: context.Background(), remaining: 1}
	identities, truncated, err := scanSQLiteBoundedAdjacencyRows(ctx, rows, graph.MaxBoundedAdjacencyRowsPerKey, &total)
	if !errors.Is(err, context.Canceled) || len(identities) != 0 || truncated {
		t.Fatalf("canceled scan leaked partial identities: len=%d truncated=%v err=%v", len(identities), truncated, err)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("rows: %v", err)
	}
	store.AddEdge(&graph.Edge{From: "after-cancel", To: "target", Kind: graph.EdgeCalls})
}

func TestSQLiteBoundedAdjacencyPlansUsePredicateIndexes(t *testing.T) {
	store := openBoundedAdjacencyTestStore(t)
	for _, test := range []struct {
		name        string
		query       string
		args        []any
		index       string
		constraints string
	}{
		{name: "outgoing", query: boundedOutgoingAdjacencySQL(1), args: []any{"source", string(graph.EdgeCalls), baseViewGeneration, 2}, index: "EDGES_BY_FROM", constraints: "(FROM_ID=? AND KIND=?)"},
		{name: "incoming", query: boundedIncomingAdjacencySQL(1), args: []any{"target", string(graph.EdgeCalls), baseViewGeneration, 2}, index: "EDGES_BY_TO", constraints: "(TO_ID=? AND KIND=?)"},
		{name: "site", query: boundedOutgoingSiteAdjacencySQL(1), args: []any{"source", 10, string(graph.EdgeCalls), baseViewGeneration, 2}, index: "EDGES_BY_FROM_LINE_KIND", constraints: "(FROM_ID=? AND LINE=? AND KIND=?)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := store.db.Query("EXPLAIN QUERY PLAN "+test.query, test.args...)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatalf("scan plan: %v", err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("plan rows: %v", err)
			}
			plan := strings.ToUpper(strings.Join(details, "\n"))
			if strings.Contains(plan, "SCAN EDGES") || strings.Contains(plan, "ORDER BY") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("bounded adjacency plan scans/sorts:\n%s", plan)
			}
			if !strings.Contains(plan, "USING INDEX "+test.index) || !strings.Contains(plan, test.constraints) {
				t.Fatalf("bounded adjacency plan missed %s %s:\n%s", test.index, test.constraints, plan)
			}
		})
	}
}

func TestSQLiteEdgesByFromLineIndexesCoexistAndOpenIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.writerDB.Exec(`DROP INDEX edges_by_from_line_kind`); err != nil {
		t.Fatalf("drop bounded-site index: %v", err)
	}
	if _, err := store.writerDB.Exec(`CREATE INDEX IF NOT EXISTS edges_by_from_line ON edges(from_id, line)`); err != nil {
		t.Fatalf("create historical index: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store without bounded-site index: %v", err)
	}

	for reopen := 0; reopen < 2; reopen++ {
		store, err = Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", reopen, err)
		}
		rows, err := store.db.Query(`PRAGMA index_info('edges_by_from_line')`)
		if err != nil {
			t.Fatalf("index info: %v", err)
		}
		var columns []string
		for rows.Next() {
			var sequence, columnID int
			var name string
			if err := rows.Scan(&sequence, &columnID, &name); err != nil {
				t.Fatalf("scan index info: %v", err)
			}
			columns = append(columns, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("index info rows: %v", err)
		}
		_ = rows.Close()
		if !reflect.DeepEqual(columns, []string{"from_id", "line"}) {
			t.Fatalf("reopen %d legacy columns = %#v", reopen, columns)
		}
		rows, err = store.db.Query(`PRAGMA index_info('edges_by_from_line_kind')`)
		if err != nil {
			t.Fatalf("bounded-site index info: %v", err)
		}
		columns = columns[:0]
		for rows.Next() {
			var sequence, columnID int
			var name string
			if err := rows.Scan(&sequence, &columnID, &name); err != nil {
				t.Fatalf("scan bounded-site index info: %v", err)
			}
			columns = append(columns, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("bounded-site index rows: %v", err)
		}
		_ = rows.Close()
		if !reflect.DeepEqual(columns, []string{"from_id", "line", "kind"}) {
			t.Fatalf("reopen %d bounded-site columns = %#v", reopen, columns)
		}
		var schemaBefore, schemaAfter int
		if err := store.writerDB.QueryRow(`PRAGMA schema_version`).Scan(&schemaBefore); err != nil {
			t.Fatalf("schema version before ensure: %v", err)
		}
		if _, err := store.writerDB.Exec(edgesByFromLineKindIndexDDL); err != nil {
			t.Fatalf("idempotent bounded-site index ensure: %v", err)
		}
		if err := store.writerDB.QueryRow(`PRAGMA schema_version`).Scan(&schemaAfter); err != nil {
			t.Fatalf("schema version after ensure: %v", err)
		}
		if schemaAfter != schemaBefore {
			t.Fatalf("correct index shape rebuilt: schema version %d -> %d", schemaBefore, schemaAfter)
		}
		var siblingCount int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'edges_by_from_line%'`).Scan(&siblingCount); err != nil {
			t.Fatalf("count site indexes: %v", err)
		}
		if siblingCount != 2 {
			t.Fatalf("reopen %d site index count = %d, want 2", reopen, siblingCount)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close reopen %d: %v", reopen, err)
		}
	}
}
