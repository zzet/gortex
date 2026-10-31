package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type eligibilityCandidateSpy struct {
	*store_sqlite.Store
	rows *int
}

func (r *eligibilityCandidateSpy) BindReadContext(ctx context.Context) graph.Reader {
	return &eligibilityCandidateSpy{Store: r.WithReadContext(ctx), rows: r.rows}
}
func (r *eligibilityCandidateSpy) DeadCodeCandidatesInScopeFiltered(k []graph.NodeKind, in map[graph.NodeKind][]graph.EdgeKind, repos []string, scope graph.NodeScope, filter graph.OutlineEdgeFilter, exclude graph.DeadCodeCandidateExclusions) ([]*graph.Node, error) {
	nodes, err := r.Store.DeadCodeCandidatesInScopeFiltered(k, in, repos, scope, filter, exclude)
	*r.rows += len(nodes)
	return nodes, err
}
func TestDeadCodeKnownIneligibleGlobalsDoNotGrowCandidateHydration(t *testing.T) {
	s, store := outlineFixture(t)
	store.AddBatch([]*graph.Node{
		{ID: "own-private", Kind: graph.KindFunction, Name: "_unused_own", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "own-live", Kind: graph.KindFunction, Name: "_live_own", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "own::module::stub", Kind: graph.KindFunction, Name: "stub", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "own::MODULE::source", Kind: graph.KindFunction, Name: "_case_sensitive", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "global-unmarked", Kind: graph.KindFunction, Name: "_global_own", Language: "python", FilePath: "external::shared"},
		{ID: "ext::caller", Kind: graph.KindFunction, Name: "caller", Language: "python", FilePath: "external::caller", Meta: map[string]any{"external": true}},
	}, []*graph.Edge{{From: "ext::caller", To: "own-live", Kind: graph.EdgeCalls}})
	run := func() (string, int) {
		rows := 0
		ctx := outlineBoundContext(s, "eligibility", "ws", nil)
		reader := newContractCoreEdges(&eligibilityCandidateSpy{Store: store, rows: &rows}, ctx, nil)
		ctx = withRequestView(ctx, &requestView{reader: reader})
		result, err := s.handleFindDeadCode(ctx, makeReq("find_dead_code", map[string]any{"format": "json"}))
		require.NoError(t, err)
		body := toolResultText(result)
		require.Contains(t, body, "own-private")
		require.Contains(t, body, "own::MODULE::source", "identity checks are case sensitive")
		require.Contains(t, body, "global-unmarked", "global visibility is not an ownership-based exclusion")
		require.NotContains(t, body, "own-live", "a flagged foreign caller remains valid usage evidence")
		return body, rows
	}
	before, rowsBefore := run()
	nodes := make([]*graph.Node, 1000)
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("ext::dependency::%04d", i), Kind: graph.KindFunction, Name: "dependency", FilePath: "external::dependency", Meta: map[string]any{"external": true}}
	}
	store.AddBatch(nodes, nil)
	after, rowsAfter := run()
	require.JSONEq(t, before, after)
	require.Equal(t, rowsBefore, rowsAfter, "already rejected globals must not reach candidate hydration/usage work")
}

func TestDeadCodeCandidateExclusionsKeepSelectedReaderEvidence(t *testing.T) {
	_, store := outlineFixture(t)
	require.NoError(t, store.AddBatchChecked([]*graph.Node{
		{ID: "own", Kind: graph.KindFunction, WorkspaceID: "ws"},
		{ID: "foreign", Kind: graph.KindFunction, FilePath: "external::caller", Meta: map[string]any{"external": true}},
		{ID: "repo::module::stub", Kind: graph.KindFunction, WorkspaceID: "ws"},
	}, []*graph.Edge{{From: "foreign", To: "own", Kind: graph.EdgeCalls}}))
	exclude := graph.DeadCodeCandidateExclusions{Stubs: true, External: true}
	for _, masked := range []map[string]bool{nil, {"foreign": true}} {
		ctx := withContractCoreReadErrors(t.Context())
		fast := newContractCoreEdges(store, ctx, masked).(graph.NodeScopedDeadCodeReader)
		fallback := newContractCoreEdges(legacyOutlineReader{store}, ctx, masked).(graph.NodeScopedDeadCodeReader)
		kinds := []graph.NodeKind{graph.KindFunction}
		incoming := map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}
		scope := graph.NodeScope{WorkspaceID: "ws"}
		got := fast.DeadCodeCandidatesInScope(kinds, incoming, nil, scope, exclude)
		require.Equal(t, fallback.DeadCodeCandidatesInScope(kinds, incoming, nil, scope, exclude), got)
		var ids []string
		for _, n := range got {
			ids = append(ids, n.ID)
		}
		require.NotContains(t, ids, "foreign")
		require.NotContains(t, ids, "repo::module::stub")
		if masked != nil {
			require.Contains(t, ids, "own")
		} else {
			require.NotContains(t, ids, "own")
		}
		require.NoError(t, contractCoreReadError(ctx))
	}
}
