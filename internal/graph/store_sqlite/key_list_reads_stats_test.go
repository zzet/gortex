package store_sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The statistics a store is read under:
//   - the live store's (sampled on a store of 1.47 million nodes);
//   - none at all for the graph indexes (a store never analyzed);
//   - those of a store fresh from a load, before any refresh: the rows the
//     store's own refresh wrote while it was small, against a graph now
//     large.
type statsState struct {
	name  string
	apply func(t *testing.T, s *Store)
}

func keyListStatsStates(small func(t *testing.T, s *Store)) []statsState {
	reload := func(t *testing.T, s *Store) {
		t.Helper()
		_, err := s.writerDB.Exec(`ANALYZE sqlite_schema`)
		require.NoError(t, err)
		recycleStatsReadPool(s.db, s.writerDB)
	}
	return []statsState{
		{"live store statistics", func(t *testing.T, s *Store) {
			withLiveStoreNodeStats(t, s)
			reload(t, s)
		}},
		{"no statistics", func(t *testing.T, s *Store) {
			_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
			_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
			require.NoError(t, err)
			reload(t, s)
		}},
		{"fresh from a load, before any refresh", func(t *testing.T, s *Store) {
			small(t, s)
			reload(t, s)
		}},
		// A store fresh from a whole index into generation 0, starved: its
		// sqlite_stat1 held these four rows only (dumped before the first
		// edit of a no-pause burst on a 392,261-node fixture), none for
		// nodes_by_name, nodes_by_repo or nodes_by_repo_language_name, and
		// the store's own check read it fresh.
		{"starved after a whole index (four rows)", func(t *testing.T, s *Store) {
			applyStarvedStats(t, s)
			reload(t, s)
		}},
	}
}

// keyListGraph writes a graph of two repositories: files of 30 functions,
// names shared across files and repositories (as common names are), three
// languages, and calls between them. It returns what the reads take.
const keyListPerFile = 30

func keyListGraph(t *testing.T, s *Store, files int) (ids, names, paths []string) {
	t.Helper()
	const perFile = keyListPerFile
	for f := 0; f < files; f += 100 {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for g := f; g < min(f+100, files); g++ {
			repo := []string{"repo", "other"}[g%2]
			file := fmt.Sprintf("%s/pkg%d/f%d.go", repo, g%40, g)
			for i := 0; i < perFile; i++ {
				id := fmt.Sprintf("%s::S%d_%d", file, g, i)
				nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("N%d", (g*7+i)%2000),
					QualName: fmt.Sprintf("%s.S%d_%d", repo, g, i), FilePath: file, RepoPrefix: repo,
					Language: []string{"go", "python", "typescript"}[g%3], StartLine: i * 10, EndLine: i*10 + 5})
				edges = append(edges, &graph.Edge{From: id, To: fmt.Sprintf("%s::S%d_%d", file, g, (i+1)%perFile), Kind: graph.EdgeCalls, FilePath: file, Line: i*10 + 1})
			}
		}
		s.AddBatch(nodes, edges)
	}
	return keyListKeys(files, 0)
}

// keyListKeys is a set of 50 keys of the graph chosen by offset, so each
// statistics state reads keys no earlier read has cached.
func keyListKeys(files, offset int) (ids, names, paths []string) {
	const perFile = keyListPerFile
	for i := 0; i < 50; i++ {
		g := (i*37 + offset*11) % files
		repo := []string{"repo", "other"}[g%2]
		file := fmt.Sprintf("%s/pkg%d/f%d.go", repo, g%40, g)
		ids = append(ids, fmt.Sprintf("%s::S%d_%d", file, g, i%perFile))
		names = append(names, fmt.Sprintf("N%d", (i*41+offset*7)%2000))
		paths = append(paths, file)
	}
	return ids, names, paths
}

// The name lookups' plans do not depend on the statistics: under each of the
// three states they seek by name (the repository form) and by repository,
// language and name (the language form), never ranging over a repository's
// generation.
func TestNameLookupPlansUnderEveryStatisticsState(t *testing.T) {
	s := openPayloadStore(t)
	small := smallStoreStats(t, s)
	_, names, _ := keyListGraph(t, s, 2000)
	namesJSON, _ := json.Marshal(names)
	langsJSON, _ := json.Marshal([]string{"go", "python"})
	for _, state := range keyListStatsStates(small) {
		state.apply(t, s)
		repoPlan := planOf(t, s, repoNamesSeekSQL, string(namesJSON), s.viewGen, "repo")
		langPlan := planOf(t, s, repoLanguageNamesSeekSQL, string(langsJSON), string(namesJSON), "repo", s.viewGen)
		t.Logf("%s:\n repo_names: %s\n repo_language_names: %s", state.name, repoPlan, langPlan)
		require.Contains(t, repoPlan, "nodes_by_name (name=? AND view_gen=?)", state.name)
		require.Contains(t, langPlan, "nodes_by_repo_language_name (repo_prefix=? AND language=? AND name=?)", state.name)
	}
}

// smallStoreStats writes 4 files of the graph, runs the store's own refresh
// (the statistics a store carries from before a load), and returns the call
// that puts those rows back once the graph has grown.
func smallStoreStats(t *testing.T, s *Store) func(t *testing.T, s *Store) {
	t.Helper()
	_, _, _ = keyListGraph(t, s, 4)
	s.writeMu.Lock()
	require.NoError(t, s.refreshPlannerStatsLocked(context.Background()))
	s.writeMu.Unlock()
	rows, err := s.writerDB.Query(`SELECT tbl, idx, stat FROM sqlite_stat1`)
	require.NoError(t, err)
	var saved [][3]string
	for rows.Next() {
		var tbl, idx, stat string
		require.NoError(t, rows.Scan(&tbl, &idx, &stat))
		saved = append(saved, [3]string{tbl, idx, stat})
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	t.Logf("statistics of the small store: %v", saved)
	return func(t *testing.T, s *Store) {
		_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
		require.NoError(t, err)
		for _, r := range saved {
			_, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES (?, ?, ?)`, r[0], r[1], r[2])
			require.NoError(t, err)
		}
	}
}

// The reads that take a list of keys and a generation, under the three
// statistics states: every statement they run seeks by its keys.
func TestKeyListReadsSeekUnderEveryStatisticsState(t *testing.T) {
	s := openPayloadStore(t)
	small := smallStoreStats(t, s)
	_, _, _ = keyListGraph(t, s, 2000)
	type read struct {
		name string
		call func()
	}
	readsFor := func(ids, names, paths []string) []read {
		return []read{
			{"GetNodesByIDs", func() { require.NotEmpty(t, s.GetNodesByIDs(ids)) }},
			{"ExistingNodeIDs", func() { require.NotEmpty(t, s.ExistingNodeIDs(ids)) }},
			{"NodePlacementsByIDs", func() { require.NotEmpty(t, s.NodePlacementsByIDs(ids)) }},
			{"FindNodesByNames", func() { require.NotEmpty(t, s.FindNodesByNames(names)) }},
			{"FindNodesByNamesInRepo", func() { require.NotEmpty(t, s.FindNodesByNamesInRepo(names, "repo")) }},
			{"FindNodesByNamesInRepoLanguages", func() {
				require.NotEmpty(t, s.FindNodesByNamesInRepoLanguages(names, "repo", []string{"go", "python"}))
			}},
			{"GetFileNodesByPaths", func() { require.NotEmpty(t, s.GetFileNodesByPaths(paths)) }},
			{"FileSymbolNamesByPaths", func() {
				require.NotEmpty(t, s.FileSymbolNamesByPaths(paths, []graph.NodeKind{graph.KindFunction, graph.KindMethod}))
			}},
			{"GetOutEdgesByNodeIDs", func() { require.NotEmpty(t, s.GetOutEdgesByNodeIDs(ids)) }},
			{"GetInEdgeIdentitiesByNodeIDs", func() { require.NotEmpty(t, s.GetInEdgeIdentitiesByNodeIDs(ids)) }},
			{"InDegreeForNodes", func() { require.NotEmpty(t, s.InDegreeForNodes(ids)) }},
		}
	}
	// Each read's statements, as the read pool ran them, planned under the
	// state: no full scan of nodes or edges, and no range over a whole
	// repository's or generation's entries (the plans the name lookups took
	// without statistics).
	var mu sync.Mutex
	var seen []string
	readStatementObserver = func(q string) { mu.Lock(); seen = append(seen, q); mu.Unlock() }
	t.Cleanup(func() { readStatementObserver = nil })
	ranges := []string{
		"SCAN nodes", "SCAN n ", "SCAN edges", "SCAN e ",
		"nodes_by_repo (repo_prefix=? AND view_gen=?)", "nodes_by_repo (repo_prefix=?)",
		"nodes_by_generation (view_gen=?)", "edges_by_generation (view_gen=?)",
		"nodes_by_kind (kind=? AND view_gen=?)", "nodes_by_repo_kind (repo_prefix=?",
	}
	for si, state := range keyListStatsStates(small) {
		state.apply(t, s)
		for _, r := range readsFor(keyListKeys(2000, si+1)) {
			mu.Lock()
			seen = seen[:0]
			mu.Unlock()
			r.call()
			mu.Lock()
			queries := append([]string(nil), seen...)
			mu.Unlock()
			require.NotEmpty(t, queries, "%s ran no pool statement", r.name)
			for _, q := range queries {
				if !strings.HasPrefix(strings.TrimSpace(q), "SELECT") && !strings.HasPrefix(strings.TrimSpace(q), "WITH") {
					continue
				}
				plan := planOfUnbound(t, s, q)
				t.Logf("%-38s %-30s %s", state.name, r.name, plan)
				for _, bad := range ranges {
					require.NotContains(t, plan, bad, "%s under %s: %s", r.name, state.name, q)
				}
			}
		}
	}
}

// planOfUnbound plans a statement with its parameters unbound (NULL): the
// index choice does not depend on the values, only on the shape.
func planOfUnbound(t *testing.T, s *Store, query string) string {
	t.Helper()
	args := make([]any, sqlParamCount(query))
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

// sqlParamCount is the number of parameters a statement takes: the largest
// ?N, or the number of bare ? outside string literals.
func sqlParamCount(query string) int {
	maxN, bare := 0, 0
	inString := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '\'' {
			inString = !inString
			continue
		}
		if inString || c != '?' {
			continue
		}
		j := i + 1
		n := 0
		for j < len(query) && query[j] >= '0' && query[j] <= '9' {
			n = n*10 + int(query[j]-'0')
			j++
		}
		if j > i+1 {
			maxN = max(maxN, n)
		} else {
			bare++
		}
		i = j - 1
	}
	return max(maxN, bare)
}

// starvedStats are the sqlite_stat1 rows of a store fresh from a whole index,
// as dumped before its first edit (all of them).
var starvedStats = [][3]string{
	{"edges", "edges_by_from_line", "2286357 1001 14 3"},
	{"edges", "edges_by_kind", "2286357 501"},
	{"nodes", "nodes_by_kind", "392261 501 501"},
	{"nodes", "nodes_go_receiver_type", "4681 1001 17 1 1"},
}

func applyStarvedStats(t *testing.T, s *Store) {
	t.Helper()
	_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
	require.NoError(t, err)
	for _, r := range starvedStats {
		_, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES (?, ?, ?)`, r[0], r[1], r[2])
		require.NoError(t, err)
	}
}
