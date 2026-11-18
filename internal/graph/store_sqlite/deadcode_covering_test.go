package store_sqlite

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeProvenWorkspaceAvoidsPrimaryNodeSeek(t *testing.T) {
	s, _ := openTempStore(t)
	// The effective-workspace seek itself proves this nonempty workspace.
	// EXPLAIN's COVERING label alone is insufficient for expression indexes:
	// inspect executable bytecode for the residual primary-key verification.
	q, args := deadCodeNodeScopeVerdictPageSQL(0, false, true, nil, graph.OutlineEdgeFilter{}, graph.DeadCodeCandidateExclusions{External: true})
	values := append([]any{int64(0), "ws"}, append(args, "", "function", 512)...)
	rows, err := s.db.Query("EXPLAIN "+q, values...)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment any
		require.NoError(t, rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment))
		require.NotEqual(t, "NotFound", opcode, "candidate-only workspace key still visits primary nodes")
	}
	require.NoError(t, rows.Err())
}

func TestDeadCodeSourceMaskUsesCoveringKindIdentity(t *testing.T) {
	s, _ := openTempStore(t)
	filter := graph.OutlineEdgeFilter{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract, graph.KindContract}}
	q, args := deadCodeNodeScopeVerdictPageSQL(0, false, false, nil, filter)
	plan := generationLookupPlan(t, s.db, q, append(args, int64(0), "", "function", 512)...)
	require.Contains(t, plan, "SEARCH hidden_source USING COVERING INDEX nodes_by_kind")
	require.NotContains(t, plan, "SEARCH source USING PRIMARY KEY")
}

func TestDeadCodeProvenWorkspaceKeepsIntersectionsAndGlobalUsage(t *testing.T) {
	s, _ := openTempStore(t)
	nodes := []*graph.Node{
		{ID: "dead", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "live", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "dangling", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "bean", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "wrong-project", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "q"},
		{ID: "wrong-repo", Kind: graph.KindFunction, RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "p"},
		{ID: "legacy", Kind: graph.KindFunction, RepoPrefix: "ws", ProjectID: "p"},
		{ID: "global", Kind: graph.KindFunction, FilePath: "external::shared"},
		{ID: "ordinary-unowned", Kind: graph.KindFunction, FilePath: "ordinary.go"},
		{ID: "foreign", Kind: graph.KindFunction, WorkspaceID: "other"},
		{ID: "hidden", Kind: graph.KindContract, WorkspaceID: "other"},
	}
	edges := []*graph.Edge{{From: "foreign", To: "live", Kind: graph.EdgeCalls}, {From: "hidden", To: "dead", Kind: graph.EdgeCalls}, {From: "missing", To: "dangling", Kind: graph.EdgeCalls}, {From: "foreign", To: "bean", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}}}
	require.NoError(t, s.AddBatchChecked(nodes, edges))
	// Another generation must not provide the source kind for this generation.
	require.NoError(t, s.AtGeneration(7).AddBatchChecked([]*graph.Node{{ID: "hidden", Kind: graph.KindFunction}}, nil))
	filter := graph.OutlineEdgeFilter{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract, graph.KindContract}, ExcludeVia: "spring.Bean"}
	kinds := []graph.NodeKind{graph.KindFunction}
	incoming := map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}
	for _, scope := range []graph.NodeScope{{WorkspaceID: "ws"}, {WorkspaceID: "ws", ProjectID: "p"}, {WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": true, "ws": true}}, {WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": false}}} {
		for _, prefixes := range [][]string{nil, {"a", "ws", ""}, {}} {
			got, err := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, prefixes, scope, filter, graph.DeadCodeCandidateExclusions{External: true, Stubs: true})
			require.NoError(t, err)
			all, err := s.DeadCodeCandidatesInReposFiltered(kinds, incoming, prefixes, filter)
			require.NoError(t, err)
			var want []*graph.Node
			for _, n := range all {
				if scope.Allows(n) {
					want = append(want, n)
				}
			}
			require.Equal(t, want, got, "scope=%+v prefixes=%v", scope, prefixes)
		}
	}
	got, err := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, nil, graph.NodeScope{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": true, "ws": true}}, filter, graph.DeadCodeCandidateExclusions{External: true})
	require.NoError(t, err)
	var ids []string
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	require.ElementsMatch(t, []string{"dead", "bean", "legacy", "global"}, ids)
	// Dropped secondary indexes retain exact results through the unhinted path.
	_, err = s.writerDB.Exec(`DROP INDEX nodes_by_kind; DROP INDEX nodes_deadcode_eligible_workspace`)
	require.NoError(t, err)
	again, err := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, nil, graph.NodeScope{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": true, "ws": true}}, filter, graph.DeadCodeCandidateExclusions{External: true})
	require.NoError(t, err)
	require.Equal(t, got, again)
	_, err = s.writerDB.Exec(`CREATE INDEX nodes_by_kind ON nodes(kind,view_gen)`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(nodesDeadCodeEligibleWorkspaceDDL)
	require.NoError(t, err)
}

func TestDeadCodeCoveringUpgradeReplacesExistingKeyAndPreservesAnalysis(t *testing.T) {
	path, header := seedIndexMigrationAnalysis(t, 42)
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`CREATE INDEX nodes_deadcode_eligible_workspace ON nodes(COALESCE(NULLIF(workspace_id,''),repo_prefix),view_gen,kind,id) WHERE external IS NOT 1`)
		require.NoError(t, err)
	})
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	requireKeptIndexMigrationAnalysis(t, s, header)
	var ddl string
	require.NoError(t, s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='nodes_deadcode_eligible_workspace'`).Scan(&ddl))
	require.Contains(t, ddl, "id, external, workspace_id, repo_prefix")
}
