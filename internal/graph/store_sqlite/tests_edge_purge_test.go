package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestOpenPurgesLegacyUnresolvedTestsEdges is the upgrade proof for the
// unresolved-tests-edge purge: stores written before the emission guard
// hold derived EdgeTests rows cloned from unresolved calls (naked stubs
// nothing may ever bind). New emission never re-creates them, and warm
// startup may skip file-scoped reconciliation entirely, so an explicit
// versioned migration removes them. Every other row — resolved tests
// projections and ordinary unresolved calls — must survive untouched.
func TestOpenPurgesLegacyUnresolvedTestsEdges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	s, err := Open(path)
	require.NoError(t, err)
	s.AddNode(&graph.Node{ID: "r/a_test.go::TestFoo", Kind: graph.KindFunction, Name: "TestFoo", FilePath: "r/a_test.go", RepoPrefix: "r"})
	s.AddNode(&graph.Node{ID: "r/b.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "r/b.go", RepoPrefix: "r"})
	s.AddBatch(nil, []*graph.Edge{
		// A healthy resolved projection: must survive.
		{From: "r/a_test.go::TestFoo", To: "r/b.go::Foo", Kind: graph.EdgeTests, FilePath: "r/a_test.go", Line: 5},
		// Legacy unresolved clones in both stub spellings: must be purged.
		{From: "r/a_test.go::TestFoo", To: graph.UnresolvedMarker + "Gone", Kind: graph.EdgeTests, FilePath: "r/a_test.go", Line: 6},
		{From: "r/a_test.go::TestFoo", To: "r::" + graph.UnresolvedMarker + "*.Gone", Kind: graph.EdgeTests, FilePath: "r/a_test.go", Line: 7},
		// An ordinary pending call: NOT a tests edge, must survive.
		{From: "r/a_test.go::TestFoo", To: graph.UnresolvedMarker + "Gone", Kind: graph.EdgeCalls, FilePath: "r/a_test.go", Line: 6},
	})
	require.NoError(t, s.Close())

	// Simulate a store written before the purge shipped.
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 13`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	var kinds []string
	for _, e := range s2.GetOutEdges("r/a_test.go::TestFoo") {
		if e == nil {
			continue
		}
		if e.Kind == graph.EdgeTests {
			require.False(t, graph.IsUnresolvedTarget(e.To),
				"legacy unresolved tests edge survived the upgrade: %+v", e)
		}
		kinds = append(kinds, string(e.Kind)+"->"+e.To)
	}
	require.Contains(t, kinds, "tests->r/b.go::Foo", "the resolved projection must survive")
	require.Contains(t, kinds, "calls->"+graph.UnresolvedMarker+"Gone", "the pending call must survive")
	require.Len(t, kinds, 2, "exactly the two healthy edges remain")
}
