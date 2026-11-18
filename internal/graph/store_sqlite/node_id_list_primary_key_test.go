package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// generationIndexTieNodeStats are the node statistics of a 188k-node store
// holding two generations of one corpus, sampled with the refresh's analysis
// limit. Under them every read that loads node columns for a list of ids on one
// generation took nodes_by_generation (view_gen=? AND id=?) over the primary
// key.
var generationIndexTieNodeStats = [][2]string{
	{"nodes_by_file", "188269 17 17"},
	{"nodes_by_generation", "188269 1001 1"},
	{"nodes_by_kind", "188269 667 667"},
	{"nodes_by_name", "188269 2 2 1 1 1 1"},
	{"nodes_by_repo", "188269 1001 1001"},
	{"nodes_by_repo_kind", "188269 1001 1001"},
	{"nodes_by_repo_language_name", "188269 1001 143 2 2"},
	{"nodes_go_receiver_type", "3565 1001 15 1 1"},
	{"nodes_stats_histogram", "188269 1001 1001 1001"},
}

const (
	idListFiles   = 40
	idListPerFile = 12
	idListBaseID  = "repo/only/base.go::BaseOnly"
	idListGenID   = "repo/only/gen.go::GenOnly"
)

func idListSymbolID(f, i int) string {
	return fmt.Sprintf("repo/pkg%d/f%03d.go::S%03d_%02d", f%8, f, f, i)
}

// idListName names a symbol's row in one generation, so a read shows which
// generation's row it returned.
func idListName(id string, generation int64) string {
	return fmt.Sprintf("Sym%s_g%d", id[strings.LastIndex(id, "::S")+3:], generation)
}

// writeIDListFixture writes the same symbols into the base corpus and into one
// published generation, each row named for its generation, plus one symbol
// only the base holds and one only the generation holds. Every third symbol is
// a Java method, so the contract seed read has rows to load. It returns both
// handles and the shared ids, sorted.
func writeIDListFixture(t *testing.T) (base, published *Store, ids []string) {
	t.Helper()
	store := openCatalogStore(t)
	ctx := context.Background()
	nodesFor := func(generation int64, only string) []*graph.Node {
		var nodes []*graph.Node
		for f := 0; f < idListFiles; f++ {
			for i := 0; i < idListPerFile; i++ {
				id := idListSymbolID(f, i)
				kind, language := graph.KindFunction, "go"
				if i%3 == 0 {
					kind, language = graph.KindMethod, "java"
				}
				nodes = append(nodes, &graph.Node{ID: id, Kind: kind, Name: idListName(id, generation),
					FilePath: id[:strings.Index(id, "::")], RepoPrefix: "repo", Language: language,
					StartLine: i*10 + 1, EndLine: i*10 + 5})
			}
		}
		return append(nodes, &graph.Node{ID: only, Kind: graph.KindFunction, Name: "Only",
			FilePath: only[:strings.Index(only, "::")], RepoPrefix: "repo", Language: "go"})
	}
	store.AddBatch(nodesFor(baseViewGeneration, idListBaseID), nil)
	generation, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-id-list", LayerID: "layer-id-list",
		GenerationKind: "commit", TreeOID: "tree-id-list", CreatedAt: 10,
	})
	require.NoError(t, err)
	handle.AddBatch(nodesFor(generation, idListGenID), nil)
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 20))
	for f := 0; f < idListFiles; f += 3 {
		for i := 0; i < idListPerFile; i += 2 {
			ids = append(ids, idListSymbolID(f, i))
		}
	}
	sort.Strings(ids)
	return store, store.AtGeneration(generation), ids
}

type idListRead struct {
	name string
	call func(t *testing.T)
}

// idListReads are the reads that load node columns for a list of ids on one
// generation, each called on the published generation (the name read also on
// the base, where it takes the unindexed path).
func idListReads(base, h *Store, ids []string) []idListRead {
	ctx := context.Background()
	readTx := func(t *testing.T, fn func(tx *sql.Tx)) {
		tx, err := h.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		fn(tx)
	}
	return []idListRead{
		{"GetNodesByIDs", func(t *testing.T) { require.Len(t, h.GetNodesByIDs(ids), len(ids)) }},
		{"GetNodesByIDsContext", func(t *testing.T) {
			got, err := h.GetNodesByIDsContext(ctx, ids)
			require.NoError(t, err)
			require.Len(t, got, len(ids))
		}},
		{"LayerContractIDProjectionContext", func(t *testing.T) {
			got, err := h.LayerContractIDProjectionContext(ctx, ids)
			require.NoError(t, err)
			require.Len(t, got.SourceNodes, len(ids))
		}},
		{"FindNodesByNameContainingFilteredContext (sealed generation)", func(t *testing.T) {
			_, indexed := h.sealedNameIndex(ctx)
			require.True(t, indexed, "the published generation has no name index")
			got, err := h.FindNodesByNameContainingFilteredContext(ctx, "Sym003_", 0, graph.NameSearchFilter{})
			require.NoError(t, err)
			require.Len(t, got, idListPerFile)
		}},
		{"FindNodesByNameContainingFilteredContext (base)", func(t *testing.T) {
			got, err := base.FindNodesByNameContainingFilteredContext(ctx, "Sym003_", 0, graph.NameSearchFilter{})
			require.NoError(t, err)
			require.Len(t, got, idListPerFile)
		}},
		{"NodeIdentityLocalizationSummariesContext", func(t *testing.T) {
			for _, includeTest := range []bool{false, true} {
				got, err := h.NodeIdentityLocalizationSummariesContext(ctx, ids, includeTest)
				require.NoError(t, err)
				require.Len(t, got, len(ids))
			}
		}},
		{"NodeIdentityMaskSummariesContext", func(t *testing.T) {
			masks := make([]NodeIdentityMask, 0, len(ids))
			for _, id := range ids {
				masks = append(masks, NodeIdentityMask{NodeID: id, Kind: NodeIdentityMaskReplace})
			}
			got, err := h.NodeIdentityMaskSummariesContext(ctx, masks)
			require.NoError(t, err)
			require.Len(t, got, len(ids))
		}},
		{"NodeNamesByIDs", func(t *testing.T) { require.Len(t, h.NodeNamesByIDs(ids), len(ids)) }},
		{"ReadConstantValueProjectionContext", func(t *testing.T) {
			got, err := h.ReadConstantValueProjectionContext(ctx, ids, nil)
			require.NoError(t, err)
			require.Len(t, got.Nodes, len(ids))
		}},
		{"ContractSeedNodesContext", func(t *testing.T) {
			got, err := h.ContractSeedNodesContext(ctx, "repo")
			require.NoError(t, err)
			require.Len(t, got, idListFiles*idListPerFile/3)
		}},
		{"mutationNodeIdentitiesTx", func(t *testing.T) {
			readTx(t, func(tx *sql.Tx) {
				got, err := mutationNodeIdentitiesTx(tx, h.viewGen, ids)
				require.NoError(t, err)
				require.Len(t, got, len(ids))
			})
		}},
		{"GetNodeKindsByIDsContext", func(t *testing.T) {
			got, err := h.GetNodeKindsByIDsContext(ctx, ids)
			require.NoError(t, err)
			require.Len(t, got, len(ids))
		}},
		{"GetNodeKindsByIDsContext (observed)", func(t *testing.T) {
			got, err := h.GetNodeKindsByIDsContext(graph.WithNodeKindReadObserver(ctx, func(graph.NodeKindReadTiming) {}), ids)
			require.NoError(t, err)
			require.Len(t, got, len(ids))
		}},
		{"NodePlacementsByIDs", func(t *testing.T) { require.Len(t, h.NodePlacementsByIDs(ids), len(ids)) }},
		{"validateVectorCorpusOwnershipTx", func(t *testing.T) {
			items := make([]graph.VectorCorpusItem, 0, len(ids))
			for _, id := range ids {
				items = append(items, graph.VectorCorpusItem{NodeID: id, Vec: []float32{1, 0}})
			}
			readTx(t, func(tx *sql.Tx) {
				require.NoError(t, validateVectorCorpusOwnershipTx(ctx, tx, h.viewGen, "repo", items))
			})
		}},
		{"nodesByIDsOrdered", func(t *testing.T) {
			got, err := h.nodesByIDsOrdered(ctx, ids)
			require.NoError(t, err)
			require.Len(t, got, len(ids))
		}},
		{"nodesByIDsInOrder", func(t *testing.T) {
			got, err := h.nodesByIDsInOrder(ctx, ids)
			require.NoError(t, err)
			require.Len(t, got, len(ids))
		}},
	}
}

// nodeIDListStatement matches a read of the nodes table keyed by an id list.
var nodeIDListStatement = regexp.MustCompile(`(?s)\bFROM nodes(?: INDEXED BY \w+)? WHERE .*\bid IN \(`)

// TestNodeIDListReadsPlanLock locks every read that loads node columns for a
// list of ids on one generation to one primary-key probe per id, under no
// statistics, the statistics of the store that planned them through
// nodes_by_generation, the live store's, and the store's own refresh. Each
// read's statements are the ones the read pool actually ran.
func TestNodeIDListReadsPlanLock(t *testing.T) {
	base, h, ids := writeIDListFixture(t)
	// The maintenance lane would read the observer, and rewrite statistics,
	// from its own goroutine; the checkpoint loop would read the observer too.
	base.stopCheckpointLoop()
	base.stopMaintenanceLane()
	reads := idListReads(base, h, ids)

	var mu sync.Mutex
	var seen []string
	readStatementObserver = func(q string) { mu.Lock(); seen = append(seen, q); mu.Unlock() }
	t.Cleanup(func() { readStatementObserver = nil })
	statements := make(map[string][]string, len(reads))
	for _, r := range reads {
		mu.Lock()
		seen = seen[:0]
		mu.Unlock()
		r.call(t)
		mu.Lock()
		for _, q := range seen {
			if nodeIDListStatement.MatchString(q) {
				statements[r.name] = append(statements[r.name], q)
			}
		}
		mu.Unlock()
		require.NotEmpty(t, statements[r.name], "%s ran no read of nodes by an id list", r.name)
	}
	readStatementObserver = nil

	reload := func(t *testing.T, s *Store) {
		t.Helper()
		_, err := s.writerDB.Exec(`ANALYZE sqlite_schema`)
		require.NoError(t, err)
		recycleStatsReadPool(s.db, s.writerDB)
	}
	states := []statsState{
		{"no statistics", func(t *testing.T, s *Store) {
			_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
			_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
			require.NoError(t, err)
		}},
		{"the store that took nodes_by_generation", func(t *testing.T, s *Store) {
			_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
			_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE tbl = 'nodes'`)
			require.NoError(t, err)
			for _, st := range generationIndexTieNodeStats {
				_, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES ('nodes', ?, ?)`, st[0], st[1])
				require.NoError(t, err)
			}
		}},
		{"live store statistics", withLiveStoreNodeStats},
		{"the store's own refresh", func(t *testing.T, s *Store) {
			s.writeMu.Lock()
			defer s.writeMu.Unlock()
			require.NoError(t, s.refreshPlannerStatsLocked(context.Background()))
		}},
	}
	for _, state := range states {
		state.apply(t, base)
		reload(t, base)
		for _, r := range reads {
			for _, q := range statements[r.name] {
				plan := planOfUnbound(t, base, q)
				t.Logf("%-40s %-62s %s", state.name, r.name, plan)
				require.Contains(t, plan, "SEARCH nodes USING PRIMARY KEY (id=? AND view_gen=?)", "%s under %s: %s", r.name, state.name, q)
				require.NotContains(t, plan, "nodes_by_generation", "%s under %s: %s", r.name, state.name, q)
				require.NotContains(t, plan, "TEMP B-TREE", "%s under %s: %s", r.name, state.name, q)
			}
		}
	}
}

// The primary key the id-list reads pin is the one SQLite reports for the
// nodes table, keyed (id, view_gen).
func TestNodesPrimaryKeyIndexNamePlanLock(t *testing.T) {
	s := openPayloadStore(t)
	var name string
	require.NoError(t, s.writerDB.QueryRow(`SELECT name FROM pragma_index_list('nodes') WHERE origin = 'pk'`).Scan(&name))
	require.Equal(t, nodesPrimaryKeyIndexName, name)
	rows, err := s.writerDB.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, name)
	require.NoError(t, err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"id", "view_gen"}, columns)
}

// The generation re-key rebuilds nodes under another name and renames it; the
// primary key's name comes from the renamed table's declaration, so the pinned
// reads keep planning through it on a migrated store.
func TestNodesPrimaryKeyIndexNameSurvivesTheGenerationRebuild(t *testing.T) {
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	_, err = raw.Exec(`CREATE TABLE nodes (
		id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
		file_path TEXT NOT NULL, repo_prefix TEXT NOT NULL DEFAULT ''
	) WITHOUT ROWID`)
	require.NoError(t, err)
	_, err = raw.Exec(`INSERT INTO nodes (id, kind, name, file_path) VALUES ('a.go::A', 'function', 'A', 'a.go')`)
	require.NoError(t, err)
	tx, err := raw.Begin()
	require.NoError(t, err)
	require.NoError(t, rebuildNodesAtBaseGeneration(tx))
	require.NoError(t, tx.Commit())

	var name string
	require.NoError(t, raw.QueryRow(`SELECT name FROM pragma_index_list('nodes') WHERE origin = 'pk'`).Scan(&name))
	require.Equal(t, nodesPrimaryKeyIndexName, name)
	rows, err := raw.Query(`EXPLAIN QUERY PLAN SELECT name FROM `+nodesByIDSource+` WHERE id IN (?, ?) AND view_gen = ?`, "a.go::A", "b.go::B", 0)
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
	require.Len(t, plan, 1)
	require.Contains(t, plan[0], "SEARCH nodes USING PRIMARY KEY")
	var got string
	require.NoError(t, raw.QueryRow(`SELECT name FROM `+nodesByIDSource+` WHERE id IN (?) AND view_gen = ?`, "a.go::A", 0).Scan(&got))
	require.Equal(t, "A", got)
}

// Pinning the primary key changes no answer: each handle reads its own
// generation's row for an id, never another generation's, skips ids its
// generation does not hold, and the ordered reads stay in id order.
func TestNodeIDListReadsReturnTheirGenerationsRows(t *testing.T) {
	base, h, ids := writeIDListFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		s       *Store
		present string
		absent  string
	}{
		{base, idListBaseID, idListGenID},
		{h, idListGenID, idListBaseID},
	} {
		s, generation := tc.s, tc.s.viewGen
		request := append(append([]string(nil), ids...), tc.present, tc.absent)
		want := make(map[string]string, len(ids)+1)
		for _, id := range ids {
			want[id] = idListName(id, generation)
		}
		want[tc.present] = "Only"

		names := func(nodes map[string]*graph.Node) map[string]string {
			out := make(map[string]string, len(nodes))
			for id, n := range nodes {
				require.Equal(t, id, n.ID)
				out[id] = n.Name
			}
			return out
		}
		require.Equal(t, want, names(s.GetNodesByIDs(request)), "GetNodesByIDs at generation %d", generation)
		got, err := s.GetNodesByIDsContext(ctx, request)
		require.NoError(t, err)
		require.Equal(t, want, names(got), "GetNodesByIDsContext at generation %d", generation)
		require.Equal(t, want, s.NodeNamesByIDs(request), "NodeNamesByIDs at generation %d", generation)

		placements := s.NodePlacementsByIDs(request)
		kinds, err := s.GetNodeKindsByIDsContext(ctx, request)
		require.NoError(t, err)
		require.Len(t, placements, len(want), "NodePlacementsByIDs at generation %d", generation)
		require.Len(t, kinds, len(want), "GetNodeKindsByIDsContext at generation %d", generation)
		for id := range want {
			require.Equal(t, id[:strings.Index(id, "::")], placements[id].FilePath, id)
			require.Equal(t, placements[id].Kind, kinds[id].Kind, id)
		}

		wantOrdered := append(append([]string(nil), ids...), tc.present)
		sort.Strings(wantOrdered)
		sortedRequest := append([]string(nil), request...)
		sort.Strings(sortedRequest)
		ordered, err := s.nodesByIDsOrdered(ctx, sortedRequest)
		require.NoError(t, err)
		inOrder, err := s.nodesByIDsInOrder(ctx, sortedRequest)
		require.NoError(t, err)
		for _, nodes := range [][]*graph.Node{ordered, inOrder} {
			gotIDs := make([]string, 0, len(nodes))
			for _, n := range nodes {
				gotIDs = append(gotIDs, n.ID)
				require.Equal(t, want[n.ID], n.Name, n.ID)
			}
			require.Equal(t, wantOrdered, gotIDs, "ordered reads at generation %d", generation)
		}
	}
}
