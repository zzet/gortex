package store_sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The fixture includes unrelated files in the same repository: an incremental
// refresh must not require a repository-wide node scan to find its frontier.
func seedRefFactWriteFixture(tb testing.TB, store *Store, count, unrelated int) {
	tb.Helper()
	nodes := make([]*graph.Node, 0, count*2+unrelated)
	edges := make([]*graph.Edge, 0, count)
	for i := 0; i < count; i++ {
		from, to := fmt.Sprintf("repo::Caller%d", i), fmt.Sprintf("repo::Target%d", i)
		nodes = append(nodes,
			refFactTestNode(from, "Caller", "repo/changed.go", "repo", graph.KindFunction),
			refFactTestNode(to, "Target", "repo/targets.go", "repo", graph.KindFunction))
		edges = append(edges, &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: "repo/changed.go", Line: i + 1, Confidence: 1})
	}
	for i := 0; i < unrelated; i++ {
		nodes = append(nodes, refFactTestNode(fmt.Sprintf("repo::Other%d", i), "Other", fmt.Sprintf("repo/other%d.go", i), "repo", graph.KindFunction))
	}
	store.AddBatch(nodes, edges)
	require.NoError(tb, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
}

func refFactTotalChanges(tb testing.TB, store *Store) int64 {
	tb.Helper()
	var changes int64
	require.NoError(tb, store.writerDB.QueryRow(`SELECT total_changes()`).Scan(&changes))
	return changes
}

func TestRefFactRefreshUnchangedWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-op.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	seedRefFactWriteFixture(t, store, 100, 1000)
	// This is a disposable test store, never the user's daemon database.
	_, err = store.writerDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	before := refFactTotalChanges(t, store)
	for i := 0; i < 5; i++ {
		require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go", "", "repo/missing.go", "repo/changed.go"}))
	}
	require.Equal(t, int64(0), refFactTotalChanges(t, store)-before, "unchanged facts must not be deleted, inserted, or updated")
	wal, err := os.Stat(path + "-wal")
	require.NoError(t, err)
	require.Zero(t, wal.Size(), "unchanged refreshes must not append WAL frames")
}

func TestRefFactRefreshOnlyWritesChangedFacts(t *testing.T) {
	store := openRefFactRebuildStore(t)
	seedRefFactWriteFixture(t, store, 20, 20)
	_, err := store.writerDB.Exec(`UPDATE nodes SET name = 'Renamed' WHERE id = 'repo::Target0' AND view_gen = 0`)
	require.NoError(t, err)
	before := refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	require.Equal(t, int64(1), refFactTotalChanges(t, store)-before, "target-only renames update exactly one fact")
	facts, err := store.LoadRefFactsByFiles("repo", []string{"repo/changed.go"})
	require.NoError(t, err)
	require.Len(t, facts, 20)
	require.Equal(t, "Renamed", factsByKey(facts)["repo::Caller0->repo::Target0:calls"].RefName)

	_, err = store.writerDB.Exec(`UPDATE edges SET origin = 'lsp_resolved' WHERE from_id = 'repo::Caller0' AND view_gen = 0`)
	require.NoError(t, err)
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	require.Equal(t, int64(1), refFactTotalChanges(t, store)-before)
	facts, err = store.LoadRefFactsByFiles("repo", []string{"repo/changed.go"})
	require.NoError(t, err)
	fact := factsByKey(facts)["repo::Caller0->repo::Target0:calls"]
	require.Equal(t, "lsp_resolved", fact.Origin)
	require.Equal(t, "lsp", fact.Tier)

	_, err = store.writerDB.Exec(`DELETE FROM edges WHERE from_id = 'repo::Caller0' AND view_gen = 0`)
	require.NoError(t, err)
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	require.Equal(t, int64(1), refFactTotalChanges(t, store)-before, "only the obsolete fact is deleted")

	_, err = store.writerDB.Exec(`DELETE FROM nodes WHERE file_path = 'repo/changed.go' AND view_gen = 0`)
	require.NoError(t, err)
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	require.Equal(t, int64(19), refFactTotalChanges(t, store)-before)
	facts, err = store.LoadRefFactsByFiles("repo", []string{"repo/changed.go"})
	require.NoError(t, err)
	require.Empty(t, facts, "a removed/empty file must not retain facts")
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	require.Equal(t, before, refFactTotalChanges(t, store))
}

func TestRefFactRefreshPreservesOtherGenerations(t *testing.T) {
	store := openRefFactRebuildStore(t)
	seedRefFactWriteFixture(t, store, 2, 0)
	_, err := store.writerDB.Exec(`INSERT INTO ref_facts (` + refFactColumns + `)
SELECT 17, repo_prefix, from_id, to_id, kind, 'Foreign', line, origin, tier, candidates, file_path, lang FROM ref_facts WHERE view_gen = 0`)
	require.NoError(t, err)
	_, err = store.writerDB.Exec(`DELETE FROM edges WHERE view_gen = 0`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	var count int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM ref_facts WHERE view_gen = 17 AND ref_name = 'Foreign'`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestRefFactRefreshDuplicateEdgeIdentityConverges(t *testing.T) {
	store := openRefFactRebuildStore(t)
	seedRefFactWriteFixture(t, store, 1, 0)
	// Edge identity includes file_path; fact identity deliberately does not.
	// Preserve the legacy adjacency traversal's last-row winner for collisions.
	store.AddBatch(nil, []*graph.Edge{{From: "repo::Caller0", To: "repo::Target0", Kind: graph.EdgeCalls, FilePath: "repo/alternate.go", Line: 1, Confidence: 0.6}})
	require.NoError(t, store.RebuildRefFactsForRepos([]string{"repo"}))
	want, err := store.LoadRefFactsByFiles("repo", nil)
	require.NoError(t, err)
	require.Len(t, want, 1)
	require.Equal(t, "ast_inferred", want[0].Origin)
	before := refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	got, err := store.LoadRefFactsByFiles("repo", nil)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, before, refFactTotalChanges(t, store), "colliding projected identities must converge without alternating payloads")
}

func TestRefFactRefreshSourceAndTargetGenerationIsolation(t *testing.T) {
	store := openRefFactRebuildStore(t)
	for _, fixture := range []struct {
		generation int64
		targetName string
		language   string
	}{{0, "CanonicalTarget", "go"}, {17, "OverlayTarget", "typescript"}} {
		layer := store.AtGeneration(fixture.generation)
		source := refFactTestNode("repo::Caller", "Caller", "repo/changed.go", "repo", graph.KindFunction)
		source.Language = fixture.language
		layer.AddBatch([]*graph.Node{source, refFactTestNode("repo::Target", fixture.targetName, "repo/targets.go", "repo", graph.KindFunction)}, []*graph.Edge{{From: "repo::Caller", To: "repo::Target", Kind: graph.EdgeCalls, Line: 7, Confidence: 1}})
	}
	for _, generation := range []int64{0, 17} {
		require.NoError(t, store.AtGeneration(generation).ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	}
	for _, want := range []struct {
		generation int64
		targetName string
		language   string
	}{{0, "CanonicalTarget", "go"}, {17, "OverlayTarget", "typescript"}} {
		var count int
		var targetName, language string
		require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM ref_facts WHERE view_gen = ?`, want.generation).Scan(&count))
		require.Equal(t, 1, count, "same node IDs in another generation must not multiply facts")
		require.NoError(t, store.db.QueryRow(`SELECT ref_name, lang FROM ref_facts WHERE view_gen = ?`, want.generation).Scan(&targetName, &language))
		require.Equal(t, want.targetName, targetName, "target join must use the edge generation")
		require.Equal(t, want.language, language, "source join must use the edge generation")
	}
	before := refFactTotalChanges(t, store)
	for _, generation := range []int64{17, 0, 17} {
		require.NoError(t, store.AtGeneration(generation).ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
	}
	require.Equal(t, before, refFactTotalChanges(t, store), "unchanged refresh must not rewrite either generation")
}

func TestRefFactRefreshMovedSourceAndPayloadConverge(t *testing.T) {
	store := openRefFactRebuildStore(t)
	seedRefFactWriteFixture(t, store, 1, 0)
	files := []string{"repo/changed.go"}
	_, err := store.writerDB.Exec(`UPDATE ref_facts SET candidates = 'stale-candidate' WHERE view_gen = 0 AND repo_prefix = 'repo'`)
	require.NoError(t, err)
	before := refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", files))
	require.Equal(t, int64(1), refFactTotalChanges(t, store)-before, "candidate-only mismatch must update one fact")
	var candidates string
	require.NoError(t, store.db.QueryRow(`SELECT candidates FROM ref_facts WHERE view_gen = 0 AND repo_prefix = 'repo'`).Scan(&candidates))
	require.Empty(t, candidates)

	_, err = store.writerDB.Exec(`UPDATE nodes SET language = 'typescript' WHERE view_gen = 0 AND id = 'repo::Caller0'`)
	require.NoError(t, err)
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", files))
	require.Equal(t, int64(1), refFactTotalChanges(t, store)-before, "language-only mismatch must update one fact")
	var language string
	require.NoError(t, store.db.QueryRow(`SELECT lang FROM ref_facts WHERE view_gen = 0 AND repo_prefix = 'repo'`).Scan(&language))
	require.Equal(t, "typescript", language)

	_, err = store.writerDB.Exec(`UPDATE nodes SET file_path = 'repo/moved.go' WHERE view_gen = 0 AND id = 'repo::Caller0'`)
	require.NoError(t, err)
	files = []string{"repo/changed.go", "repo/moved.go"}
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", files))
	require.Equal(t, int64(1), refFactTotalChanges(t, store)-before, "file-only mismatch must update one fact")
	var count int
	var file string
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM ref_facts WHERE view_gen = 0 AND repo_prefix = 'repo'`).Scan(&count))
	require.Equal(t, 1, count, "moving a source must remove its obsolete file frontier")
	require.NoError(t, store.db.QueryRow(`SELECT file_path, lang, candidates FROM ref_facts WHERE view_gen = 0 AND repo_prefix = 'repo'`).Scan(&file, &language, &candidates))
	require.Equal(t, "repo/moved.go", file)
	require.Equal(t, "typescript", language)
	require.Empty(t, candidates)
	before = refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/moved.go", "repo/changed.go", "repo/moved.go"}))
	require.Equal(t, before, refFactTotalChanges(t, store), "reordered duplicate frontiers must converge")
}

func TestRefFactRefreshRemovesNewlyIneligibleFacts(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		target   string
		kind     string
		eligible bool
	}{
		{"missing_named_target", "repo::Missing", "calls", true},
		{"empty_target", "", "calls", false},
		{"unresolved", "unresolved::Missing", "calls", false},
		{"prefixed_unresolved", "repo::unresolved::Missing", "calls", false},
		{"stdlib", "stdlib::Print", "calls", false},
		{"prefixed_stdlib", "repo::stdlib::Print", "calls", false},
		{"external", "external_call::Print", "calls", false},
		{"builtin", "builtin::Print", "calls", false},
		{"module", "module::Print", "calls", false},
		{"case_sensitive_id", "Unresolved::StillNamed", "calls", true},
		{"different_reference_kind", "repo::Target0", "references", true},
		{"non_reference_kind", "repo::Target0", "contains", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store := openRefFactRebuildStore(t)
			seedRefFactWriteFixture(t, store, 1, 0)
			_, err := store.writerDB.Exec(`UPDATE edges SET to_id = ?, kind = ? WHERE view_gen = 0 AND from_id = 'repo::Caller0'`, fixture.target, fixture.kind)
			require.NoError(t, err)
			require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
			var count int
			require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM ref_facts WHERE view_gen = 0 AND repo_prefix = 'repo'`).Scan(&count))
			want := 0
			if fixture.eligible {
				want = 1
			}
			require.Equal(t, want, count)
			if fixture.eligible {
				var target, kind string
				require.NoError(t, store.db.QueryRow(`SELECT to_id, kind FROM ref_facts WHERE view_gen = 0 AND repo_prefix = 'repo'`).Scan(&target, &kind))
				require.Equal(t, fixture.target, target)
				require.Equal(t, fixture.kind, kind)
			}
			before := refFactTotalChanges(t, store)
			require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
			require.Equal(t, before, refFactTotalChanges(t, store), "the eligibility state must settle without further writes")
		})
	}
}

func TestRefFactRefreshWithoutOptionalNodeFileIndex(t *testing.T) {
	store := openRefFactRebuildStore(t)
	seedRefFactWriteFixture(t, store, 10, 10)
	_, err := store.writerDB.Exec(`DROP INDEX nodes_by_file`)
	require.NoError(t, err)
	before := refFactTotalChanges(t, store)
	require.NoError(t, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}), "bulk loading may temporarily remove the optional node-file index")
	require.Equal(t, before, refFactTotalChanges(t, store))
}

func TestRefFactRefreshEmbeddedQueryPlanUsesIndexedFrontier(t *testing.T) {
	store := openRefFactRebuildStore(t)
	seedRefFactWriteFixture(t, store, 256, 1024)
	var facts int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM ref_facts WHERE view_gen = ?`, store.viewGen).Scan(&facts))
	require.Equal(t, 256, facts, "requested-file projection must not admit unrelated source files")
	filesJSON := `["repo/changed.go"]`
	for _, fixture := range []struct {
		name string
		sql  string
		args []any
	}{
		{"obsolete", refFactDeleteObsolete, []any{filesJSON, "repo", store.viewGen, store.viewGen, store.viewGen, "repo", filesJSON}},
		{"changed", refFactUpsertChanged, []any{filesJSON, "repo", store.viewGen, store.viewGen}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			rows, err := store.db.Query("EXPLAIN QUERY PLAN "+fixture.sql, fixture.args...)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			plan := fmt.Sprint(details)
			t.Logf("embedded SQLite %s plan: %s", fixture.name, plan)
			require.Contains(t, plan, "nodes_by_file", "file-frontier refresh must not scan all repository source nodes")
			require.Contains(t, plan, "edges_by_from (view_gen=? AND from_id=?", "edge lookup must bind generation and each selected source ID")
			if fixture.name == "obsolete" {
				require.Regexp(t, `\bSEARCH d\b`, plan, "each old fact must probe the indexed desired-key set")
				require.NotRegexp(t, `\bSCAN d\b`, plan, "a correlated full desired-set scan makes no-op refresh quadratic")
			}
		})
	}
}

func BenchmarkRefFactRefreshWrites(b *testing.B) {
	for _, changed := range []bool{false, true} {
		b.Run(fmt.Sprintf("changed=%t", changed), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "facts.sqlite")
			store, err := Open(path)
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			seedRefFactWriteFixture(b, store, 1000, 10000)
			_, err = store.writerDB.Exec(`PRAGMA wal_autocheckpoint=0`)
			require.NoError(b, err)
			_, err = store.writerDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
			require.NoError(b, err)
			var rows int64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if changed {
					_, err = store.writerDB.Exec(`UPDATE nodes SET name = ? WHERE id = 'repo::Target0' AND view_gen = 0`, fmt.Sprintf("Renamed%d", i))
					require.NoError(b, err)
				}
				before := refFactTotalChanges(b, store)
				require.NoError(b, store.ReplaceRefFactsForFiles("repo", []string{"repo/changed.go"}))
				rows += refFactTotalChanges(b, store) - before
			}
			b.StopTimer()
			b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
			wal, err := os.Stat(path + "-wal")
			require.NoError(b, err)
			b.ReportMetric(float64(wal.Size())/float64(b.N), "wal-B/op")
		})
	}
}
