package store_sqlite

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openScopedProjectionTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "scoped.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func scopedProjectionPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

func TestScopedProjectionKeysetPagesRepositoryAndFileRows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "scoped-projection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const perRepo = scopedProjectionPage + 73
	nodes := make([]*graph.Node, 0, perRepo*2+1)
	edges := make([]*graph.Edge, 0, perRepo*2)
	nodes = append(nodes, &graph.Node{ID: "target", Kind: graph.KindFunction, Name: "target", RepoPrefix: "target"})
	for _, repo := range []string{"a", "b"} {
		for i := 0; i < perRepo; i++ {
			file := fmt.Sprintf("%s/file-%03d.go", repo, i)
			id := fmt.Sprintf("%s::fn-%03d", repo, i)
			nodes = append(nodes, &graph.Node{
				ID: id, Kind: graph.KindFunction, Name: "fn", FilePath: file,
				Language: "go", RepoPrefix: repo, Meta: map[string]any{"ordinal": i},
			})
			edges = append(edges, &graph.Edge{
				From: id, To: "target", Kind: graph.EdgeCalls, FilePath: file, Line: i + 1,
				Meta: map[string]any{"ordinal": i},
			})
		}
	}
	store.AddBatch(nodes, edges)

	seenNodes := 0
	for node := range store.NodesInScopeSeq([]string{"a"}, nil, graph.KindFunction) {
		if node.RepoPrefix != "a" || node.Meta == nil {
			t.Fatalf("unexpected full scoped node: %#v", node)
		}
		seenNodes++
	}
	if seenNodes != perRepo {
		t.Fatalf("repo node rows = %d, want %d", seenNodes, perRepo)
	}

	seenFileNodes := 0
	for node := range store.NodesInScopeSeq(
		[]string{"a"}, []string{"a/file-007.go"}, graph.KindFunction,
	) {
		seenFileNodes++
		if node.ID != "a::fn-007" {
			t.Fatalf("file-scoped node = %q, want a::fn-007", node.ID)
		}
	}
	if seenFileNodes != 1 {
		t.Fatalf("file node rows = %d, want 1", seenFileNodes)
	}

	seenLight := 0
	for node := range store.NodesLightInScopeSeq([]string{"a"}, nil) {
		if node.Meta != nil {
			t.Fatalf("light scope decoded metadata: %#v", node.Meta)
		}
		seenLight++
	}
	if seenLight != perRepo {
		t.Fatalf("light repo rows = %d, want %d", seenLight, perRepo)
	}

	seenEdges := 0
	for row := range store.EdgesInScopeSeq([]string{"a"}, nil, graph.EdgeCalls) {
		if row.Edge == nil || row.Source == nil || row.Source.ID != row.Edge.From {
			t.Fatalf("edge/source hydration mismatch: %#v", row)
		}
		if row.Source.RepoPrefix != "a" || row.Edge.Meta == nil {
			t.Fatalf("unexpected full scoped edge row: %#v", row)
		}
		seenEdges++
		if seenEdges == 1 {
			// The sequence freezes max(id) before page one. A synthesizer write
			// during iteration must not feed the same candidate scan.
			store.AddEdge(&graph.Edge{
				From: "a::fn-000", To: "target", Kind: graph.EdgeCalls,
				FilePath: "a/file-000.go", Line: 999,
			})
		}
	}
	if seenEdges != perRepo {
		t.Fatalf("repo edge rows = %d, want frozen %d", seenEdges, perRepo)
	}
}

func TestScopedProjectionClosesPageCursorBeforeYield(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "scoped-projection-write.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	store.AddBatch([]*graph.Node{
		{ID: "a::source", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a"},
		{ID: "target", Kind: graph.KindFunction, RepoPrefix: "target"},
		{ID: "new-target", Kind: graph.KindFunction, RepoPrefix: "target"},
	}, []*graph.Edge{{From: "a::source", To: "target", Kind: graph.EdgeCalls, FilePath: "a/main.go"}})

	for row := range store.EdgesInScopeSeq([]string{"a"}, []string{"a/main.go"}, graph.EdgeCalls) {
		if row.Edge == nil {
			t.Fatal("nil edge")
		}
		// This would deadlock with MaxOpenConns(1) if a row cursor remained
		// open across yield.
		store.AddEdge(&graph.Edge{
			From: "a::source", To: "new-target", Kind: graph.EdgeCalls,
			FilePath: "a/main.go", Line: 2,
		})
		break
	}
	if got := len(store.GetOutEdges("a::source")); got != 2 {
		t.Fatalf("post-yield write edges = %d, want 2", got)
	}
}

// The scoped projections must never plan a temp b-tree: with the former
// json_each kinds CTE, SQLite re-sorted every remaining row on each 256-row
// page, turning linear walks quadratic on multi-million-row stores.
func TestScopedProjectionQueriesStreamWithoutTempBTree(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "scoped-projection-plan.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := make([]*graph.Node, 0, 512)
	edges := make([]*graph.Edge, 0, 512)
	nodes = append(nodes, &graph.Node{ID: "target", Kind: graph.KindFunction, Name: "t", RepoPrefix: "b"})
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("a::fn-%03d", i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: "fn",
			FilePath: fmt.Sprintf("a/f-%03d.go", i), RepoPrefix: "a",
		})
		edges = append(edges, &graph.Edge{From: id, To: "target", Kind: graph.EdgeCalls, Line: i + 1})
	}
	store.AddBatch(nodes, edges)

	assertNoTempBTree := func(name, query string, args []any) {
		t.Helper()
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			if strings.Contains(detail, "TEMP B-TREE") {
				t.Fatalf("%s: plan uses a temp b-tree: %s", name, detail)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", name, err)
		}
	}

	edgeQuery, edgeArgs, ok := scopedEdgeProjectionQuery([]string{"a"}, nil, string(graph.EdgeCalls), baseViewGeneration)
	if !ok {
		t.Fatal("edge query not built")
	}
	assertNoTempBTree("edges", edgeQuery, append(edgeArgs, int64(0), int64(1)<<60, scopedProjectionPage))

	nodeQuery, nodeArgs, ok := scopedNodeProjectionQuery([]string{"a"}, nil, string(graph.KindFunction), lookupNodeCols, baseViewGeneration)
	if !ok {
		t.Fatal("node query not built")
	}
	assertNoTempBTree("nodes", nodeQuery, append(nodeArgs, "", scopedProjectionPage))

	lightQuery, lightArgs, ok := scopedNodeProjectionQuery([]string{"a"}, nil, "", lookupNodeSummaryCols, baseViewGeneration)
	if !ok {
		t.Fatal("light query not built")
	}
	assertNoTempBTree("nodes-light", lightQuery, append(lightArgs, "", scopedProjectionPage))
}

func TestScopedProjectionMultiKindStreamsEveryKind(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "scoped-projection-kinds.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const perKind = scopedProjectionPage + 9
	nodes := []*graph.Node{
		{ID: "child", Kind: graph.KindType, Name: "child", RepoPrefix: "a", FilePath: "a/t.go"},
		{ID: "parent", Kind: graph.KindType, Name: "parent", RepoPrefix: "a", FilePath: "a/t.go"},
	}
	edges := make([]*graph.Edge, 0, perKind*2)
	for i := 0; i < perKind; i++ {
		edges = append(edges,
			&graph.Edge{From: "child", To: "parent", Kind: graph.EdgeExtends, FilePath: "a/t.go", Line: i + 1},
			&graph.Edge{From: "child", To: "parent", Kind: graph.EdgeComposes, FilePath: "a/t.go", Line: i + 1},
		)
	}
	store.AddBatch(nodes, edges)

	perKindSeen := map[graph.EdgeKind]int{}
	for row := range store.EdgesInScopeSeq([]string{"a"}, nil, graph.EdgeExtends, graph.EdgeComposes, graph.EdgeExtends) {
		perKindSeen[row.Edge.Kind]++
		if row.Source == nil || row.Target == nil {
			t.Fatalf("endpoints not hydrated: %#v", row)
		}
	}
	if perKindSeen[graph.EdgeExtends] != perKind || perKindSeen[graph.EdgeComposes] != perKind {
		t.Fatalf("per-kind rows = %v, want %d each (duplicate kind must not double-yield)", perKindSeen, perKind)
	}

	empty := 0
	for range store.EdgesInScopeSeq([]string{"a"}, nil, graph.EdgeKind("")) {
		empty++
	}
	if empty != 0 {
		t.Fatalf("all-empty kinds yielded %d rows, want none", empty)
	}
}

func TestScopedEdgeProjectionPlansStayInsideRequestedFiles(t *testing.T) {
	store := openScopedProjectionTestStore(t)
	for _, tc := range []struct {
		name     string
		query    string
		args     []any
		required []string
	}{
		func() struct {
			name     string
			query    string
			args     []any
			required []string
		} {
			query, args, ok := scopedEdgeProjectionQuery(
				[]string{"repo"}, []string{"pkg/caller.go"}, string(graph.EdgeCalls), baseViewGeneration,
			)
			if !ok {
				t.Fatal("canonical file query was not built")
			}
			return struct {
				name     string
				query    string
				args     []any
				required []string
			}{"canonical", query, args, []string{"edges_by_file"}}
		}(),
		func() struct {
			name     string
			query    string
			args     []any
			required []string
		} {
			query, args, ok := scopedEdgeSourceProjectionQuery(
				[]string{"repo"}, []string{"pkg/caller.go"}, string(graph.EdgeCalls), baseViewGeneration,
			)
			if !ok {
				t.Fatal("source fallback query was not built")
			}
			return struct {
				name     string
				query    string
				args     []any
				required []string
			}{"source fallback", query, args, []string{"nodes_by_file", "edges_by_from"}}
		}(),
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]any(nil), tc.args...), int64(0), int64(math.MaxInt64), scopedProjectionPage)
			plan := scopedProjectionPlan(t, store, tc.query, args...)
			for _, required := range tc.required {
				if !strings.Contains(plan, required) {
					t.Fatalf("query plan missing %s:\n%s", required, plan)
				}
			}
			if strings.Contains(plan, "SCAN e") || strings.Contains(plan, "SCAN n") {
				t.Fatalf("query plan contains an unbounded table scan:\n%s", plan)
			}
		})
	}
}

func TestScopedEdgeProjectionFallsBackForMalformedFileProvenance(t *testing.T) {
	store := openScopedProjectionTestStore(t)
	const (
		repo       = "repo"
		callerPath = "pkg/caller.go"
		otherPath  = "pkg/other.go"
		callerID   = "repo/pkg/caller.go::Caller"
		otherID    = "repo/pkg/other.go::Other"
	)
	store.AddBatch([]*graph.Node{
		{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: callerPath, RepoPrefix: repo},
		{ID: otherID, Kind: graph.KindFunction, Name: "Other", FilePath: otherPath, RepoPrefix: repo},
	}, []*graph.Edge{
		{From: callerID, To: "unresolved::Canonical", Kind: graph.EdgeCalls, FilePath: callerPath, Line: 1},
		{From: callerID, To: "unresolved::Malformed", Kind: graph.EdgeReferences, FilePath: "", Line: 2},
	})

	noise := make([]*graph.Edge, 0, 1024)
	for i := 0; i < cap(noise); i++ {
		noise = append(noise, &graph.Edge{
			From: otherID, To: fmt.Sprintf("unresolved::Noise%04d", i),
			Kind: graph.EdgeCalls, FilePath: otherPath, Line: i + 1,
		})
	}
	store.AddBatch(nil, noise)

	var maxID int64
	if err := store.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM edges`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if store.scopedEdgeFileProvenanceCanonical(
		[]string{repo}, []string{callerPath},
		[]string{string(graph.EdgeCalls), string(graph.EdgeReferences)}, maxID,
	) {
		t.Fatal("malformed edge provenance was accepted as canonical")
	}

	got := make(map[string]bool)
	for row := range store.EdgesInScopeSeq(
		[]string{repo}, []string{callerPath}, graph.EdgeCalls, graph.EdgeReferences,
	) {
		if row.Edge == nil || row.Source == nil {
			t.Fatalf("incomplete scoped row: %#v", row)
		}
		got[row.Edge.To] = true
	}
	if len(got) != 2 || !got["unresolved::Canonical"] || !got["unresolved::Malformed"] {
		t.Fatalf("scoped targets = %v, want canonical and malformed", got)
	}
}
