package store_sqlite

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteFindNodesByNamesInRepoLanguagesUsesCompoundIndexAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	store.AddBatch([]*graph.Node{
		{ID: "mono::go::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "go", FilePath: "mono/main.go"},
		{ID: "mono::python::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "python", FilePath: "mono/main.py"},
		{ID: "mono::neutral::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "", FilePath: "mono/generated"},
		{ID: "other::go::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "other", Language: "go", FilePath: "other/main.go"},
	}, nil)

	if _, err := store.writerDB.Exec(`DROP INDEX nodes_by_repo_language_name`); err != nil {
		t.Fatal(err)
	}
	before := explainRepoLanguageNamePlan(t, store)
	if strings.Contains(before, "nodes_by_repo_language_name") {
		t.Fatalf("compound index unexpectedly present before rebuild: %s", before)
	}
	rebuildStart := time.Now()
	if _, err := store.writerDB.Exec(`CREATE INDEX nodes_by_repo_language_name ON nodes(repo_prefix, language, name) WHERE name <> ''`); err != nil {
		t.Fatal(err)
	}
	t.Logf("minimal compound-index rebuild on fixture: %s", time.Since(rebuildStart))
	after := explainRepoLanguageNamePlan(t, store)
	t.Logf("query plan before: %s", before)
	t.Logf("query plan after: %s", after)
	if !strings.Contains(after, "nodes_by_repo_language_name") {
		t.Fatalf("repo+language+name query did not use compound index; before=%q after=%q", before, after)
	}

	assertNodeIDs(t,
		store.FindNodesByNamesInRepoLanguages([]string{"Shared"}, "mono", []string{"", "go"})["Shared"],
		[]string{"mono::go::Shared", "mono::neutral::Shared"})
	assertNodeIDs(t,
		store.FindNodesByNamesInRepoLanguages([]string{"Shared"}, "mono", nil)["Shared"],
		[]string{"mono::go::Shared", "mono::neutral::Shared", "mono::python::Shared"})

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertNodeIDs(t,
		store.FindNodesByNamesInRepoLanguages([]string{"Shared"}, "mono", []string{"", "go"})["Shared"],
		[]string{"mono::go::Shared", "mono::neutral::Shared"})
	if plan := explainRepoLanguageNamePlan(t, store); !strings.Contains(plan, "nodes_by_repo_language_name") {
		t.Fatalf("reopened store lost compound lookup plan: %s", plan)
	}
}

func TestSQLiteFindNodesByResolverNameScopesSetParityAndStableOrder(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "mono::go::A", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "go", FilePath: "mono/a.go"},
		{ID: "mono::go::B", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "go", FilePath: "mono/b.go"},
		{ID: "mono::neutral::A", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "", FilePath: "mono/generated"},
		{ID: "mono::python::A", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "python", FilePath: "mono/a.py"},
		{ID: "other::go::A", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "other", Language: "go", FilePath: "other/a.go"},
		{ID: "other::neutral::A", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "other", Language: "", FilePath: "other/generated"},
	}, nil)

	scopes := []graph.ResolverNameScope{
		{RepoPrefix: "mono", Languages: []string{"", "go"}, Names: []string{"Shared", "Missing"}},
		{RepoPrefix: "mono", Names: []string{"Shared"}},
		{AllRepos: true, Languages: []string{"", "go"}, Names: []string{"Shared"}},
		{AllRepos: true, Names: []string{"Shared"}},
	}
	results, err := store.FindNodesByResolverNameScopes(scopes)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(scopes) {
		t.Fatalf("result scopes = %d, want %d", len(results), len(scopes))
	}
	assertOrderedNodeIDs(t, results[0]["Shared"], []string{"mono::neutral::A", "mono::go::A", "mono::go::B"})
	assertOrderedNodeIDs(t, results[1]["Shared"], []string{"mono::go::A", "mono::go::B", "mono::neutral::A", "mono::python::A"})
	assertOrderedNodeIDs(t, results[2]["Shared"], []string{"mono::neutral::A", "mono::go::A", "mono::go::B", "other::neutral::A", "other::go::A"})
	assertOrderedNodeIDs(t, results[3]["Shared"], []string{"mono::go::A", "mono::go::B", "mono::neutral::A", "mono::python::A", "other::go::A", "other::neutral::A"})
	if _, exists := results[0]["Missing"]; exists {
		t.Fatal("store-level set lookup should not invent negative entries")
	}

	for i := 0; i < 2; i++ {
		legacy := store.FindNodesByNamesInRepoLanguages(scopes[i].Names, scopes[i].RepoPrefix, scopes[i].Languages)
		assertNodeIDSetEqual(t, results[i]["Shared"], legacy["Shared"])
	}
}

func TestSQLiteFindNodesByResolverNameScopesExceedsBindLimit(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{ID: "mono::Needle", Kind: graph.KindFunction, Name: "Needle", RepoPrefix: "mono", Language: "go", FilePath: "mono/needle.go"})

	names := make([]string, 40_001)
	for i := 0; i < len(names)-1; i++ {
		names[i] = fmt.Sprintf("Missing%05d", i)
	}
	names[len(names)-1] = "Needle"
	results, err := store.FindNodesByResolverNameScopes([]graph.ResolverNameScope{{
		RepoPrefix: "mono",
		Languages:  []string{"", "go"},
		Names:      names,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("result scopes = %d, want 1", len(results))
	}
	assertOrderedNodeIDs(t, results[0]["Needle"], []string{"mono::Needle"})
}

func TestSQLiteFindNodesByResolverNameScopesPlanUsesExistingIndexes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	payload, err := json.Marshal([]resolverNameScopePayload{
		{ScopeID: 0, RepoPrefix: "mono", Languages: []string{"", "go"}, Names: []string{"Shared"}},
		{ScopeID: 1, AllRepos: true, Languages: []string{"", "go"}, Names: []string{"Shared"}},
		{ScopeID: 2, RepoPrefix: "mono", Names: []string{"Shared"}},
		{ScopeID: 3, AllRepos: true, Names: []string{"Shared"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.writerDB.Query("EXPLAIN QUERY PLAN "+resolverNameScopeQuery,
		resolverNameScopeArgs(payload, store.viewGen)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, " | ")
	t.Logf("resolver scope query plan: %s", plan)
	if !strings.Contains(plan, "nodes_by_repo_language_name") || !strings.Contains(plan, "nodes_by_name") {
		t.Fatalf("resolver scope query missed required indexes: %s", plan)
	}
	if got := strings.Count(plan, "SEARCH n USING INDEX"); got != 4 {
		t.Fatalf("resolver scope query has %d indexed node seeks, want one per UNION arm: %s", got, plan)
	}
	if strings.Contains(plan, "SCAN n USING INDEX") {
		t.Fatalf("resolver scope query scans a node index: %s", plan)
	}
}

func TestSQLiteFindNodesByResolverNameScopesReturnsQueryAndScanErrors(t *testing.T) {
	t.Run("closed database", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FindNodesByResolverNameScopes([]graph.ResolverNameScope{{AllRepos: true, Names: []string{"Shared"}}}); err == nil {
			t.Fatal("closed database lookup returned an authoritative result")
		}
	})

	t.Run("invalid metadata", func(t *testing.T) {
		store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		store.AddNode(&graph.Node{ID: "mono::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "mono", Language: "go", FilePath: "mono/shared.go"})
		if _, err := store.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id = ?`, []byte("{"), "mono::Shared"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FindNodesByResolverNameScopes([]graph.ResolverNameScope{{RepoPrefix: "mono", Languages: []string{"go"}, Names: []string{"Shared"}}}); err == nil {
			t.Fatal("metadata scan failure returned an authoritative result")
		}
	})
}

func explainRepoLanguageNamePlan(t *testing.T, store *Store) string {
	t.Helper()
	rows, err := store.writerDB.Query(`EXPLAIN QUERY PLAN SELECT `+lookupNodeCols+` FROM nodes
WHERE repo_prefix = ? AND language IN (?, ?) AND name IN (?) AND name <> ''`, "mono", "", "go", "Shared")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, " | ")
}

func assertNodeIDs(t *testing.T, nodes []*graph.Node, want []string) {
	t.Helper()
	got := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			got = append(got, node.ID)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("node ids = %v, want %v", got, want)
	}
}

func assertOrderedNodeIDs(t *testing.T, nodes []*graph.Node, want []string) {
	t.Helper()
	got := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			got = append(got, node.ID)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ordered node ids = %v, want %v", got, want)
	}
}

func assertNodeIDSetEqual(t *testing.T, gotNodes, wantNodes []*graph.Node) {
	t.Helper()
	got := make([]string, 0, len(gotNodes))
	for _, node := range gotNodes {
		if node != nil {
			got = append(got, node.ID)
		}
	}
	want := make([]string, 0, len(wantNodes))
	for _, node := range wantNodes {
		if node != nil {
			want = append(want, node.ID)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("node id sets differ: got %v want %v", got, want)
	}
}
