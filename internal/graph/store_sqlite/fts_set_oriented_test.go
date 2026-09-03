package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func sqliteExplainPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(details, "\n")
}

func TestFTSOwnershipDeletesSeekIndexedSidecars(t *testing.T) {
	store := openReindexReceiptTestStore(t)

	symbolPlan := sqliteExplainPlan(t, store.db, deleteSymbolFTSForRepoSQL, store.viewGen, "repo")
	require.Contains(t, symbolPlan, "symbol_fts_rowid_by_repo")
	require.Contains(t, symbolPlan, "symbol_fts VIRTUAL TABLE INDEX 0:=",
		"FTS deletion must constrain the virtual table by rowid")

	contentQuery := `DELETE FROM content_fts
WHERE rowid IN (
    SELECT fts_rowid FROM content_fts_rowid WHERE ` + contentOwnerByRepoFile + `
)`
	contentPlan := sqliteExplainPlan(t, store.db, contentQuery, store.viewGen, "repo", "docs/a.md")
	require.Contains(t, contentPlan, "content_fts_rowid_by_repo_file")
	require.Contains(t, contentPlan, "content_fts VIRTUAL TABLE INDEX 0:=",
		"per-file deletion must constrain the virtual table by rowid")

	presencePlan := sqliteExplainPlan(t, store.db,
		`SELECT 1 FROM content_fts_rowid WHERE view_gen = ? AND repo_prefix = ? LIMIT 1`,
		store.viewGen, "repo")
	require.Contains(t, presencePlan, "content_fts_rowid_by_repo_file",
		"cold/warm presence projection must remain an indexed repo seek")
}

func TestBulkSymbolFTSRebuildKeepsRowidMapExactAcrossRepos(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	items := func(repo, token string, n int) []graph.SymbolFTSItem {
		out := make([]graph.SymbolFTSItem, n)
		for i := range out {
			out[i] = graph.SymbolFTSItem{
				NodeID: fmt.Sprintf("%s/f.go::S%d", repo, i),
				Tokens: fmt.Sprintf("%s symbol%d", token, i),
			}
		}
		return out
	}
	require.NoError(t, store.BulkUpsertSymbolFTS("repoA", items("repoA", "stalealpha", 550)))
	require.NoError(t, store.BulkUpsertSymbolFTS("repoB", items("repoB", "betalive", 550)))
	require.NoError(t, store.BulkUpsertSymbolFTS("repoA", items("repoA", "freshalpha", 325)))

	var ftsRows, mapRows int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts`).Scan(&ftsRows))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts_rowid`).Scan(&mapRows))
	require.Equal(t, 875, ftsRows)
	require.Equal(t, ftsRows, mapRows, "every FTS docid must have one ownership row")

	hits, err := store.SearchSymbols("stalealpha", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	hits, err = store.SearchSymbols("betalive", 1000)
	require.NoError(t, err)
	require.Len(t, hits, 550, "rebuilding repoA must preserve repoB")
}

func TestContentFTSRowidMapTracksAppendFileWipeAndSweep(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	hasRows, err := store.ContentRepoHasRows("repoA")
	require.NoError(t, err)
	require.False(t, hasRows)
	require.NoError(t, store.AppendContent("repoA", []graph.ContentFTSItem{
		{NodeID: "repoA/a.md::0", FilePath: "a.md", Body: "alpha old"},
		{NodeID: "repoA/b.md::0", FilePath: "b.md", Body: "bravo live"},
	}))
	require.NoError(t, store.AppendContent("repoB", []graph.ContentFTSItem{
		{NodeID: "repoB/a.md::0", FilePath: "a.md", Body: "beta sibling"},
	}))
	hasRows, err = store.ContentRepoHasRows("repoA")
	require.NoError(t, err)
	require.True(t, hasRows)

	assertExact := func(want int) {
		t.Helper()
		var ftsRows, mapRows int
		require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&ftsRows))
		require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM content_fts_rowid`).Scan(&mapRows))
		require.Equal(t, want, ftsRows)
		require.Equal(t, ftsRows, mapRows)
	}
	assertExact(3)

	require.NoError(t, store.WipeContentFileInRepo("repoA", "a.md"))
	assertExact(2)
	hits, err := store.SearchContent("sibling", "repoB", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "same-named file in sibling repo must survive")

	require.NoError(t, store.DeleteContentFilesForRepoNotIn("repoA", map[string]struct{}{"unrelated.md": {}}))
	assertExact(1)
	hits, err = store.SearchContent("bravo", "repoA", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
}

func TestBackfillContentFTSRowidMapSupportsIndexedWipe(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	_, err := store.writerDB.Exec(`INSERT INTO content_fts
        (node_id, repo_prefix, file_path, ordinal, body)
        VALUES ('repo/legacy.md::0', 'repo', 'legacy.md', 0, 'legacy marker')`)
	require.NoError(t, err)
	require.NoError(t, backfillContentFTSRowidMap(store.writerDB))

	var mapped int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM content_fts_rowid`).Scan(&mapped))
	require.Equal(t, 1, mapped)
	require.NoError(t, store.WipeContentFileInRepo("repo", "legacy.md"))

	var remaining int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&remaining))
	require.Zero(t, remaining)
}

func TestBatchUpsertSymbolFTSUsesBoundedStatementsAndKeepsOwnershipExact(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const symbolCount = 550
	nodes := make([]*graph.Node, 0, symbolCount)
	items := make([]graph.SymbolFTSItem, 0, symbolCount+1)
	for i := 0; i < symbolCount; i++ {
		repo := "repoA"
		if i%2 == 1 {
			repo = "repoB"
		}
		id := fmt.Sprintf("%s/f.go::S%d", repo, i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("S%d", i),
			FilePath: repo + "/f.go", RepoPrefix: repo,
		})
		items = append(items, graph.SymbolFTSItem{NodeID: id, Tokens: "initialbatchtoken"})
	}
	// Last write wins without increasing the SQL batch count.
	items = append(items, graph.SymbolFTSItem{NodeID: items[0].NodeID, Tokens: "initialbatchtoken dedupwinner"})
	store.AddBatch(nodes, nil)
	plan := sqliteExplainPlan(t, store.db, `
WITH wanted(ord, node_id) AS (VALUES (?, ?))
SELECT wanted.ord, COALESCE(nodes.repo_prefix, ''), symbol_fts_rowid.fts_rowid
FROM wanted
LEFT JOIN nodes ON nodes.id = wanted.node_id
LEFT JOIN symbol_fts_rowid
       ON symbol_fts_rowid.view_gen = ? AND symbol_fts_rowid.node_id = wanted.node_id`,
		0, items[0].NodeID, store.viewGen)
	require.Contains(t, plan, "SEARCH nodes USING PRIMARY KEY")
	require.Contains(t, plan, "SEARCH symbol_fts_rowid USING PRIMARY KEY")

	stats, err := store.batchUpsertSymbolFTS(items)
	require.NoError(t, err)
	require.Equal(t, symbolFTSBatchStats{
		allocatorQueries: 1, lookupStatements: 3,
		insertStatements: 3, ownershipStatements: 3, commits: 1,
	}, stats)

	var ftsRows, ownershipRows, exactPairs, repoARows, winnerRows int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts`).Scan(&ftsRows))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts_rowid`).Scan(&ownershipRows))
	require.NoError(t, store.db.QueryRow(`
SELECT COUNT(*) FROM symbol_fts AS f
JOIN symbol_fts_rowid AS o ON o.fts_rowid = f.rowid AND o.node_id = f.node_id`).Scan(&exactPairs))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts_rowid WHERE repo_prefix = 'repoA'`).Scan(&repoARows))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts WHERE symbol_fts MATCH 'dedupwinner'`).Scan(&winnerRows))
	require.Equal(t, symbolCount, ftsRows)
	require.Equal(t, ftsRows, ownershipRows)
	require.Equal(t, ftsRows, exactPairs)
	require.Equal(t, symbolCount/2, repoARows)
	require.Equal(t, 1, winnerRows)

	for i := range items[:symbolCount] {
		items[i].Tokens = "replacementbatchtoken"
	}
	stats, err = store.batchUpsertSymbolFTS(items[:symbolCount])
	require.NoError(t, err)
	require.Equal(t, symbolFTSBatchStats{
		allocatorQueries: 1, lookupStatements: 3, deleteStatements: 3,
		insertStatements: 3, ownershipStatements: 3, commits: 1,
	}, stats)

	var replaced, stale int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts WHERE symbol_fts MATCH 'replacementbatchtoken'`).Scan(&replaced))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts WHERE symbol_fts MATCH 'initialbatchtoken'`).Scan(&stale))
	require.Equal(t, symbolCount, replaced)
	require.Zero(t, stale)
}

func TestResetSymbolFTSThenBoundedAppendsPreservePriorChunksAndSiblings(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	nodes := []*graph.Node{
		{ID: "repoA/a", Name: "A", RepoPrefix: "repoA"},
		{ID: "repoA/b", Name: "B", RepoPrefix: "repoA"},
		{ID: "repoB/c", Name: "C", RepoPrefix: "repoB"},
	}
	store.AddBatch(nodes, nil)
	require.NoError(t, store.BulkUpsertSymbolFTS("repoA", []graph.SymbolFTSItem{
		{NodeID: "repoA/a", Tokens: "stale-a"},
		{NodeID: "repoA/b", Tokens: "stale-b"},
	}))
	require.NoError(t, store.BulkUpsertSymbolFTS("repoB", []graph.SymbolFTSItem{
		{NodeID: "repoB/c", Tokens: "sibling-c"},
	}))

	require.NoError(t, store.ResetSymbolFTS("repoA"))
	require.NoError(t, store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "repoA/a", Tokens: "fresh-a"}}))
	require.NoError(t, store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "repoA/b", Tokens: "fresh-b"}}))

	var repoA, repoB, stale, fresh int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts_rowid WHERE repo_prefix = 'repoA'`).Scan(&repoA))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts_rowid WHERE repo_prefix = 'repoB'`).Scan(&repoB))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts WHERE symbol_fts MATCH 'stale'`).Scan(&stale))
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM symbol_fts WHERE symbol_fts MATCH 'fresh'`).Scan(&fresh))
	require.Equal(t, 2, repoA)
	require.Equal(t, 1, repoB)
	require.Zero(t, stale)
	require.Equal(t, 2, fresh)
}
