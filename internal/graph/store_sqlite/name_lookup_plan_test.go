package store_sqlite

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// withLiveStoreNodeStats installs the node statistics of the live store
// (sampled with an analysis limit: about 1,000 rows per repository and 167
// per repository and language, where the store holds 1.75 million), under
// which the IN-list name reads scanned every generation's index entries.
func withLiveStoreNodeStats(t *testing.T, s *Store) {
	t.Helper()
	stats := [][2]string{
		{"nodes_by_file", "1466778 69 69"},
		{"nodes_by_generation", "1466778 1001 1"},
		{"nodes_by_kind", "1466778 1001 201"},
		{"nodes_by_name", "1466778 3 2"},
		{"nodes_by_repo", "1466778 1001 1001"},
		{"nodes_by_repo_language_name", "1466776 1001 167 3 2"},
	}
	_, err := s.writerDB.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
	for _, st := range stats {
		_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE tbl = 'nodes' AND idx = ?`, st[0])
		require.NoError(t, err)
		_, err = s.writerDB.Exec(`INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES ('nodes', ?, ?)`, st[0], st[1])
		require.NoError(t, err)
	}
	_, err = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
}

func planOf(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.writerDB.Query(`EXPLAIN QUERY PLAN `+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		out = append(out, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(out, " | ")
}

// Under the live store's statistics, with three languages and 195 names (the
// burst edit that took 268 s), the name reads seek the full key per pair and
// never range over the repository's index entries of every generation; and
// they return what the IN-list form returns.
func TestRepoNameReadsSeekUnderTheLiveStoreStatistics(t *testing.T) {
	s := openPayloadStore(t)
	var nodes []*graph.Node
	var names []string
	for i := 0; i < 195; i++ {
		name := fmt.Sprintf("N%03d", i)
		names = append(names, name)
		lang := []string{"go", "typescript", "python"}[i%3]
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("repo/f%d.go::%s", i%20, name), Kind: graph.KindFunction, Name: name,
			FilePath: fmt.Sprintf("repo/f%d.go", i%20), RepoPrefix: "repo", Language: lang})
	}
	s.AddBatch(nodes, nil)
	withLiveStoreNodeStats(t, s)
	langs := []string{"go", "typescript", "javascript"}
	namesJSON, _ := json.Marshal(names)
	langsJSON, _ := json.Marshal(langs)

	// Precondition: under these statistics the IN-list form ranges over the
	// repository's entries of every generation.
	inList := `SELECT id FROM nodes WHERE repo_prefix = ? AND language IN (?, ?, ?) AND name IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + `) AND name <> '' AND view_gen = ?`
	inArgs := []any{"repo", langs[0], langs[1], langs[2]}
	for _, n := range names {
		inArgs = append(inArgs, n)
	}
	inArgs = append(inArgs, s.viewGen)
	require.Contains(t, planOf(t, s, inList, inArgs...), "(repo_prefix=? AND language=?)", "precondition: the statistics reproduce the flip")

	// The reads run the seek statements.
	var used []string
	nameLookupSQLObserver = func(q string) { used = append(used, q) }
	t.Cleanup(func() { nameLookupSQLObserver = nil })
	_ = s.FindNodesByNamesInRepoLanguages(names, "repo", langs)
	_ = s.FindNodesByNamesInRepo(names, "repo")
	require.Equal(t, []string{repoLanguageNamesSeekSQL, repoNamesSeekSQL}, used)

	plan := planOf(t, s, repoLanguageNamesSeekSQL, string(langsJSON), string(namesJSON), "repo", s.viewGen)
	t.Logf("repo+languages plan: %s", plan)
	require.Contains(t, plan, "nodes_by_repo_language_name (repo_prefix=? AND language=? AND name=?)")
	plan = planOf(t, s, repoNamesSeekSQL, string(namesJSON), s.viewGen, "repo")
	t.Logf("repo plan: %s", plan)
	require.Contains(t, plan, "(name=? AND view_gen=?)")
	require.NotContains(t, plan, "nodes_by_repo (")

	// Parity with the IN-list reads.
	got := s.FindNodesByNamesInRepoLanguages(names, "repo", langs)
	count := 0
	for name, ns := range got {
		for _, n := range ns {
			require.Equal(t, name, n.Name)
			require.Contains(t, []string{"go", "typescript"}, n.Language)
			count++
		}
	}
	require.Equal(t, 130, count, "go and typescript nodes of the 195")
	all := s.FindNodesByNamesInRepo(names, "repo")
	total := 0
	for _, ns := range all {
		total += len(ns)
	}
	require.Equal(t, 195, total)
	require.Empty(t, s.FindNodesByNamesInRepo(names, "other"))
}

// FileSymbolNamesByPaths seeks each path under the live store's statistics,
// never ranging over the generation's nodes of the kinds.
func TestFileSymbolNamesByPathsSeeksUnderTheLiveStoreStatistics(t *testing.T) {
	s := openPayloadStore(t)
	var nodes []*graph.Node
	var paths []string
	for i := 0; i < 60; i++ {
		file := fmt.Sprintf("repo/f%02d.go", i)
		paths = append(paths, file)
		nodes = append(nodes, &graph.Node{ID: file + "::F", Kind: graph.KindFunction, Name: "F", FilePath: file, RepoPrefix: "repo", Language: "go"})
	}
	s.AddBatch(nodes, nil)
	withLiveStoreNodeStats(t, s)
	var used []string
	nameLookupSQLObserver = func(q string) { used = append(used, q) }
	t.Cleanup(func() { nameLookupSQLObserver = nil })
	rows := s.FileSymbolNamesByPaths(paths, []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType})
	require.Len(t, rows, 60)
	require.Len(t, used, 1)
	pathsJSON, _ := json.Marshal(paths)
	plan := planOf(t, s, used[0], string(pathsJSON), s.viewGen, "function", "method", "type")
	t.Logf("plan: %s", plan)
	require.Contains(t, plan, "(file_path=? AND view_gen=?)")
	require.NotContains(t, plan, "nodes_by_kind")
}
