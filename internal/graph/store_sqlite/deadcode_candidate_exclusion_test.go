package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeCandidateExclusionsPreserveRawScopeAndLegacyEvidence(t *testing.T) {
	s, _ := openTempStore(t)
	ids := []string{"own", "live", "promoted", "legacy", "stdlib::x", "repo::module::x", "repo::MODULE::x", "repo::nested::module::x", "global"}
	var nodes []*graph.Node
	for _, id := range ids {
		n := &graph.Node{ID: id, Kind: graph.KindFunction, WorkspaceID: "ws"}
		if id == "promoted" || id == "legacy" {
			n.Meta = map[string]any{"external": true}
		}
		if id == "global" {
			n.WorkspaceID = ""
			n.FilePath = "external::shared"
		}
		nodes = append(nodes, n)
	}
	require.NoError(t, s.AddBatchChecked(nodes, []*graph.Edge{{From: "promoted", To: "live", Kind: graph.EdgeCalls}}))
	// A legacy blob-only marker cannot be rejected on promoted column proof.
	blob, err := encodeMeta(map[string]any{"external": true})
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`UPDATE nodes SET external=NULL, meta=? WHERE id='legacy'`, blob)
	require.NoError(t, err)
	query := func(exclude graph.DeadCodeCandidateExclusions, filter graph.OutlineEdgeFilter) ([]string, []*graph.Node) {
		got, queryErr := s.DeadCodeCandidatesInScopeFiltered([]graph.NodeKind{graph.KindFunction}, map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}, nil, graph.NodeScope{WorkspaceID: "ws"}, filter, exclude)
		require.NoError(t, queryErr)
		var out []string
		for _, n := range got {
			out = append(out, n.ID)
		}
		return out, got
	}
	raw, _ := query(graph.DeadCodeCandidateExclusions{}, graph.OutlineEdgeFilter{})
	require.ElementsMatch(t, []string{"own", "promoted", "legacy", "stdlib::x", "repo::module::x", "repo::MODULE::x", "repo::nested::module::x", "global"}, raw)
	exclude := graph.DeadCodeCandidateExclusions{Stubs: true, External: true}
	eligible, got := query(exclude, graph.OutlineEdgeFilter{})
	require.ElementsMatch(t, []string{"own", "legacy", "repo::MODULE::x", "repo::nested::module::x", "global"}, eligible)
	for _, n := range got {
		if n.ID == "legacy" {
			require.Equal(t, true, n.Meta["external"])
			require.False(t, exclude.Allows(n), "decoded final eligibility still rejects legacy blob-only markers")
		}
	}
	// The candidate-only exclusion must not remove the flagged caller's
	// incoming evidence. An explicit adjacency mask can remove that evidence.
	masked, _ := query(exclude, graph.OutlineEdgeFilter{ExcludeNodeIDs: map[string]bool{"promoted": true}})
	require.Contains(t, masked, "live")
}

// Unlike the hydration control, this measures rejected ownership-bucket work
// inside SQLite: adding globals must not hide behind an unchanged result count.
func TestDeadCodeEligibleWorkspacePlanSkipsRejectedGlobalPopulation(t *testing.T) {
	s, _ := openTempStore(t)
	nodes := []*graph.Node{{ID: "own", Kind: graph.KindFunction, WorkspaceID: "ws"}}
	for i := range 1000 {
		nodes = append(nodes, &graph.Node{ID: fmt.Sprint("global-", i), Kind: graph.KindFunction, FilePath: "external::dependency", Meta: map[string]any{"external": true}})
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	// Prove rejected globals are absent from the actual B-tree visited by
	// candidate pages, rather than only absent from hydrated results.
	var eligible int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM nodes INDEXED BY nodes_deadcode_eligible_workspace WHERE external IS NOT 1`).Scan(&eligible))
	require.Equal(t, 1, eligible)
	scope := graph.NodeScope{WorkspaceID: "ws"}
	for _, exclude := range []graph.DeadCodeCandidateExclusions{{}, {External: true, Stubs: true}} {
		q, args := deadCodeNodeScopeVerdictPageSQL(0, false, true, &scope, graph.OutlineEdgeFilter{}, exclude)
		plan := generationLookupPlan(t, s.db, q, append([]any{int64(0), ""}, append(args, "", "function", 5)...)...)
		if exclude.External {
			require.Contains(t, plan, "nodes_deadcode_eligible_workspace")
		} else {
			require.Contains(t, plan, "nodes_by_effective_workspace")
		}
		require.Contains(t, plan, "<expr>=? AND view_gen=? AND kind=? AND id>?")
		require.NotContains(t, plan, "SCAN n")
	}
}

func BenchmarkDeadCodeRejectedGlobalScan(b *testing.B) {
	for _, globals := range []int{0, 10000} {
		b.Run(fmt.Sprint(globals), func(b *testing.B) {
			s, err := Open(b.TempDir() + "/scope.db")
			require.NoError(b, err)
			b.Cleanup(func() { _ = s.Close() })
			nodes := []*graph.Node{{ID: "own", Kind: graph.KindFunction, WorkspaceID: "ws"}}
			for i := range globals {
				nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("global-%05d", i), Kind: graph.KindFunction, FilePath: "external::dependency", Meta: map[string]any{"external": true}})
			}
			require.NoError(b, s.AddBatchChecked(nodes, nil))
			b.ResetTimer()
			for b.Loop() {
				got, err := s.DeadCodeCandidatesInScopeFiltered([]graph.NodeKind{graph.KindFunction}, nil, nil, graph.NodeScope{WorkspaceID: "ws"}, graph.OutlineEdgeFilter{}, graph.DeadCodeCandidateExclusions{External: true, Stubs: true})
				if err != nil || len(got) != 1 || got[0].ID != "own" {
					b.Fatal(len(got), err)
				}
			}
		})
	}
}
