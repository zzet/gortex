package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func exactCount(t *testing.T, s *Store, table string, gen int64) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE view_gen = ?`, gen).Scan(&n))
	return n
}

func requireCountersExact(t *testing.T, s *Store, gens ...int64) {
	t.Helper()
	for _, g := range gens {
		h := s.AtGeneration(g)
		require.Equal(t, exactCount(t, s, "nodes", g), h.NodeCount(), "nodes at generation %d", g)
		require.Equal(t, exactCount(t, s, "edges", g), h.EdgeCount(), "edges at generation %d", g)
	}
	check, err := s.CheckRowCounters(context.Background(), false)
	require.NoError(t, err)
	require.True(t, check.Ready)
	require.Empty(t, check.Drift)
}

func rowCounterFixture(file string, n int) ([]*graph.Node, []*graph.Edge) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s::F%d", file, i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("F%d", i), FilePath: file, RepoPrefix: "repo"})
		edges = append(edges, &graph.Edge{From: id, To: fmt.Sprintf("%s::F%d", file, (i+1)%n), Kind: graph.EdgeCalls, FilePath: file, Line: i})
	}
	return nodes, edges
}

// The counters follow every kind of write: the batch writer's insert, its
// upsert and INSERT OR IGNORE no-ops, raw deletes, a row moved between
// generations, and a generation's retirement sweep — and NodeCount/EdgeCount
// read them.
func TestRowCountersFollowEveryWritePath(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	nodes, edges := rowCounterFixture("repo/a.go", 40)
	store.AddBatch(nodes, edges)
	gn, ge := rowCounterFixture("repo/g.go", 25)
	handle.AddBatch(gn, ge)
	require.NoError(t, store.EnsureRowCounters(ctx))
	require.True(t, store.RowCountersReady())
	requireCountersExact(t, store, 0, generationID)

	more, moreEdges := rowCounterFixture("repo/b.go", 30)
	store.AddBatch(more, moreEdges)
	store.AddBatch(nodes, edges) // upsert / ignore: no new rows
	requireCountersExact(t, store, 0, generationID)

	store.writeMu.Lock()
	_, err := store.writerDB.Exec(`DELETE FROM edges WHERE view_gen = 0 AND file_path = 'repo/b.go' AND line < 10`)
	require.NoError(t, err)
	_, err = store.writerDB.Exec(`DELETE FROM nodes WHERE view_gen = 0 AND file_path = 'repo/b.go' AND name IN ('F1','F2','F3')`)
	require.NoError(t, err)
	_, err = store.writerDB.Exec(`UPDATE nodes SET view_gen = ? WHERE view_gen = 0 AND file_path = 'repo/b.go' AND name = 'F4'`, generationID)
	require.NoError(t, err)
	store.writeMu.Unlock()
	requireCountersExact(t, store, 0, generationID)

	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 7000))
	require.NoError(t, store.RetirePayloadGeneration(ctx, generationID, nil))
	requireCountersExact(t, store, 0, generationID)
	require.Zero(t, store.AtGeneration(generationID).NodeCount())
}

// Seeding pins its snapshot before it releases the writer: a write committed
// right after the release is counted once (by the trigger), not twice.
func TestRowCountersSeedCountsAWriteAfterTheReleaseOnce(t *testing.T) {
	ctx := context.Background()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "seed.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	nodes, edges := rowCounterFixture("repo/a.go", 50)
	s.AddBatch(nodes, edges)
	late, lateEdges := rowCounterFixture("repo/late.go", 7)
	rowCountersAfterPinHook = func() { s.AddBatch(late, lateEdges) }
	t.Cleanup(func() { rowCountersAfterPinHook = nil })
	require.NoError(t, s.EnsureRowCounters(ctx))
	rowCountersAfterPinHook = nil
	require.Equal(t, 57, exactCount(t, s, "nodes", 0))
	requireCountersExact(t, s, 0)
}

// A drifted counter is reported, repaired by the snapshot's difference, and
// NodeCount reads the counter (so a drift would be visible there).
func TestRowCountersDriftIsReportedAndRepaired(t *testing.T) {
	ctx := context.Background()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "drift.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	nodes, edges := rowCounterFixture("repo/a.go", 20)
	s.AddBatch(nodes, edges)
	require.NoError(t, s.EnsureRowCounters(ctx))
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`UPDATE generation_row_counts SET nodes = nodes + 7 WHERE view_gen = 0`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	require.Equal(t, 27, s.NodeCount(), "NodeCount reads the counter")
	check, err := s.CheckRowCounters(ctx, false)
	require.NoError(t, err)
	require.Len(t, check.Drift, 1)
	require.Equal(t, RowCounterDrift{GenerationID: 0, CounterNodes: 27, ExactNodes: 20, CounterEdges: 20, ExactEdges: 20}, check.Drift[0])
	require.False(t, check.Repaired)
	check, err = s.CheckRowCounters(ctx, true)
	require.NoError(t, err)
	require.True(t, check.Repaired)
	requireCountersExact(t, s, 0)
}

// Triggers lost to a table rebuild are noticed at the next install, which
// re-seeds; with the kill switch the exact counts stay in use.
func TestRowCountersReinstallAfterLostTriggersAndKillSwitch(t *testing.T) {
	ctx := context.Background()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "lost.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	nodes, edges := rowCounterFixture("repo/a.go", 12)
	s.AddBatch(nodes, edges)
	require.NoError(t, s.EnsureRowCounters(ctx))
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`DROP TRIGGER generation_row_counts_edges_insert`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	installed, err := s.rowCountersInstalled(ctx)
	require.NoError(t, err)
	require.False(t, installed)
	more, moreEdges := rowCounterFixture("repo/b.go", 5)
	s.AddBatch(more, moreEdges) // uncounted edges: the trigger is gone
	require.NoError(t, s.EnsureRowCounters(ctx))
	requireCountersExact(t, s, 0)

	t.Setenv("GORTEX_SQLITE_ROW_COUNTERS", "0")
	s2, err := openPristine(t, filepath.Join(t.TempDir(), "off.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s2.Close() }()
	s2.AddBatch(nodes, edges)
	require.NoError(t, s2.EnsureRowCounters(ctx))
	require.False(t, s2.RowCountersReady())
	require.Equal(t, 12, s2.NodeCount())
}
