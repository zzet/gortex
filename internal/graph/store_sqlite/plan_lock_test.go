package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Access-path plan locks.
//
// Every hot read the resolver and enrichment passes issue must go through a
// vetted access path whose query plan is pinned here. The recurring failure
// mode this table exists to kill: a hand-assembled query left to a
// stats-blind planner picks the wrong index (or a temp B-tree) and turns an
// index probe into a whole-range scan — found three times in production
// profiling before this harness existed (scoped projections, the
// NodesInFilesByKind file projection, and the edge-candidate probes).
//
// The vetted access paths and where their plans are locked:
//   - probe nodes by file      → nodesInFilesByKindQuery        (here)
//   - probe edges by endpoint  → edgeCandidatesEndpointQuery    (here)
//   - probe edges by call site → edgeCandidates*SiteQuery       (here)
//   - probe nodes by name      → FindNodesByNamesInRepoLanguages
//     (repo_language_name_batch_test.go locks it to the compound index)
//   - stream scoped edges      → streamScopedEdges keyset cursor
//     (scoped projection tests lock the no-temp-btree property)
//   - BFS adjacency expansion  → store_bfs.go builders (store_bfs_test.go)
//
// A new hot query lands with a row in this table. Rows run against a
// realistically-shaped fixture WITH planner statistics (ANALYZE), because
// that is the state production stores run in after the bulk-finalize /
// open-heal refresh in planner_stats.go.
func TestHotQueryPlansLocked(t *testing.T) {
	s := newPlanLockFixture(t)
	defer s.Close()

	cases := []struct {
		name  string
		query string
		args  int
		want  []string
		// forbid entries are substring matches against every plan line.
		// Site shapes tolerate a temp B-tree: SELECT DISTINCT legitimately
		// materialises its dedup set, bounded by one chunk's probe results.
		forbid []string
	}{
		{
			name:   "nodes_in_files_by_kind",
			query:  nodesInFilesByKindQuery(3, 2),
			args:   6,
			want:   []string{"USING INDEX nodes_by_file"},
			forbid: []string{"SCAN nodes", "USE TEMP B-TREE"},
		},
		{
			name:  "edge_candidates_endpoint",
			query: edgeCandidatesEndpointQuery(3),
			args:  7,
			// The unique-key autoindex probes (from_id=? AND to_id=?) —
			// better than edges_by_from's prefix probe. Lock the property
			// (an index probe seeded on from_id), not the index name.
			want:   []string{"SEARCH e USING INDEX", "from_id=?"},
			forbid: []string{"SCAN e", "USE TEMP B-TREE"},
		},
		{
			// Site probes must constrain BOTH from_id and line in the index
			// probe. A from_id-only probe (the covering primary key, or
			// edges_by_from without line) re-reads a hub caller's whole
			// out-edge row set once per site — the production misplan that
			// cost the cross-package guard ~980s of a cold index before
			// edges_by_from_line existed. Lock the property, not the name.
			name:   "edge_candidates_exact_site",
			query:  edgeCandidatesExactSiteQuery(3),
			args:   10,
			want:   []string{"SEARCH e USING INDEX", "from_id=? AND line=?"},
			forbid: []string{"SCAN e"},
		},
		{
			name:   "edge_candidates_any_site",
			query:  edgeCandidatesAnySiteQuery(3),
			args:   7,
			want:   []string{"SEARCH e USING INDEX", "from_id=? AND line=?"},
			forbid: []string{"SCAN e"},
		},
		{
			// Repo projections are CROSS JOIN pinned, so the plan shape is
			// the same across cardinality regimes: nodes are probed through
			// a repo-leading index (either nodes_by_repo variant qualifies —
			// the lock is on the property, not the index name), never a
			// global kind scan.
			name:   "repo_node_ids_by_kinds",
			query:  repoNodeIDsByKindsQuery(),
			args:   3,
			want:   []string{"nodes_by_repo_kind (repo_prefix=? AND kind=?)"},
			forbid: []string{"SCAN n", "USE TEMP B-TREE"},
		},
		{
			// Node-first drive must keep the full (from_id, kind) seek on
			// edges — losing the kind column would trade the global scan for
			// per-node over-reads.
			name:  "repo_edges_by_kinds",
			query: repoEdgesByKindsQuery(),
			args:  3,
			want: []string{
				"nodes_by_repo_kind (repo_prefix=?)",
				"SEARCH e USING INDEX edges_by_from (from_id=? AND kind=?)",
			},
			forbid: []string{"SCAN n", "SCAN e", "USE TEMP B-TREE"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, s, tc.query, tc.args)
			joined := strings.Join(plan, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("plan missing %q:\n%s", want, joined)
				}
			}
			for _, forbidden := range tc.forbid {
				for _, line := range plan {
					if strings.Contains(line, forbidden) {
						t.Fatalf("plan contains forbidden %q:\n%s", forbidden, joined)
					}
				}
			}
		})
	}
}

// newPlanLockFixture builds an on-disk store shaped like a small production
// workspace — many files, production-like kind skew (locals dominate,
// functions and methods are the minority the hot projections ask for), and
// call edges fanning out from a subset of symbols — then refreshes planner
// statistics the way a real store gets them.
func newPlanLockFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "plan_lock.sqlite"))
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	// Registered after t.TempDir() so LIFO cleanup closes the database
	// before the directory is removed. Without it the sqlite handle
	// outlives the test and TempDir's RemoveAll fails on Windows, where
	// an open file cannot be unlinked.
	t.Cleanup(func() { _ = s.Close() })
	kinds := []graph.NodeKind{
		graph.KindLocal, graph.KindLocal, graph.KindLocal, graph.KindLocal,
		graph.KindParam, graph.KindParam, graph.KindVariable,
		graph.KindFunction, graph.KindMethod, graph.KindType,
	}
	var nodes []*graph.Node
	var edges []*graph.Edge
	for f := 0; f < 40; f++ {
		file := fmt.Sprintf("pkg/file%02d.go", f)
		for n := 0; n < 30; n++ {
			kind := kinds[n%len(kinds)]
			id := fmt.Sprintf("%s::sym%02d", file, n)
			nodes = append(nodes, &graph.Node{
				ID:         id,
				Name:       fmt.Sprintf("sym%02d", n),
				Kind:       kind,
				FilePath:   file,
				Language:   "go",
				RepoPrefix: fmt.Sprintf("repo%d", f%3),
				StartLine:  n*10 + 1,
				EndLine:    n*10 + 8,
			})
			if kind == graph.KindFunction || kind == graph.KindMethod {
				edges = append(edges, &graph.Edge{
					From:     id,
					To:       fmt.Sprintf("pkg/file%02d.go::sym%02d", (f+1)%40, 7),
					Kind:     graph.EdgeCalls,
					FilePath: file,
					Line:     n*10 + 3,
				})
			}
		}
	}
	s.AddBatch(nodes, edges)
	s.writeMu.Lock()
	statsErr := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if statsErr != nil {
		t.Fatalf("refresh planner stats: %v", statsErr)
	}
	return s
}

// explainQueryPlan runs EXPLAIN QUERY PLAN over the exact production SQL
// with placeholder-count-matching dummy arguments (the driver enforces the
// bound-parameter count even for EXPLAIN) and returns the plan detail lines.
func explainQueryPlan(t *testing.T, s *Store, query string, argCount int) []string {
	t.Helper()
	args := make([]any, argCount)
	for i := range args {
		args[i] = ""
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("empty query plan")
	}
	return plan
}
