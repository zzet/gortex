package store_sqlite

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBaseRepoKindProjectionPlansKeepPositiveGenerationQueries(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	for _, generation := range []int64{0, 6, 62, 63, 64, 65} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			queries := []struct {
				query string
				args  []any
			}{
				{resolverScopedProjectionHighWaterQuery, []any{"repo", graph.KindFile, generation}},
				{resolverScopedProjectionPageQuery("id, file_path, repo_prefix, workspace_id", false), []any{"repo", graph.KindFile, "z", generation, 256}},
				{resolverScopedProjectionPageQuery("id, file_path, repo_prefix, workspace_id", true), []any{"repo", graph.KindFile, "a", "z", generation, 256}},
			}
			for _, q := range queries {
				actual := resolverScopedProjectionQueryForGeneration(q.query, generation, graph.KindFile)
				if generation != 0 && actual != q.query {
					t.Fatalf("positive generation query changed: %s", actual)
				}
				if generation == 0 {
					plan := scopedProjectionPlan(t, store, actual, q.args...)
					if !strings.Contains(plan, "nodes_by_repo_kind") || strings.Contains(plan, "TEMP B-TREE") {
						t.Fatalf("base query does not seek the repository/kind: %s", plan)
					}
				}
			}
			// Common symbol kinds keep their original generation-selective SQL,
			// including base0, rather than inheriting the sparse-kind optimization.
			for _, query := range []string{resolverScopedProjectionHighWaterQuery, resolverScopedProjectionPageQuery("id, name", false), resolverScopedProjectionPageQuery("id, name", true)} {
				if got := resolverScopedProjectionQueryForGeneration(query, generation, graph.KindFunction); got != query {
					t.Fatalf("function projection SQL changed: %s", got)
				}
			}
			functionQuery, _, _ := scopedNodeProjectionQuery([]string{"repo"}, nil, string(graph.KindFunction), lookupNodeCols, generation)
			wantFunction := `SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + ` FROM nodes AS n WHERE n.repo_prefix = ? AND n.kind = ? AND n.view_gen = ? AND n.id > ? ORDER BY n.id LIMIT ?`
			if functionQuery != wantFunction {
				t.Fatalf("full function SQL changed: %s", functionQuery)
			}
			for _, kind := range []graph.NodeKind{graph.KindFile, graph.KindContract} {
				q, args, ok := scopedNodeProjectionQuery([]string{"repo"}, nil, string(kind), lookupNodeCols, generation)
				if !ok {
					t.Fatal("missing exact repository query")
				}
				if generation != 0 {
					want := `SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + ` FROM nodes AS n WHERE n.repo_prefix = ? AND n.kind = ? AND n.view_gen = ? AND n.id > ? ORDER BY n.id LIMIT ?`
					if q != want {
						t.Fatalf("positive generation SQL changed: %s", q)
					}
				} else {
					plan := scopedProjectionPlan(t, store, q, append(args, "", 256)...)
					if !strings.Contains(plan, "nodes_by_repo_kind") || strings.Contains(plan, "TEMP B-TREE") {
						t.Fatalf("base full node query is not kind-selective: %s", plan)
					}
				}
			}
		})
	}
}

func TestBaseRepoKindProjectionPreservesPayloadAndKeysetBoundaries(t *testing.T) {
	store := openResolverProjectionTestStore(t)
	const count = resolverProjectionPageSize*2 + 3
	for _, generation := range []int64{0, 62} {
		handle := store.AtGeneration(generation)
		var nodes []*graph.Node
		for i := 0; i < count; i++ {
			for _, kind := range []graph.NodeKind{graph.KindFile, graph.KindContract, graph.KindFunction} {
				for _, repo := range []string{"repo", "sibling", ""} {
					id := fmt.Sprintf("%s::%s-%04d", repo, kind, i)
					nodes = append(nodes, &graph.Node{ID: id, Kind: kind, RepoPrefix: repo,
						Name: fmt.Sprintf("generation-%d", generation), FilePath: fmt.Sprintf("%s/generation-%d/%04d", repo, generation, i),
						WorkspaceID: fmt.Sprintf("workspace-%d", generation), Meta: map[string]any{"generation": generation}})
				}
			}
		}
		if err := handle.AddBatchChecked(nodes, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, generation := range []int64{0, 6, 62, 63} {
		for _, repo := range []string{"repo", ""} {
			wantCount := 0
			if generation == 0 || generation == 62 {
				wantCount = count
			}
			handle := store.AtGeneration(generation)
			var fileIDs []string
			for row := range handle.FileNodeIdentitiesSeq([]string{repo}) {
				if row.RepoPrefix != repo || row.WorkspaceID != fmt.Sprintf("workspace-%d", generation) {
					t.Fatalf("file projection leaked ownership: %#v", row)
				}
				fileIDs = append(fileIDs, row.ID)
			}
			if len(fileIDs) != wantCount {
				t.Fatalf("generation %d repo %q files=%d want %d", generation, repo, len(fileIDs), wantCount)
			}
			for _, kind := range []graph.NodeKind{graph.KindFile, graph.KindContract} {
				var ids []string
				for node := range handle.NodesInScopeSeq([]string{repo}, nil, kind) {
					if node.RepoPrefix != repo || node.Kind != kind || node.Name != fmt.Sprintf("generation-%d", generation) || node.Meta == nil {
						t.Fatalf("full node projection leaked payload: %#v", node)
					}
					ids = append(ids, node.ID)
				}
				if len(ids) != wantCount {
					t.Fatalf("generation %d repo %q kind %s rows=%d want %d", generation, repo, kind, len(ids), wantCount)
				}
				if kind == graph.KindFile && !reflect.DeepEqual(ids, fileIDs) {
					t.Fatal("compact and full file keysets differ")
				}
			}
		}
	}
	// The hint is not INDEXED BY: a missing optional index changes cost,
	// never ownership or row completeness.
	if _, err := store.writerDB.Exec(`DROP INDEX nodes_by_repo_kind`); err != nil {
		t.Fatal(err)
	}
	if got := collectResolverProjection(store.FileNodeIdentitiesSeq([]string{"repo"})); len(got) != count {
		t.Fatalf("index-absent compact file rows=%d want %d", len(got), count)
	}
	if got := collectResolverProjection(store.NodesInScopeSeq([]string{"repo"}, nil, graph.KindContract)); len(got) != count {
		t.Fatalf("index-absent contract rows=%d want %d", len(got), count)
	}
}
