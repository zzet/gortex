package store_sqlite

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The narrowed read returns exactly the rows of the ordinary incoming read
// whose source node is in one of the repositories, in the same order, with
// or without the indexes a bulk load drops. A derived generation owns the
// same callers under other repositories, so each handle must judge a source
// by its own generation's node.
func TestRepoScopedInEdgesMatchFilteredInEdges(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "in-edges.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	const bare, member = graph.UnresolvedMarker + "Target", graph.UnresolvedMarker + "*.Target"
	var nodes []*graph.Node
	var edges []*graph.Edge
	for _, repo := range []string{"a", "b", "c", ""} {
		path := strings.TrimPrefix(repo+"/svc/worker.go", "/")
		caller := path + "::Run"
		nodes = append(nodes, &graph.Node{ID: caller, Name: "Run", Kind: graph.KindFunction, RepoPrefix: repo, FilePath: path})
		edges = append(edges,
			&graph.Edge{From: caller, To: bare, Kind: graph.EdgeCalls, FilePath: path, Line: 6},
			&graph.Edge{From: caller, To: bare, Kind: graph.EdgeReferences, FilePath: path, Line: 6},
			&graph.Edge{From: caller, To: member, Kind: graph.EdgeCalls, FilePath: path, Line: 8, Meta: map[string]any{"via": "retained"}},
		)
	}
	edges = append(edges,
		&graph.Edge{From: "ghost/worker.go::Run", To: bare, Kind: graph.EdgeCalls, FilePath: "ghost/worker.go", Line: 3},
		&graph.Edge{From: "b/svc/worker.go::Run", To: "a/svc/worker.go::Run", Kind: graph.EdgeCalls, FilePath: "b/svc/worker.go", Line: 9},
	)
	require.NoError(t, s.AddBatchChecked(nodes, edges))
	// Generation 1 keeps every call site but swaps the a and b callers'
	// repositories and has no node for the c caller.
	derived := s.AtGeneration(1)
	owner := map[string]string{"a": "b", "b": "a", "": ""}
	var moved []*graph.Node
	for _, n := range nodes {
		if repo, ok := owner[n.RepoPrefix]; ok {
			copied := *n
			copied.RepoPrefix = repo
			moved = append(moved, &copied)
		}
	}
	require.NoError(t, derived.AddBatchChecked(moved, edges))
	ids := []string{bare, member, "a/svc/worker.go::Run", "absent"}
	want := func(h *Store, repos []string) map[string][]*graph.Edge {
		out := make(map[string][]*graph.Edge)
		for to, rows := range h.GetInEdgesByNodeIDs(ids) {
			for _, edge := range rows {
				if source := h.GetNode(edge.From); source != nil && slices.Contains(repos, source.RepoPrefix) {
					out[to] = append(out[to], edge)
				}
			}
		}
		return out
	}
	check := func() {
		for _, h := range []*Store{s, derived} {
			for _, repos := range [][]string{{""}, {"", "a"}, {"", "a", "b"}, {"b", "", "a"}, {"c"}, {"missing"}} {
				got, handled, err := h.GetInEdgesByNodeIDsFromRepos(ids, repos)
				require.NoError(t, err)
				require.True(t, handled)
				require.Equal(t, want(h, repos), got, "generation=%d repos=%v", h.viewGen, repos)
			}
		}
	}
	check()
	require.Len(t, want(s, []string{"", "a"})[bare], 4, "the fixture parks two sites from each admitted repository on the bare placeholder")
	require.NotEqual(t, want(s, []string{"a"}), want(derived, []string{"a"}), "the generations disagree on which caller repository a owns")
	require.Empty(t, want(derived, []string{"c"}), "only generation 0 has a node for the c caller")

	// Each source probe, the named list and the empty prefix alike, is a
	// covering seek on the full repository key.
	for _, shape := range []struct {
		unowned bool
		probes  int
	}{{false, 1}, {true, 2}} {
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+repoScopedInEdgesSQL(len(ids), shape.unowned), append(toAnyArgs(ids), s.viewGen, `["a"]`)...)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		joined := strings.Join(plan, "\n")
		require.Contains(t, joined, "SEARCH e USING INDEX edges_by_to", joined)
		require.Equal(t, shape.probes, strings.Count(joined, "USING COVERING INDEX nodes_by_repo (repo_prefix=? AND view_gen=? AND id=?)"), joined)
		require.NotContains(t, joined, "SCAN e", joined)
		require.NotContains(t, joined, "SCAN n", joined)
	}

	_, err = s.writerDB.Exec(`DROP INDEX nodes_by_repo; DROP INDEX edges_by_to`)
	require.NoError(t, err)
	check()
}
