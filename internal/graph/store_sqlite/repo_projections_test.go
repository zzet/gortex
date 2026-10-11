package store_sqlite

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteRepoProjectionsUseFlatColumnsAcrossRepoSets(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	largeMeta := strings.Repeat("metadata-that-must-stay-in-sqlite", 256)
	store.AddBatch([]*graph.Node{
		{ID: "a-file", Kind: graph.KindFile, RepoPrefix: "a", FilePath: "a.go", Language: "go", Meta: map[string]any{"payload": largeMeta}},
		{ID: "a-fn", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a.go", Language: "go", Meta: map[string]any{"payload": largeMeta}},
		{ID: "a-type", Kind: graph.KindType, RepoPrefix: "a", FilePath: "a.go", Language: "go", Meta: map[string]any{"payload": largeMeta}},
		{ID: "a-content", Kind: graph.KindDoc, RepoPrefix: "a", FilePath: "guide.md", Language: "markdown", Meta: map[string]any{"data_class": "content", "payload": largeMeta}},
		{ID: "b-type", Kind: graph.KindType, RepoPrefix: "b", FilePath: "b.py", Language: "python", Meta: map[string]any{"payload": largeMeta}},
		{ID: "other-type", Kind: graph.KindType, RepoPrefix: "other", FilePath: "other.rs", Language: "rust", Meta: map[string]any{"payload": largeMeta}},
		{ID: "empty-iface", Kind: graph.KindInterface, FilePath: "single.go", Language: "go", Meta: map[string]any{"payload": largeMeta}},
	}, nil)

	// Invalidating the opaque blob after its promoted columns were written makes
	// any accidental Node/Meta scan fail while the projection queries remain
	// fully answerable from flat columns.
	if _, err := store.writerDB.Exec(`UPDATE nodes SET meta = x'80'`); err != nil {
		t.Fatal(err)
	}

	wantFiles := []graph.RepoLanguageFileCount{
		{RepoPrefix: "a", FilePath: "a.go", Language: "go", Count: 3},
		{RepoPrefix: "b", FilePath: "b.py", Language: "python", Count: 1},
	}
	if got := store.RepoLanguageFileCounts([]string{"b", "a", "a"}); !reflect.DeepEqual(got, wantFiles) {
		t.Fatalf("file counts = %#v, want %#v", got, wantFiles)
	}

	wantCounts := map[string]map[string]int{
		"a": {"go": 3},
		"b": {"python": 1},
	}
	if got := store.RepoLanguageCounts([]string{"a", "b"}); !reflect.DeepEqual(got, wantCounts) {
		t.Fatalf("language counts = %#v, want %#v", got, wantCounts)
	}

	if got, want := store.RepoNodeIDsByKinds(
		[]string{"b", "a"},
		[]graph.NodeKind{graph.KindInterface, graph.KindType},
	), []string{"a-type", "b-type"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kind IDs = %#v, want %#v", got, want)
	}
	if got, want := store.RepoNodeIDsByKinds(
		[]string{""},
		[]graph.NodeKind{graph.KindInterface},
	), []string{"empty-iface"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty-prefix kind IDs = %#v, want %#v", got, want)
	}
}

func TestRepoNodeIDsByKindsKeepsRepositorySeekAcrossHistogramStates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "repo_kind.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.BeginBulkLoad()
	if s.bulkConn == nil {
		t.Fatal("fixture did not enter the cold bulk window")
	}
	if err := s.AddBatchChecked([]*graph.Node{
		{ID: "owned", Kind: graph.KindFunction, RepoPrefix: "repo"},
		{ID: "unowned", Kind: graph.KindFunction},
		{ID: "unrelated", Kind: graph.KindFunction, RepoPrefix: "other"},
		{ID: "wrong-kind", Kind: graph.KindType, RepoPrefix: "repo"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	selected := s.AtGeneration(3)
	if err := selected.AddBatchChecked([]*graph.Node{
		{ID: "owned", Kind: graph.KindType, RepoPrefix: "repo"},
		{ID: "selected", Kind: graph.KindFunction, RepoPrefix: "repo"},
		{ID: "selected-unowned", Kind: graph.KindFunction},
	}, nil); err != nil {
		t.Fatal(err)
	}
	wantHistogram := 0
	check := func(t *testing.T) {
		t.Helper()
		var histogram int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'nodes_stats_histogram'`).Scan(&histogram); err != nil {
			t.Fatal(err)
		}
		if histogram != wantHistogram {
			t.Fatalf("histogram indexes = %d, want %d", histogram, wantHistogram)
		}
		for _, view := range []*Store{s, selected} {
			plan := strings.Join(explainQueryPlanArgs(t, s, repoNodeIDsByKindsQuery(),
				`["repo",""]`, `["function"]`, view.viewGen), "\n")
			if !strings.Contains(plan, "nodes_by_repo_kind (repo_prefix=? AND kind=?)") ||
				strings.Contains(plan, "SCAN n") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("repository projection lost its repository/kind seek:\n%s", plan)
			}
			want := []string{"owned", "unowned"}
			if view.viewGen != 0 {
				want = []string{"selected", "selected-unowned"}
			}
			if got := view.RepoNodeIDsByKinds([]string{"repo", ""}, []graph.NodeKind{graph.KindFunction}); !reflect.DeepEqual(got, want) {
				t.Fatalf("generation %d IDs = %v, want %v", view.viewGen, got, want)
			}
		}
	}
	t.Run("cold_bulk_without_histogram", check)
	if err := s.FlushBulk(); err != nil {
		t.Fatal(err)
	}
	wantHistogram = 1
	// Clear both statistics tables: a fresh index may have no statistics yet.
	if _, err := s.writerDB.Exec(`ANALYZE; DELETE FROM sqlite_stat1; DELETE FROM sqlite_stat4; ANALYZE sqlite_schema`); err != nil {
		t.Fatal(err)
	}
	recycleStatsReadPool(s.db, s.writerDB)
	t.Run("histogram_without_statistics", check)
	if _, err := s.writerDB.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	recycleStatsReadPool(s.db, s.writerDB)
	t.Run("histogram_with_statistics", check)
}

func TestSQLiteRepoEdgeProjectionAndKindEvictionAreSetOriented(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{ID: "a-src", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "b-src", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "other-src", Kind: graph.KindFunction, RepoPrefix: "other"},
		{ID: "target", Kind: graph.KindField, RepoPrefix: "target"},
	}, []*graph.Edge{
		{From: "a-src", To: "target", Kind: graph.EdgeReads, FilePath: "a.go", Line: 1},
		{From: "b-src", To: "target", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2},
		{From: "b-src", To: "target", Kind: graph.EdgeWrites, FilePath: "b.go", Line: 3},
		{From: "other-src", To: "target", Kind: graph.EdgeReads, FilePath: "other.go", Line: 4},
	})

	rows := store.RepoEdgesByKinds(
		[]string{"b", "a", "a"},
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReads},
	)
	if len(rows) != 2 {
		t.Fatalf("projected rows = %d, want 2: %#v", len(rows), rows)
	}
	if rows[0].RepoPrefix != "a" || rows[0].Edge.From != "a-src" || rows[0].Edge.Kind != graph.EdgeReads {
		t.Fatalf("first projected row = %#v", rows[0])
	}
	if rows[1].RepoPrefix != "b" || rows[1].Edge.From != "b-src" || rows[1].Edge.Kind != graph.EdgeCalls {
		t.Fatalf("second projected row = %#v", rows[1])
	}

	if got := store.EvictEdgesByKinds([]graph.EdgeKind{graph.EdgeReads, graph.EdgeCalls, graph.EdgeReads}); got != 3 {
		t.Fatalf("evicted edges = %d, want 3", got)
	}
	if got := store.EdgeCount(); got != 1 {
		t.Fatalf("remaining edge count = %d, want 1", got)
	}
	remaining := store.GetOutEdges("b-src")
	if len(remaining) != 1 || remaining[0].Kind != graph.EdgeWrites {
		t.Fatalf("remaining edges = %#v, want only writes", remaining)
	}
}
