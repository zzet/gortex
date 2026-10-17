package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRepoPrefixesUsePayloadFreeCoveringIndex(t *testing.T) {
	s := openCatalogStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	require.NoError(t, s.AddBatchChecked([]*graph.Node{
		{ID: "same", Kind: graph.KindFunction, RepoPrefix: "repo", Meta: map[string]any{"evidence": strings.Repeat("x", 1<<20)}},
		{ID: "duplicate", Kind: graph.KindType, RepoPrefix: "repo"},
		{ID: "unicode", Kind: graph.KindFunction, RepoPrefix: "é/日本"},
		{ID: "solo", Kind: graph.KindFunction},
	}, nil))
	require.NoError(t, s.AtGeneration(7).AddBatchChecked([]*graph.Node{
		{ID: "same", Kind: graph.KindFunction, RepoPrefix: "other"},
		{ID: "also", Kind: graph.KindType, RepoPrefix: "other"},
		{ID: "positive-empty", Kind: graph.KindFunction},
	}, nil))
	var mu sync.Mutex
	var queries []string
	readStatementObserver = func(q string) {
		if strings.HasPrefix(strings.TrimSpace(q), "SELECT DISTINCT repo_prefix FROM nodes ") {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
		}
	}
	t.Cleanup(func() { readStatementObserver = nil })
	check := func() {
		require.ElementsMatch(t, []string{"repo", "é/日本"}, s.RepoPrefixes())
		require.Equal(t, []string{"other"}, s.AtGeneration(7).RepoPrefixes())
		require.Empty(t, s.AtGeneration(8).RepoPrefixes())
	}
	for _, state := range keyListStatsStates(func(t *testing.T, s *Store) {
		_, err := s.writerDB.Exec("ANALYZE")
		require.NoError(t, err)
	}) {
		t.Run(state.name, func(t *testing.T) { state.apply(t, s); check() })
	}
	require.NoError(t, s.AtGeneration(7).AddBatchChecked([]*graph.Node{{ID: "new", Kind: graph.KindFunction, RepoPrefix: "later"}}, nil))
	require.ElementsMatch(t, []string{"other", "later"}, s.AtGeneration(7).RepoPrefixes())
	require.ElementsMatch(t, []string{"repo", "é/日本"}, s.RepoPrefixes())
	mu.Lock()
	used := append([]string(nil), queries...)
	mu.Unlock()
	require.NotEmpty(t, used, "inspect the statement used by the real Reader API")
	for _, q := range used {
		require.Contains(t, planOfUnbound(t, s, q), "USING COVERING INDEX nodes_by_repo_kind", "prefix reads must not fetch payload B-trees")
	}
	require.Equal(t, strings.Repeat("x", 1<<20), s.GetNode("same").Meta["evidence"], "projection leaves the node payload unchanged")
}

func TestRepoPrefixesCoveringIndexRemainsLiveDuringColdBulk(t *testing.T) {
	s, err := openPristine(t, filepath.Join(t.TempDir(), "prefixes.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	require.True(t, s.BeginCoordinatedBulkLoad())
	var live, dropped int
	require.NoError(t, s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='nodes_by_repo_kind'").Scan(&live))
	require.Equal(t, 1, live)
	require.NoError(t, s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='nodes_by_repo'").Scan(&dropped))
	require.Zero(t, dropped, "the mandatory index must not be one of the deferred indexes")
	require.Empty(t, s.RepoPrefixes())
	nodes := []*graph.Node{}
	for i, prefix := range []string{"repo", "repo", "é/日本", ""} {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprint(i), Kind: graph.KindFunction, RepoPrefix: prefix})
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	require.ElementsMatch(t, []string{"repo", "é/日本"}, s.RepoPrefixes())
	require.NoError(t, s.EndCoordinatedBulkLoad())
	require.ElementsMatch(t, []string{"repo", "é/日本"}, s.RepoPrefixes())
}
