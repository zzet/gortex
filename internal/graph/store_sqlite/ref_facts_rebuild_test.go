package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func openRefFactRebuildStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "ref-facts.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func refFactTestNode(id, name, file, repo string, kind graph.NodeKind) *graph.Node {
	return &graph.Node{ID: id, Name: name, FilePath: file, RepoPrefix: repo, Language: "go", Kind: kind}
}

func factsByKey(facts []graph.RefFact) map[string]graph.RefFact {
	out := make(map[string]graph.RefFact, len(facts))
	for _, fact := range facts {
		out[fact.FromID+"->"+fact.ToID+":"+fact.Kind] = fact
	}
	return out
}

func TestRebuildRefFactsForReposPreservesFactSemantics(t *testing.T) {
	store := openRefFactRebuildStore(t)
	nodes := []*graph.Node{
		refFactTestNode("repoA::a.go", "a.go", "repoA/a.go", "repoA", graph.KindFile),
		refFactTestNode("repoA::a.go::Caller", "Caller", "repoA/a.go", "repoA", graph.KindFunction),
		refFactTestNode("repoA::a.go::Impl", "Impl", "repoA/a.go", "repoA", graph.KindType),
		refFactTestNode("repoB::b.go::Target", "Target", "repoB/b.go", "repoB", graph.KindFunction),
		refFactTestNode("repoB::b.go::Iface", "Iface", "repoB/b.go", "repoB", graph.KindInterface),
	}
	edges := []*graph.Edge{
		{From: "repoA::a.go::Caller", To: "repoB::b.go::Target", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7, Confidence: 1, Meta: map[string]any{"semantic_source": "go-types"}},
		{From: "repoA::a.go::Caller", To: "repoB::b.go::Iface", Kind: graph.EdgeReferences, FilePath: "repoA/a.go", Line: 8, Confidence: 0.7},
		{From: "repoA::a.go::Impl", To: "repoB::b.go::Iface", Kind: graph.EdgeImplements, FilePath: "repoA/a.go", Line: 9, Confidence: 1, Meta: map[string]any{"semantic_source": "go-types"}},
		{From: "repoA::a.go::Caller", To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 10, Confidence: 1},
		{From: "repoA::a.go::Caller", To: "repoA::stdlib::fmt.Printf", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 11, Confidence: 1},
		{From: "repoA::a.go::Caller", To: "repoB::b.go::Target", Kind: graph.EdgeDefines, FilePath: "repoA/a.go", Line: 12, Confidence: 1},
	}
	store.AddBatch(nodes, edges)

	statements, err := store.rebuildRefFactsForRepos([]string{"repoA", "repoA"})
	require.NoError(t, err)
	require.Equal(t, 2, statements, "one scoped delete plus one INSERT SELECT")

	facts, err := store.LoadRefFactsByFiles("repoA", nil)
	require.NoError(t, err)
	require.Len(t, facts, 3)
	byKey := factsByKey(facts)

	call := byKey["repoA::a.go::Caller->repoB::b.go::Target:calls"]
	require.Equal(t, "Target", call.RefName, "cross-repo target name comes from the joined target node")
	require.Equal(t, "lsp_resolved", call.Origin)
	require.Equal(t, "lsp", call.Tier)
	require.Equal(t, "repoA/a.go", call.FilePath)
	require.Equal(t, "go", call.Lang)

	reference := byKey["repoA::a.go::Caller->repoB::b.go::Iface:references"]
	require.Equal(t, "ast_inferred", reference.Origin)
	require.Equal(t, "heuristic", reference.Tier)

	implementation := byKey["repoA::a.go::Impl->repoB::b.go::Iface:implements"]
	require.Equal(t, "lsp_dispatch", implementation.Origin)
	require.Equal(t, "lsp", implementation.Tier)
}

func TestReplaceRefFactsForFilesIsAtomicAndPreciselyScoped(t *testing.T) {
	store := openRefFactRebuildStore(t)
	store.AddBatch([]*graph.Node{
		refFactTestNode("repoA::a.go::Caller", "Caller", "shared/a.go", "repoA", graph.KindFunction),
		refFactTestNode("repoA::b.go::Caller", "CallerB", "repoA/b.go", "repoA", graph.KindFunction),
		refFactTestNode("repoA::target", "Fresh", "repoA/target.go", "repoA", graph.KindFunction),
	}, []*graph.Edge{{From: "repoA::a.go::Caller", To: "repoA::target", Kind: graph.EdgeCalls, FilePath: "shared/a.go", Line: 3, Confidence: 0.95}})
	require.NoError(t, store.BulkSetRefFacts("repoA", []graph.RefFact{
		{FromID: "repoA::a.go::Caller", ToID: "stale", Kind: "calls", FilePath: "shared/a.go"},
		{FromID: "repoA::b.go::Caller", ToID: "keep-a", Kind: "calls", FilePath: "repoA/b.go"},
	}))
	require.NoError(t, store.BulkSetRefFacts("repoB", []graph.RefFact{
		{FromID: "repoB::a.go::Caller", ToID: "keep-b", Kind: "calls", FilePath: "shared/a.go"},
	}))

	statements, err := store.replaceRefFactsForFiles("repoA", []string{"shared/a.go", "shared/a.go"})
	require.NoError(t, err)
	require.Equal(t, 2, statements)

	a, err := store.LoadRefFactsByFiles("repoA", nil)
	require.NoError(t, err)
	require.Len(t, a, 2)
	sort.Slice(a, func(i, j int) bool { return a[i].FilePath < a[j].FilePath })
	require.Equal(t, "repoA/b.go", a[0].FilePath)
	require.Equal(t, "keep-a", a[0].ToID)
	require.Equal(t, "shared/a.go", a[1].FilePath)
	require.Equal(t, "repoA::target", a[1].ToID)

	b, err := store.LoadRefFactsByFiles("repoB", nil)
	require.NoError(t, err)
	require.Len(t, b, 1)
	require.Equal(t, "keep-b", b[0].ToID)

	store.AddBatch([]*graph.Node{
		refFactTestNode("rollback", "Rollback", "rollback.go", "repoA", graph.KindFunction),
		refFactTestNode("rollback-target", "RollbackTarget", "target.go", "repoA", graph.KindFunction),
	}, []*graph.Edge{{From: "rollback", To: "rollback-target", Kind: graph.EdgeCalls, FilePath: "rollback.go", Line: 1, Confidence: 1}})
	require.NoError(t, store.BulkSetRefFacts("repoA", []graph.RefFact{{FromID: "rollback", ToID: "survives", Kind: "calls", FilePath: "rollback.go"}}))
	_, err = store.writerDB.Exec(`CREATE TRIGGER fail_ref_fact_rebuild BEFORE INSERT ON ref_facts
BEGIN SELECT RAISE(ABORT, 'forced refill failure'); END`)
	require.NoError(t, err)
	err = store.ReplaceRefFactsForFiles("repoA", []string{"rollback.go"})
	require.ErrorContains(t, err, "forced refill failure")
	rolledBack, err := store.LoadRefFactsByFiles("repoA", []string{"rollback.go"})
	require.NoError(t, err)
	require.Len(t, rolledBack, 1, "failed refill must roll its preceding delete back")
	require.Equal(t, "survives", rolledBack[0].ToID)
}

func TestRefFactRebuildPlanUsesOwnershipAndAdjacencyIndexes(t *testing.T) {
	store := openRefFactRebuildStore(t)
	query := `EXPLAIN QUERY PLAN ` + refFactInsertPrefix + `    FROM json_each(?) AS requested
    JOIN nodes AS n
      ON n.repo_prefix = ? AND n.file_path = CAST(requested.value AS TEXT)
    JOIN edges AS e INDEXED BY edges_by_from ON e.from_id = n.id AND e.view_gen = n.view_gen` + refFactInsertSuffix
	rows, err := store.db.Query(query, `["repoA/a.go"]`, "repoA", store.viewGen, store.viewGen)
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
	plan := strings.Join(details, "\n")
	require.Contains(t, plan, "edges_by_from", plan)
	require.NotContains(t, plan, "SCAN e", plan)
}

func TestBackfillEdgeSemanticSourcesPagesAndIsIdempotent(t *testing.T) {
	store := openRefFactRebuildStore(t)
	meta, err := encodeMeta(map[string]any{"semantic_source": "go-types", "keep": "value"})
	require.NoError(t, err)
	tx, err := store.writerDB.Begin()
	require.NoError(t, err)
	for i := 0; i < edgeSemanticSourceMigrationPageRows+5; i++ {
		_, err := tx.Exec(`INSERT INTO edges
(from_id, to_id, kind, file_path, line, confidence, meta, semantic_source)
VALUES (?, ?, 'calls', 'f.go', ?, 1, ?, NULL)`, fmt.Sprintf("from-%d", i), fmt.Sprintf("to-%d", i), i, meta)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	tx, err = store.writerDB.Begin()
	require.NoError(t, err)
	require.NoError(t, backfillEdgeSemanticSources(tx))
	require.NoError(t, tx.Commit())

	var promoted int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE semantic_source = 'go-types'`).Scan(&promoted))
	require.Equal(t, edgeSemanticSourceMigrationPageRows+5, promoted)
	var blob []byte
	require.NoError(t, store.db.QueryRow(`SELECT meta FROM edges ORDER BY id LIMIT 1`).Scan(&blob))
	remaining, err := decodeMeta(blob)
	require.NoError(t, err)
	require.Equal(t, "value", remaining["keep"])
	require.NotContains(t, remaining, "semantic_source")

	// A second run is a write-free no-op and preserves the promoted rows.
	tx, err = store.writerDB.Begin()
	require.NoError(t, err)
	require.NoError(t, backfillEdgeSemanticSources(tx))
	require.NoError(t, tx.Commit())
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE semantic_source = 'go-types'`).Scan(&promoted))
	require.Equal(t, edgeSemanticSourceMigrationPageRows+5, promoted)
}

func TestV4SemanticSourceMigrationRollsBackAndResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	valid, err := encodeMeta(map[string]any{"semantic_source": "go-types"})
	require.NoError(t, err)
	malformed := append([]byte{metaFlatMagic0, metaFlatVersion, 1, byte(len(edgeSemanticSourceMetaMarker))}, edgeSemanticSourceMetaMarker...)
	_, err = store.writerDB.Exec(`INSERT INTO edges (from_id, to_id, kind, file_path, line, meta, semantic_source)
VALUES ('valid-from', 'valid-to', 'calls', 'f.go', 1, ?, NULL),
       ('bad-from', 'bad-to', 'calls', 'f.go', 2, ?, NULL)`, valid, malformed)
	require.NoError(t, err)
	require.NoError(t, setUserVersion(store.writerDB, 4))
	require.NoError(t, store.Close())

	_, err = Open(path)
	require.Error(t, err)
	withRawDB(t, path, func(db *sql.DB) {
		var version int
		require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
		require.Equal(t, 4, version)
		var source sql.NullString
		require.NoError(t, db.QueryRow(`SELECT semantic_source FROM edges WHERE from_id = 'valid-from'`).Scan(&source))
		require.False(t, source.Valid, "the valid row update must roll back with the bad row")
		_, err := db.Exec(`UPDATE edges SET meta = NULL WHERE from_id = 'bad-from'`)
		require.NoError(t, err)
	})

	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()
	var version int
	require.NoError(t, reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, currentSchemaVersion, version)
	edge := reopened.GetOutEdges("valid-from")
	require.Len(t, edge, 1)
	require.Equal(t, "go-types", edge[0].Meta["semantic_source"])
}
