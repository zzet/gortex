package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestNodeKindMembershipSQLParityChunksAndGeneration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "membership.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	kinds := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey, graph.KindFunction, graph.NodeKind("future_kind")}
	var nodes []*graph.Node
	var ids []string
	for i := 0; i < lookupChunkSize+3; i++ {
		id := fmt.Sprintf("repo/f.go::N%d", i)
		ids = append(ids, id)
		nodes = append(nodes, &graph.Node{ID: id, Kind: kinds[i%len(kinds)], FilePath: "repo/f.go", RepoPrefix: "repo"})
	}
	s.AddBatch(nodes, nil)
	positive := s.AtGeneration(7)
	positive.AddNode(&graph.Node{ID: ids[0], Kind: graph.KindFunction})
	positive.AddNode(&graph.Node{ID: ids[3], Kind: graph.KindConfigKey})
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE view_gen = 0`, []byte{0xff})
	require.NoError(t, err)
	ids = append(ids, "", ids[0], "missing")
	owned := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey, graph.KindContract}
	before := append([]string(nil), ids...)
	structural, err := s.GetNodeKindsByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	want := map[string]struct{}{}
	for id, row := range structural {
		if row.Kind == graph.KindContract || row.Kind == graph.KindContractBridge || row.Kind == graph.KindConfigKey {
			want[id] = struct{}{}
		}
	}
	got, err := s.GetNodeIDsByKindsContext(t.Context(), ids, owned)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, before, ids)
	got, err = positive.GetNodeIDsByKindsContext(t.Context(), ids, owned)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{ids[3]: {}}, got)
	got, err = s.GetNodeIDsByKindsContext(t.Context(), ids, []graph.NodeKind{graph.NodeKind("future_kind")})
	require.NoError(t, err)
	require.Contains(t, got, ids[4])
	for _, input := range [][]string{nil, {"", "missing"}} {
		got, err = s.GetNodeIDsByKindsContext(t.Context(), input, owned)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Empty(t, got)
	}
	got, err = s.GetNodeIDsByKindsContext(t.Context(), ids, nil)
	require.NoError(t, err)
	require.Empty(t, got)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = s.GetNodeIDsByKindsContext(ctx, ids, owned)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	require.NoError(t, s.Close())
	got, err = s.GetNodeIDsByKindsContext(t.Context(), ids, owned)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestNodeIDsByKindsBoundedReadsGenerationMembershipThroughKindIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kind-ids.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	// Keep the global observer away from background reads.
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	kinds := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey, graph.KindFunction, graph.NodeKind("future_kind")}
	var nodes []*graph.Node
	var ids []string
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("repo/f.go::N%d", i)
		ids = append(ids, id)
		nodes = append(nodes, &graph.Node{ID: id, Kind: kinds[i%len(kinds)], FilePath: "repo/f.go", RepoPrefix: "repo"})
	}
	s.AddBatch(nodes, nil)
	positive := s.AtGeneration(7)
	positive.AddNode(&graph.Node{ID: ids[0], Kind: graph.KindFunction})
	positive.AddNode(&graph.Node{ID: ids[3], Kind: graph.KindConfigKey})
	owned := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey, graph.KindContract}

	var mu sync.Mutex
	var queries []string
	readStatementObserver = func(q string) {
		if strings.Contains(q, "SELECT id FROM nodes INDEXED BY") {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
		}
	}
	t.Cleanup(func() { readStatementObserver = nil })

	var baseCount int
	for _, handle := range []*Store{s, positive} {
		got, complete, err := handle.NodeIDsByKindsBoundedContext(t.Context(), owned, 1000)
		require.NoError(t, err)
		require.True(t, complete)
		want, err := handle.GetNodeIDsByKindsContext(t.Context(), ids, owned)
		require.NoError(t, err)
		set := make(map[string]struct{}, len(got))
		for _, id := range got {
			set[id] = struct{}{}
		}
		require.Equal(t, want, set, "the whole read answers every per-ID membership of its generation")
		require.Len(t, got, len(want), "each identity is read once")
		if handle == s {
			baseCount = len(got)
		}
	}
	require.Equal(t, 24, baseCount)
	mu.Lock()
	used := append([]string(nil), queries...)
	mu.Unlock()
	require.Len(t, used, 2)
	for _, q := range used {
		require.Contains(t, planOfUnbound(t, s, q), "USING COVERING INDEX nodes_by_kind", "the identity read must not visit node payloads")
	}

	got, complete, err := s.NodeIDsByKindsBoundedContext(t.Context(), owned, baseCount)
	require.NoError(t, err)
	require.True(t, complete)
	require.Len(t, got, baseCount)
	got, complete, err = s.NodeIDsByKindsBoundedContext(t.Context(), owned, baseCount-1)
	require.NoError(t, err)
	require.False(t, complete, "a set past the bound is refused, not truncated")
	require.Nil(t, got)
	got, complete, err = s.NodeIDsByKindsBoundedContext(t.Context(), nil, 10)
	require.NoError(t, err)
	require.True(t, complete)
	require.Empty(t, got)

	// A cold bulk load drops the kind index; the read declines instead of
	// scanning the generation or failing.
	_, err = s.writerDB.Exec(`DROP INDEX ` + nodesByKindIndexName)
	require.NoError(t, err)
	got, complete, err = s.NodeIDsByKindsBoundedContext(t.Context(), owned, 1000)
	require.NoError(t, err)
	require.False(t, complete)
	require.Nil(t, got)
	_, err = s.writerDB.Exec(`CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind, view_gen)`)
	require.NoError(t, err)
	got, complete, err = s.NodeIDsByKindsBoundedContext(t.Context(), owned, 1000)
	require.NoError(t, err)
	require.True(t, complete)
	require.Len(t, got, baseCount)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, complete, err = s.NodeIDsByKindsBoundedContext(ctx, owned, 1000)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, complete)
	require.Nil(t, got)
	require.NoError(t, s.Close())
	got, complete, err = s.NodeIDsByKindsBoundedContext(t.Context(), owned, 1000)
	require.Error(t, err)
	require.False(t, complete)
	require.Nil(t, got)
}

// A read whose statement found the kind index dropped declines even when the
// index is rebuilt before anything else is read: the rebuild window never turns
// into a read failure.
func TestNodeIDsByKindsBoundedDeclinesWhenTheIndexReturnsAfterAFailedPrepare(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kind-ids-rebuild.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	// Keep the global observer away from background reads.
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	s.AddBatch([]*graph.Node{
		{ID: "repo/f.go::C", Kind: graph.KindContract, FilePath: "repo/f.go", RepoPrefix: "repo"},
		{ID: "repo/f.go::K", Kind: graph.KindConfigKey, FilePath: "repo/f.go", RepoPrefix: "repo"},
		{ID: "repo/f.go::F", Kind: graph.KindFunction, FilePath: "repo/f.go", RepoPrefix: "repo"},
	}, nil)
	owned := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey}
	const rebuild = `CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind, view_gen)`
	_, err = s.writerDB.Exec(`DROP INDEX ` + nodesByKindIndexName)
	require.NoError(t, err)

	// The index comes back right after the pinned statement fails, before
	// any later statement of the read runs.
	var (
		mu         sync.Mutex
		pinned     bool
		rebuilt    bool
		rebuildErr error
	)
	readStatementObserver = func(q string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(q, "SELECT id FROM nodes INDEXED BY") {
			pinned = true
			return
		}
		if pinned && !rebuilt {
			rebuilt = true
			_, rebuildErr = s.writerDB.Exec(rebuild)
		}
	}
	t.Cleanup(func() { readStatementObserver = nil })

	got, complete, err := s.NodeIDsByKindsBoundedContext(t.Context(), owned, 1000)
	require.NoError(t, err, "the dropped index declines the read, whatever a later probe sees")
	require.False(t, complete)
	require.Nil(t, got)
	mu.Lock()
	readStatementObserver = nil
	require.True(t, pinned)
	require.False(t, rebuilt, "a prepare error naming the missing index declines without a later probe")
	require.NoError(t, rebuildErr)
	mu.Unlock()

	_, err = s.writerDB.Exec(rebuild)
	require.NoError(t, err)
	got, complete, err = s.NodeIDsByKindsBoundedContext(t.Context(), owned, 1000)
	require.NoError(t, err)
	require.True(t, complete)
	require.ElementsMatch(t, []string{"repo/f.go::C", "repo/f.go::K"}, got)
}
