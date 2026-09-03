package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openImportProjectionTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "imports.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestImportAdjacencyProjectionUsesCoveringFileIndexes(t *testing.T) {
	store := openImportProjectionTestStore(t)
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+importAdjacencyProjectionSQL,
		`["pkg/caller.go"]`, string(graph.EdgeImports), baseViewGeneration,
		string(graph.EdgeImports), baseViewGeneration)
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
	got := plan.String()
	for _, required := range []string{
		"edges_by_file", "nodes_by_file", "edges_by_from",
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("query plan missing %s:\n%s", required, got)
		}
	}
	if strings.Contains(got, "SCAN e") || strings.Contains(got, "SCAN source") {
		t.Fatalf("query plan contains an unbounded table scan:\n%s", got)
	}
}

func TestImportAdjacencyProjectionSkipsUnrelatedOutgoingRows(t *testing.T) {
	store := openImportProjectionTestStore(t)
	const callerPath = "pkg/caller.go"
	const callerID = "pkg/caller.go::Caller"
	const targetPath = "dep/target.go"
	nodes := []*graph.Node{
		{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: callerPath},
		{ID: targetPath, Kind: graph.KindFile, Name: "target.go", FilePath: targetPath},
	}
	edges := []*graph.Edge{{From: callerID, To: targetPath, Kind: graph.EdgeImports, FilePath: callerPath}}
	for i := 0; i < 4096; i++ {
		edges = append(edges, &graph.Edge{
			From: callerID, To: fmt.Sprintf("unresolved::Call%04d", i),
			Kind: graph.EdgeCalls, FilePath: callerPath, Line: i + 1,
			Meta: map[string]any{"payload": strings.Repeat("x", 128)},
		})
	}
	store.AddBatch(nodes, edges)

	got, complete := store.ProjectImportAdjacency([]string{callerPath, callerPath})
	if !complete {
		t.Fatal("valid projection reported incomplete")
	}
	if targets := got[callerPath]; len(targets) != 1 || targets[0] != targetPath {
		t.Fatalf("projected targets = %v, want [%s]", targets, targetPath)
	}
}

func TestImportAdjacencyProjectionAcceptsMixedSeparatorPaths(t *testing.T) {
	// Windows stores index paths with the repo prefix joined by '/' and the
	// rest OS-native ("repo/dir\file.cs"). filepath.Clean normalizes the
	// separators, so a separator-only difference is not a canonicality
	// violation — treating it as one silently disabled the projection (and
	// every consumer above it) on Windows.
	store := openImportProjectionTestStore(t)
	const callerPath = `pkg/sub\caller.go`
	const callerID = callerPath + "::Caller"
	const targetPath = "dep/target.go"
	store.AddBatch([]*graph.Node{
		{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: callerPath},
		{ID: targetPath, Kind: graph.KindFile, Name: "target.go", FilePath: targetPath},
	}, []*graph.Edge{{
		From: callerID, To: targetPath, Kind: graph.EdgeImports, FilePath: callerPath,
	}})
	got, complete := store.ProjectImportAdjacency([]string{callerPath})
	if !complete {
		t.Fatal("mixed-separator canonical path reported incomplete")
	}
	if targets := got[callerPath]; len(targets) != 1 || targets[0] != targetPath {
		t.Fatalf("projected targets = %v, want [%s]", targets, targetPath)
	}
}

func TestImportAdjacencyProjectionRejectsMalformedProvenance(t *testing.T) {
	t.Run("noncanonical request", func(t *testing.T) {
		store := openImportProjectionTestStore(t)
		if got, complete := store.ProjectImportAdjacency([]string{"pkg/../pkg/caller.go"}); complete || got != nil {
			t.Fatalf("projection = %v, complete=%v; want nil/false", got, complete)
		}
	})

	for _, tc := range []struct {
		name     string
		edgePath string
	}{
		{name: "blank edge path", edgePath: ""},
		{name: "mismatched edge path", edgePath: "other/caller.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openImportProjectionTestStore(t)
			const callerPath = "pkg/caller.go"
			const callerID = "pkg/caller.go::Caller"
			store.AddBatch([]*graph.Node{
				{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: callerPath},
				{ID: "dep/target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "dep/target.go"},
			}, []*graph.Edge{{
				From: callerID, To: "dep/target.go", Kind: graph.EdgeImports, FilePath: tc.edgePath,
			}})
			if got, complete := store.ProjectImportAdjacency([]string{callerPath}); complete || got != nil {
				t.Fatalf("projection = %v, complete=%v; want nil/false", got, complete)
			}
		})
	}
}
