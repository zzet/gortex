package store_sqlite

import (
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// fileLocationFields keeps the columns FileLocationsInScopeSeq promises.
func fileLocationFields(n *graph.Node) *graph.Node {
	return &graph.Node{
		ID: n.ID, Kind: n.Kind, FilePath: n.FilePath, Language: n.Language,
		RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID,
	}
}

func fileLocationRows(seq iter.Seq[*graph.Node], project bool) []*graph.Node {
	var out []*graph.Node
	for n := range seq {
		if project {
			n = fileLocationFields(n)
		}
		out = append(out, n)
	}
	return out
}

// writeFileLocationFixture writes perRepo file rows into each of the
// repositories a, b and c, and unowned (empty prefix), at generation g. IDs
// descend while paths ascend, so nodes_repo_files' (repository, workspace,
// language, path) order never matches ID order; several IDs share one path;
// and non-file kinds sit at the same paths. Shared IDs carry a
// generation-specific language.
func writeFileLocationFixture(t *testing.T, s *Store, g int64, perRepo int) {
	t.Helper()
	var nodes []*graph.Node
	for i := 0; i < perRepo; i++ {
		for ordinal, repo := range []string{"", "a", "b", "c"} {
			path := fmt.Sprintf("dir-%d/file-%04d.go", i%5, i)
			if i%7 == 0 {
				path = "shared.go"
			}
			if repo != "" {
				path = repo + "/" + path
			}
			workspace := ""
			if i%3 != 0 {
				workspace = "ws-" + strings.Repeat(repo, i%2+1)
			}
			id := fmt.Sprintf("file-%04d-%d", perRepo-i, ordinal)
			nodes = append(nodes,
				&graph.Node{
					ID: id, Kind: graph.KindFile, Name: "file", QualName: id + "/qualified",
					FilePath: path, StartLine: 1, EndLine: 2, Language: fmt.Sprintf("lang-%d-%d", g, i%3),
					RepoPrefix: repo, WorkspaceID: workspace, ProjectID: "project",
					Meta: map[string]any{"doc": "payload a location read never touches"},
				},
				&graph.Node{ID: id + "::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: path, RepoPrefix: repo, WorkspaceID: workspace},
			)
		}
	}
	nodes = append(nodes,
		&graph.Node{ID: "a/empty-path", Kind: graph.KindFile, RepoPrefix: "a", Language: "text"},
		&graph.Node{ID: `a\win\file.go`, Kind: graph.KindFile, FilePath: `a\win\file.go`, RepoPrefix: "a", Language: "go"},
		&graph.Node{ID: "Zeta-upper", Kind: graph.KindFile, FilePath: "a/Zeta.go", RepoPrefix: "a"},
		&graph.Node{ID: "é-unicode", Kind: graph.KindFile, FilePath: "b/é.go", RepoPrefix: "b"},
		&graph.Node{ID: fmt.Sprintf("only-generation-%d", g), Kind: graph.KindFile, FilePath: "b/only.go", RepoPrefix: "b"},
		&graph.Node{ID: "a/contract", Kind: graph.KindContract, FilePath: "a/shared.go", RepoPrefix: "a"},
	)
	require.NoError(t, s.AtGeneration(g).AddBatchChecked(nodes, nil))
}

func TestFileLocationsMatchSummarySequence(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	perRepo := scopedProjectionPage + 37
	for _, g := range []int64{0, 31} {
		writeFileLocationFixture(t, s, g, perRepo)
	}
	many := []string{"a", "", "b"}
	for i := 0; i < scopedBaseFileRepoUnionArms; i++ {
		many = append(many, fmt.Sprintf("absent-%02d", i))
	}
	requests := map[string][]string{
		"duplicates":     {"b", "", "a", "b", ""},
		"one repository": {"a"},
		"unowned only":   {""},
		"two":            {"a", "b"},
		"beyond arms":    many,
		"absent":         {"absent"},
		"none":           nil,
	}
	for _, g := range []int64{0, 31} {
		handle := s.AtGeneration(g)
		for name, repos := range requests {
			t.Run(fmt.Sprintf("generation %d/%s", g, name), func(t *testing.T) {
				want := fileLocationRows(handle.NodesLightByKindsInScopeSeq(repos, nil, graph.KindFile), true)
				got := fileLocationRows(handle.FileLocationsInScopeSeq(repos), false)
				require.Equal(t, want, got, "the same rows, ID order and location columns as the summary read, and nothing more")
				for _, row := range got {
					require.Contains(t, repos, row.RepoPrefix)
					if strings.HasPrefix(row.Language, "lang-") {
						require.True(t, strings.HasPrefix(row.Language, fmt.Sprintf("lang-%d-", g)), "row %s leaked from another generation", row.ID)
					}
					require.NotEqual(t, fmt.Sprintf("only-generation-%d", 31-g), row.ID)
				}
				if len(repos) > 0 && repos[0] == "b" {
					require.Greater(t, len(got), 3*scopedProjectionPage, "the census spans several summary pages")
				}
			})
		}
		t.Run(fmt.Sprintf("generation %d/stops early", g), func(t *testing.T) {
			repos := []string{"", "a", "b"}
			want := fileLocationRows(handle.NodesLightByKindsInScopeSeq(repos, nil, graph.KindFile), true)
			var got []*graph.Node
			handle.FileLocationsInScopeSeq(repos)(func(n *graph.Node) bool {
				got = append(got, n)
				return len(got) < 3
			})
			require.Equal(t, want[:3], got)
		})
	}
}

// observeReadStatements records the statements the read pool runs during read.
func observeReadStatements(t *testing.T, read func()) []string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	readStatementObserver = func(q string) { mu.Lock(); seen = append(seen, q); mu.Unlock() }
	defer func() { readStatementObserver = nil }()
	read()
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), seen...)
}

func TestFileLocationsOneStatement(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	for _, g := range []int64{0, 31} {
		writeFileLocationFixture(t, s, g, scopedProjectionPage+1)
	}
	repos := []string{"", "a", "b"}
	for _, g := range []int64{0, 31} {
		handle := s.AtGeneration(g)
		var rows int
		statements := observeReadStatements(t, func() {
			for range handle.FileLocationsInScopeSeq(repos) {
				rows++
			}
		})
		require.Greater(t, rows, 3*scopedProjectionPage)
		require.Equal(t, []string{fileLocationProjectionSQL}, statements, "generation %d", g)
		summaryRows := 0
		summary := observeReadStatements(t, func() {
			for range handle.NodesLightByKindsInScopeSeq(repos, nil, graph.KindFile) {
				summaryRows++
			}
		})
		require.Equal(t, rows, summaryRows)
		require.Greater(t, len(summary), 1, "the paged summary read this census replaces")
	}
}

func TestFileLocationsCapFallback(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	writeFileLocationFixture(t, s, 0, 40)
	repos := []string{"", "a", "b"}
	want := fileLocationRows(s.NodesLightByKindsInScopeSeq(repos, nil, graph.KindFile), true)
	defer func(previous int) { fileLocationCensusCap = previous }(fileLocationCensusCap)
	for _, c := range []struct {
		cap      int
		fallback bool
	}{{len(want), false}, {len(want) - 1, true}, {1, true}} {
		fileLocationCensusCap = c.cap
		var got []*graph.Node
		statements := observeReadStatements(t, func() {
			got = fileLocationRows(s.FileLocationsInScopeSeq(repos), true)
		})
		require.Equal(t, want, got, "cap %d", c.cap)
		require.Equal(t, fileLocationProjectionSQL, statements[0])
		require.Equal(t, c.fallback, len(statements) > 1, "cap %d ran %d statements", c.cap, len(statements))
	}

	// A store without the covering index cannot run the statement at all
	// and serves the same census from the summary cursor.
	fileLocationCensusCap = len(want)
	_, err := s.writerDB.Exec(`DROP INDEX nodes_repo_files`)
	require.NoError(t, err)
	require.Equal(t, want, fileLocationRows(s.FileLocationsInScopeSeq(repos), true))
}

func TestFileLocationsPlanLock(t *testing.T) {
	s := openScopedProjectionTestStore(t)
	for _, g := range []int64{0, 31} {
		writeFileLocationFixture(t, s, g, 64)
	}
	for _, state := range keyListStatsStates(func(t *testing.T, s *Store) {
		_, err := s.writerDB.Exec(`ANALYZE`)
		require.NoError(t, err)
	}) {
		t.Run(state.name, func(t *testing.T) {
			state.apply(t, s)
			for _, g := range []int64{0, 31} {
				plan := scopedProjectionPlan(t, s, fileLocationProjectionSQL, `["","a","b"]`, g, fileLocationCensusCap+1)
				require.Contains(t, plan, "SEARCH n USING COVERING INDEX nodes_repo_files (repo_prefix=?)")
				for _, banned := range []string{"SCAN n", "PRIMARY KEY", "nodes_by_repo_kind", "TEMP B-TREE"} {
					require.NotContains(t, plan, banned, "generation %d plan:\n%s", g, plan)
				}
			}
		})
	}
}
