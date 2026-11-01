package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// Repo-scoped usages read only the scoped repository's references and the
// call sites parked on the target's name placeholders. Other repositories
// park their own calls on the same bare placeholders; those must change
// neither the usages nor what was read to build them.
func TestFindUsagesScopedNeverScansAndRowsStayFlat(t *testing.T) {
	srv, store, counter := scopedRowServer(t)
	target := scopedRowID("a", scopedRowTarget)
	usages := func() string {
		defaults := scopedRowCall(t, srv, srv.handleFindUsages, t.Context(), "find_usages", map[string]any{"id": target, "repo": "a", "format": "json"})
		nameOnly := scopedRowCall(t, srv, srv.handleFindUsages, t.Context(), "find_usages", map[string]any{"id": target, "repo": "a", "format": "json", "min_tier": "text_matched"})
		return "[" + defaults + "," + nameOnly + "]"
	}
	var payload []struct {
		NameOnlyCandidates int `json:"name_only_candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(usages()), &payload))
	require.Equal(t, 3, payload[0].NameOnlyCandidates, "the scoped repository's three unbound call sites are counted")
	requireScopedReadsStayFlat(t, counter, func() { addUnrelatedScopedRowRepos(t, store, 5) }, usages)
	require.Positive(t, counter.rows()["GetInEdgesByNodeIDsFromRepos"], "the placeholder read is narrowed to the scoped repository")
}

// Narrowing the placeholder read to the requested repositories drops only
// sources the scope check would drop: workspace and project boundaries, test
// exclusion, unowned and missing sources and the contract-core edge mask all
// leave the same candidates as reading every repository's call sites.
func TestNameOnlyCandidatesRepoNarrowMatchesUnnarrowedRead(t *testing.T) {
	store := openContractCoreCapStore(t)
	addScopedRowRepo(t, store, "a")
	addScopedRowRepo(t, store, "b")
	addUnrelatedScopedRowRepos(t, store, 2)
	const bare = graph.UnresolvedMarker + "Target"
	require.NoError(t, store.AddBatchChecked([]*graph.Node{
		{ID: "loose/run.go::Loose", Name: "Loose", Kind: graph.KindFunction, FilePath: "loose/run.go", Language: "go"},
		{ID: "a/svc/other.go::Elsewhere", Name: "Elsewhere", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "other", ProjectID: "other", FilePath: "a/svc/other.go", Language: "go"},
		{ID: "a/svc/run_test.go::TestRun", Name: "TestRun", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/svc/run_test.go", Language: "go"},
		{ID: "a/svc/bean.go::Bean", Name: "Bean", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/svc/bean.go", Language: "go"},
		{ID: "a/api.yaml::contract", Kind: graph.KindContract, RepoPrefix: "a", FilePath: "a/api.yaml"},
	}, []*graph.Edge{
		{From: "loose/run.go::Loose", To: bare, Kind: graph.EdgeCalls, FilePath: "loose/run.go", Line: 2},
		{From: "a/svc/other.go::Elsewhere", To: bare, Kind: graph.EdgeCalls, FilePath: "a/svc/other.go", Line: 3},
		{From: "a/svc/run_test.go::TestRun", To: bare, Kind: graph.EdgeCalls, FilePath: "a/svc/run_test.go", Line: 4},
		{From: "a/svc/bean.go::Bean", To: bare, Kind: graph.EdgeCalls, FilePath: "a/svc/bean.go", Line: 5, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a/api.yaml::contract", To: bare, Kind: graph.EdgeCalls, FilePath: "a/api.yaml", Line: 6},
		{From: "a/svc/ghost.go::Ghost", To: bare, Kind: graph.EdgeCalls, FilePath: "a/svc/ghost.go", Line: 7},
	}))
	target := store.GetNode(scopedRowID("a", scopedRowTarget))
	require.NotNil(t, target)
	narrowed := newContractCoreEdges(store, t.Context(), nil)
	require.Implements(t, (*graph.RepoScopedInEdgeReader)(nil), narrowed)
	unnarrowed := legacyOutlineReader{newContractCoreEdges(legacyOutlineReader{store}, t.Context(), nil)}
	eng := query.NewEngine(store)
	for _, opts := range []query.QueryOptions{
		{RepoAllow: map[string]bool{"a": true}},
		{RepoAllow: map[string]bool{"a": true, "b": true, "unrelated0": false}},
		{RepoAllow: map[string]bool{"a": true}, WorkspaceID: "other"},
		{RepoAllow: map[string]bool{"a": true}, WorkspaceID: "a", ProjectID: "a"},
		{RepoAllow: map[string]bool{"a": true}, ExcludeTests: true},
		{RepoAllow: map[string]bool{"missing": true}},
		{WorkspaceID: "a"},
	} {
		want := eng.WithReader(unnarrowed).FindNameOnlyCandidates(target, nil, opts)
		got := eng.WithReader(narrowed).FindNameOnlyCandidates(target, nil, opts)
		require.Equal(t, want, got, "opts=%+v", opts)
	}
	require.Equal(t, 6, eng.WithReader(narrowed).FindNameOnlyCandidates(target, nil, query.QueryOptions{RepoAllow: map[string]bool{"a": true}}).Total,
		"three sites in a, the unowned caller, the other workspace's caller and the test; the bean, contract and missing callers stay hidden")
}
