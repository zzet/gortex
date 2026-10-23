package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// floodedScopeStore seeds two repos: "noise" holds noiseCount symbols
// whose token bag is exactly the query token (they win BM25 outright),
// "app" holds one WidgetExtensions. This is the cross-repo flood shape:
// every unscoped head slot goes to noise rows, so any post-fetch scope
// filter starves no matter how far the caller over-fetches.
func floodedScopeStore(t *testing.T, noiseCount int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "scope.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	appID := "app/widget.go::WidgetExtensions"
	s.AddNode(&graph.Node{
		ID: appID, Kind: graph.KindType, Name: "WidgetExtensions",
		FilePath: "app/widget.go", Language: "go", RepoPrefix: "app",
	})
	if err := s.UpsertSymbolFTS(appID, "widget extensions"); err != nil {
		t.Fatalf("UpsertSymbolFTS(app): %v", err)
	}

	for i := 0; i < noiseCount; i++ {
		id := fmt.Sprintf("noise/f%d.go::Extensions", i)
		s.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: "Extensions",
			FilePath: fmt.Sprintf("noise/f%d.go", i), Language: "go", RepoPrefix: "noise",
		})
		if err := s.UpsertSymbolFTS(id, "extensions"); err != nil {
			t.Fatalf("UpsertSymbolFTS(noise %d): %v", i, err)
		}
	}
	return s
}

// TestSearchSymbolsRepoScoped_CrossRepoFloodDoesNotStarve: with 250
// out-of-scope rows outranking the one in-scope symbol, the scoped
// search must return that symbol — filtering inside the FTS query,
// not over a bounded head.
func TestSearchSymbolsRepoScoped_CrossRepoFloodDoesNotStarve(t *testing.T) {
	s := floodedScopeStore(t, 250)

	hits, err := s.SearchSymbolsRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "app/widget.go::WidgetExtensions" {
		t.Fatalf("scoped search = %+v, want exactly the app symbol", hits)
	}
}

// TestSearchSymbolsRepoScoped_ExactNameTierRespectsScope: the exact-name
// tier-0 short-circuit must not leak out-of-scope nodes; when every
// exact-name hit is out of scope it falls through to the scoped FTS.
func TestSearchSymbolsRepoScoped_ExactNameTierRespectsScope(t *testing.T) {
	s := floodedScopeStore(t, 30)

	// "Extensions" is the exact name of every noise node and no app node.
	hits, err := s.SearchSymbolsRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	for _, h := range hits {
		if strings.HasPrefix(h.NodeID, "noise/") {
			t.Fatalf("exact-name tier leaked out-of-scope hit %q", h.NodeID)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("scoped search = %+v, want the single app symbol", hits)
	}
}

// TestSearchSymbolsRepoScoped_AdmitsUnownedNodes: rows with an empty
// repo_prefix (synthetic externals) pass every repo-narrow predicate
// in the codebase (see QueryOptions.ScopeAllows) — the FTS-level
// narrow must agree or scoped searches silently lose externals.
func TestSearchSymbolsRepoScoped_AdmitsUnownedNodes(t *testing.T) {
	s := floodedScopeStore(t, 3)

	extID := "dep::widgets/WidgetExtensions"
	s.AddNode(&graph.Node{
		ID: extID, Kind: graph.KindType, Name: "WidgetExtensions", Language: "go",
	})
	if err := s.UpsertSymbolFTS(extID, "widget extensions external"); err != nil {
		t.Fatalf("UpsertSymbolFTS(ext): %v", err)
	}

	hits, err := s.SearchSymbolsRepoScoped("widget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.NodeID] = true
	}
	if !found["app/widget.go::WidgetExtensions"] || !found[extID] {
		t.Fatalf("scoped search = %+v, want the app symbol AND the unowned external", hits)
	}
}

// TestSearchSymbols_ExactNameModuleDoesNotEclipseFTS: an exact-name
// tier-0 hit that is only a container node (module / package — e.g.
// a .NET project named "Extensions") must not short-circuit the FTS
// tier where the real classes rank. Observed live: two csproj module
// nodes named "Extensions" eclipsed 63 `*Extensions` types.
func TestSearchSymbols_ExactNameModuleDoesNotEclipseFTS(t *testing.T) {
	s := floodedScopeStore(t, 3)

	for i, id := range []string{"module::x:app/Core/Extensions", "app/pkgdecoy::Extensions"} {
		kind := graph.KindModule
		if i == 1 {
			kind = graph.KindPackage
		}
		s.AddNode(&graph.Node{ID: id, Kind: kind, Name: "Extensions", Language: "go", RepoPrefix: "app"})
		if err := s.UpsertSymbolFTS(id, "extensions"); err != nil {
			t.Fatalf("UpsertSymbolFTS(%s): %v", id, err)
		}
	}

	hits, err := s.SearchSymbolsRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.NodeID] = true
	}
	if !found["app/widget.go::WidgetExtensions"] {
		t.Fatalf("container-kind exact-name hits eclipsed the FTS tier; hits=%+v", hits)
	}
	// The containers are MERGED, not dropped — a kind:module caller
	// post-filtering these hits must still find its module.
	if !found["module::x:app/Core/Extensions"] {
		t.Fatalf("exact-name container hits must ride along for kind-filtered callers; hits=%+v", hits)
	}
}

// TestSearchSymbols_ExactTypeWinsOverSiblings pins the intended
// tier-0 contract: a symbol-kind node named exactly like the query
// short-circuits even when same-suffix siblings exist — "I typed the
// name, give me the name". Sibling discovery is the prefix query's job.
func TestSearchSymbols_ExactTypeWinsOverSiblings(t *testing.T) {
	s := floodedScopeStore(t, 3)
	exact := "app/exact.go::Extensions"
	s.AddNode(&graph.Node{ID: exact, Kind: graph.KindType, Name: "Extensions", Language: "go", RepoPrefix: "app"})
	if err := s.UpsertSymbolFTS(exact, "extensions"); err != nil {
		t.Fatalf("UpsertSymbolFTS: %v", err)
	}

	hits, err := s.SearchSymbolsRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != exact || hits[0].Score != 100.0 {
		t.Fatalf("exact symbol-kind hit must keep the short-circuit; hits=%+v", hits)
	}
}

// TestSearchSymbols_ExactNameTypeStillShortCircuits: a genuine
// exact-name symbol hit keeps the tier-0 fast path.
func TestSearchSymbols_ExactNameTypeStillShortCircuits(t *testing.T) {
	s := floodedScopeStore(t, 3)

	hits, err := s.SearchSymbolsRepoScoped("WidgetExtensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "app/widget.go::WidgetExtensions" || hits[0].Score != 100.0 {
		t.Fatalf("exact-name type must keep the tier-0 fast path; hits=%+v", hits)
	}
}

// TestSearchSymbolsRepoScoped_EmptyScopeMatchesUnscoped: a nil allow
// list means no repo narrowing — identical behaviour to SearchSymbols.
func TestSearchSymbolsRepoScoped_EmptyScopeMatchesUnscoped(t *testing.T) {
	s := floodedScopeStore(t, 5)

	scoped, err := s.SearchSymbolsRepoScoped("Extensions", nil, 50)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped(nil): %v", err)
	}
	unscoped, err := s.SearchSymbols("Extensions", 50)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	if len(scoped) != len(unscoped) {
		t.Fatalf("nil-scope hits = %d, unscoped = %d — must match", len(scoped), len(unscoped))
	}
}

// TestSearchSymbolsRepoScoped_MultipleRepos: the IN-list arity above
// one — both allowed repos' rows return, the third stays out.
func TestSearchSymbolsRepoScoped_MultipleRepos(t *testing.T) {
	s := floodedScopeStore(t, 3)
	otherID := "beta/w.go::BetaWidgetExtensions"
	s.AddNode(&graph.Node{ID: otherID, Kind: graph.KindType, Name: "BetaWidgetExtensions", Language: "go", RepoPrefix: "beta"})
	if err := s.UpsertSymbolFTS(otherID, "beta widget extensions"); err != nil {
		t.Fatalf("UpsertSymbolFTS: %v", err)
	}

	hits, err := s.SearchSymbolsRepoScoped("widget", []string{"app", "beta"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.NodeID] = true
		if strings.HasPrefix(h.NodeID, "noise/") {
			t.Fatalf("multi-repo scope leaked %q", h.NodeID)
		}
	}
	if !found["app/widget.go::WidgetExtensions"] || !found[otherID] {
		t.Fatalf("both allowed repos must return; hits=%+v", hits)
	}
}

// TestSearchSymbolsRepoScoped_ExcludesOrphanDocs: a scoped search must
// drop orphan FTS docs — documents whose node row no longer exists
// (eviction leaked the doc) — while keeping deliberate unowned globals
// (dep:: externals, empty prefix WITH a live node row). The two are
// indistinguishable by prefix alone; the node-row existence is the
// discriminator. Unscoped search keeps both: orphans are excluded from
// scopes only, cleaning them up is a maintenance concern, not a search
// semantics change.
func TestSearchSymbolsRepoScoped_ExcludesOrphanDocs(t *testing.T) {
	s := floodedScopeStore(t, 3)

	// Deliberate unowned global: node row EXISTS, prefix empty.
	extID := "dep::widgets/WidgetExtensions"
	s.AddNode(&graph.Node{ID: extID, Kind: graph.KindType, Name: "WidgetExtensions", Language: "go"})
	if err := s.UpsertSymbolFTS(extID, "widget extensions external"); err != nil {
		t.Fatalf("UpsertSymbolFTS(ext): %v", err)
	}

	// Orphan: FTS doc without any node row (the eviction-leak shape).
	if err := s.UpsertSymbolFTS("orphan::ghost::Ghost", "ghost widget extensions"); err != nil {
		t.Fatalf("UpsertSymbolFTS(orphan): %v", err)
	}

	hits, err := s.SearchSymbolsRepoScoped("widget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolsRepoScoped: %v", err)
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.NodeID] = true
	}
	if !found["app/widget.go::WidgetExtensions"] {
		t.Fatalf("scoped search lost the in-scope symbol: %+v", hits)
	}
	if !found[extID] {
		t.Fatalf("scoped search lost the deliberate unowned external: %+v", hits)
	}
	if found["orphan::ghost::Ghost"] {
		t.Fatalf("scoped search leaked the orphan doc: %+v", hits)
	}

	// The bundle variant composes the same scoped hit query.
	bundles, err := s.SearchSymbolBundlesRepoScoped("widget", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolBundlesRepoScoped: %v", err)
	}
	for _, b := range bundles {
		if b.Node != nil && b.Node.ID == "orphan::ghost::Ghost" {
			t.Fatalf("scoped bundles leaked the orphan doc: %+v", bundles)
		}
	}

	// Unscoped search is unchanged: the orphan stays visible.
	unscoped, err := s.SearchSymbols("widget", 50)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	unscopedFound := false
	for _, h := range unscoped {
		if h.NodeID == "orphan::ghost::Ghost" {
			unscopedFound = true
		}
	}
	if !unscopedFound {
		t.Fatalf("unscoped search must keep the orphan (scope-only exclusion); hits=%+v", unscoped)
	}
}

// TestSearchSymbolBundlesRepoScoped_FiltersInQuery: the bundle variant
// inherits the scoped hit query and returns only in-scope bundles.
func TestSearchSymbolBundlesRepoScoped_FiltersInQuery(t *testing.T) {
	s := floodedScopeStore(t, 250)

	bundles, err := s.SearchSymbolBundlesRepoScoped("Extensions", []string{"app"}, 20)
	if err != nil {
		t.Fatalf("SearchSymbolBundlesRepoScoped: %v", err)
	}
	if len(bundles) != 1 || bundles[0].Node == nil || bundles[0].Node.ID != "app/widget.go::WidgetExtensions" {
		t.Fatalf("scoped bundles = %+v, want exactly the app symbol", bundles)
	}
}
