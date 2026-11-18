package store_sqlite

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type noopFTSDocument struct {
	RowID                  int64
	NodeID, Repo, Tokens   string
	OwnerNodeID, OwnerRepo string
}

func readNoopFTSDocuments(t *testing.T, s *Store) []noopFTSDocument {
	t.Helper()
	rows, err := s.db.Query(`SELECT f.rowid,f.node_id,f.repo_prefix,f.tokens,m.node_id,m.repo_prefix
FROM symbol_fts_rowid m JOIN symbol_fts f ON f.rowid=m.fts_rowid
WHERE m.view_gen=? ORDER BY f.rowid`, s.viewGen)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []noopFTSDocument
	for rows.Next() {
		var row noopFTSDocument
		require.NoError(t, rows.Scan(&row.RowID, &row.NodeID, &row.Repo, &row.Tokens, &row.OwnerNodeID, &row.OwnerRepo))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func readNoopFTSRevision(t *testing.T, s *Store) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, s.db.QueryRow(`SELECT rev FROM symbol_fts_generation_revisions WHERE view_gen=?`, s.viewGen).Scan(&revision))
	return revision
}

func TestSymbolFTSIdenticalUpsertPreservesExactRankingAndPrefixCache(t *testing.T) {
	s := openReindexReceiptTestStore(t)
	nodes, items := ftsDocs("needle", 3, func(i int) string { return fmt.Sprintf("common token%d", i) })
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	require.NoError(t, s.BatchUpsertSymbolFTS(items))
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	prefixes := []string{"need", "common", "missing"}
	beforeStats, beforeCounts, err := s.SymbolFTSScoringSnapshot(t.Context(), prefixes)
	require.NoError(t, err)
	beforeHits, err := s.SearchSymbols("needle common", 2)
	require.NoError(t, err)
	require.Len(t, beforeHits, 2)
	beforeRows := readNoopFTSDocuments(t, s)
	beforeRevision := readNoopFTSRevision(t, s)
	// A source span/metadata update is real graph work, but these actual FTS
	// documents remain exactly equal. Their revision and score inputs must too.
	nodes[0].EndLine = 31
	nodes[0].Meta = map[string]any{"presentation": "changed"}
	require.NoError(t, s.AddBatchChecked(nodes[:1], nil))
	require.NoError(t, s.BatchUpsertSymbolFTS(items))
	require.Equal(t, beforeRows, readNoopFTSDocuments(t, s))
	require.Equal(t, beforeRevision, readNoopFTSRevision(t, s))
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	afterStats, afterCounts, err := s.SymbolFTSScoringSnapshot(t.Context(), prefixes)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, beforeStats, afterStats)
	require.Equal(t, beforeCounts, afterCounts)
	require.Zero(t, walks, "unchanged exact documents retain their validated prefix baseline")
	afterHits, err := s.SearchSymbols("needle common", 2)
	require.NoError(t, err)
	require.Equal(t, beforeHits, afterHits)
	for i := range beforeHits {
		require.Equal(t, math.Float64bits(beforeHits[i].Score), math.Float64bits(afterHits[i].Score))
	}
}

func TestSymbolFTSMixedNoopsKeepDuplicateWinnersAndRowOrder(t *testing.T) {
	s := openReindexReceiptTestStore(t)
	nodes, items := ftsDocs("initial", ftsInsertChunkRows+2, func(int) string { return "common" })
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	require.NoError(t, s.BatchUpsertSymbolFTS(items[:ftsInsertChunkRows]))
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	before := readNoopFTSDocuments(t, s)
	beforeRevision := readNoopFTSRevision(t, s)
	mixed := append([]graph.SymbolFTSItem(nil), items...)
	mixed[0].Tokens = "discarded intermediate"
	mixed[1].Tokens = "changed winner"
	mixed = append(mixed, graph.SymbolFTSItem{NodeID: "", Tokens: "ignored"}, items[0])
	require.NoError(t, s.BatchUpsertSymbolFTS(mixed))
	after := readNoopFTSDocuments(t, s)
	require.Len(t, after, len(items))
	for i := range before {
		require.Equal(t, before[i].RowID, after[i].RowID, "existing documents retain their tie-order rowids")
		if i != 1 {
			require.Equal(t, before[i], after[i])
		}
	}
	require.Equal(t, "changed winner", after[1].Tokens)
	require.Equal(t, items[ftsInsertChunkRows].NodeID, after[ftsInsertChunkRows].NodeID)
	require.Equal(t, items[ftsInsertChunkRows+1].NodeID, after[ftsInsertChunkRows+1].NodeID)
	require.Less(t, after[ftsInsertChunkRows].RowID, after[ftsInsertChunkRows+1].RowID)
	require.Greater(t, readNoopFTSRevision(t, s), beforeRevision, "real changes still invalidate prefix counts")
	for _, tokens := range []string{"discarded", "ignored"} {
		hits, err := s.SearchSymbols(tokens, 10)
		require.NoError(t, err)
		require.Empty(t, hits)
	}
}

func TestSymbolFTSNoopRequiresActualDocumentAndOwner(t *testing.T) {
	for _, fault := range []string{"missing_document", "map_owner", "document_owner", "document_node", "null_tokens", "node_owner"} {
		t.Run(fault, func(t *testing.T) {
			s := openReindexReceiptTestStore(t)
			nodes, items := ftsDocs("needle", 1, func(int) string { return "common" })
			require.NoError(t, s.AddBatchChecked(nodes, nil))
			require.NoError(t, s.BatchUpsertSymbolFTS(items))
			require.NoError(t, s.EnsureRowCounters(t.Context()))
			before := readNoopFTSDocuments(t, s)[0]
			wantRepo := nodes[0].RepoPrefix
			switch fault {
			case "missing_document":
				_, err := s.writerDB.Exec(`DELETE FROM symbol_fts WHERE rowid=?`, before.RowID)
				require.NoError(t, err)
			case "map_owner":
				_, err := s.writerDB.Exec(`UPDATE symbol_fts_rowid SET repo_prefix='wrong' WHERE fts_rowid=?`, before.RowID)
				require.NoError(t, err)
			case "document_owner", "document_node", "null_tokens":
				column, value := "repo_prefix", any("wrong")
				switch fault {
				case "document_node":
					column = "node_id"
				case "null_tokens":
					column, value = "tokens", nil
				}
				_, err := s.writerDB.Exec(`UPDATE symbol_fts SET `+column+`=? WHERE rowid=?`, value, before.RowID)
				require.NoError(t, err)
			case "node_owner":
				wantRepo = "moved"
				nodes[0].RepoPrefix = wantRepo
				require.NoError(t, s.AddBatchChecked(nodes, nil))
			}
			revision := readNoopFTSRevision(t, s)
			require.NoError(t, s.BatchUpsertSymbolFTS(items))
			got := readNoopFTSDocuments(t, s)
			require.Equal(t, []noopFTSDocument{{RowID: before.RowID, NodeID: items[0].NodeID, Repo: wantRepo, Tokens: items[0].Tokens, OwnerNodeID: items[0].NodeID, OwnerRepo: wantRepo}}, got)
			require.Greater(t, readNoopFTSRevision(t, s), revision, "a repair must not masquerade as an unchanged row")
		})
	}
}

func TestSymbolFTSNoopsPreserveGenerationIsolationAndFreshRows(t *testing.T) {
	s, generation, layer := beginManifestGeneration(t)
	nodes, items := ftsDocs("same", 2, func(int) string { return "needle" })
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	require.NoError(t, s.BatchUpsertSymbolFTS(items))
	base := readNoopFTSDocuments(t, s)
	// The IDs/tokens are identical but the selected generation is different:
	// it needs its own documents and exact independently derived owner.
	for _, node := range nodes {
		node.RepoPrefix = "layer-owner"
	}
	require.NoError(t, layer.AddBatchChecked(nodes, nil))
	require.NoError(t, layer.BatchUpsertSymbolFTS(items))
	layerRows := readNoopFTSDocuments(t, layer)
	require.Len(t, layerRows, len(items))
	for i := range items {
		require.NotEqual(t, base[i].RowID, layerRows[i].RowID)
		require.Equal(t, "layer-owner", layerRows[i].Repo)
	}
	_, complete := s.TakeFreshSymbolFTSRows(generation)
	require.True(t, complete)
	// Taking the handoff then re-upserting an unchanged logical set must
	// restore the complete fresh handoff, despite no physical FTS writes.
	require.NoError(t, layer.BatchUpsertSymbolFTS(items))
	fresh, complete := s.TakeFreshSymbolFTSRows(generation)
	require.True(t, complete)
	require.Len(t, fresh, len(items))
	for i, row := range fresh {
		require.Equal(t, layerRows[i].RowID, row.RowID)
		require.Equal(t, items[i].NodeID, row.NodeID)
		require.Equal(t, items[i].Tokens, row.Tokens)
		require.Equal(t, "layer-owner", row.RepoPrefix)
	}
	require.Equal(t, base, readNoopFTSDocuments(t, s))
	require.Equal(t, layerRows, readNoopFTSDocuments(t, layer))
}
