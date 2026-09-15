package resolver

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type repoLanguageLookupCountingStore struct {
	graph.Store

	mu                    sync.Mutex
	resolverScopeCalls    int
	resolverScopeCount    int
	repoLanguageCalls     int
	globalNameBatchCalls  int
	pointNameCalls        int
	pointRepoNameCalls    int
	allNodesCalls         int
	allEdgesCalls         int
	returnedWrongLanguage bool
}

func (s *repoLanguageLookupCountingStore) FindNodesByResolverNameScopes(scopes []graph.ResolverNameScope) ([]map[string][]*graph.Node, error) {
	hits, err := s.Store.(graph.ResolverNameScopeFinder).FindNodesByResolverNameScopes(scopes)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolverScopeCalls++
	s.resolverScopeCount += len(scopes)
	for i, scope := range scopes {
		allowed := make(map[string]struct{}, len(scope.Languages))
		for _, language := range scope.Languages {
			allowed[language] = struct{}{}
		}
		if len(allowed) == 0 || i >= len(hits) {
			continue
		}
		for _, nodes := range hits[i] {
			for _, node := range nodes {
				if node != nil {
					if _, ok := allowed[node.Language]; !ok {
						s.returnedWrongLanguage = true
					}
				}
			}
		}
	}
	return hits, err
}

func (s *repoLanguageLookupCountingStore) FindNodesByNamesInRepoLanguages(names []string, repo string, languages []string) map[string][]*graph.Node {
	finder := s.Store.(graph.RepoLanguageNameFinder)
	hits := finder.FindNodesByNamesInRepoLanguages(names, repo, languages)
	allowed := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		allowed[language] = struct{}{}
	}
	s.mu.Lock()
	s.repoLanguageCalls++
	if len(allowed) > 0 {
		for _, nodes := range hits {
			for _, node := range nodes {
				if node == nil {
					continue
				}
				if _, ok := allowed[node.Language]; !ok {
					s.returnedWrongLanguage = true
				}
			}
		}
	}
	s.mu.Unlock()
	return hits
}

func (s *repoLanguageLookupCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.mu.Lock()
	s.globalNameBatchCalls++
	s.mu.Unlock()
	return s.Store.FindNodesByNames(names)
}

func (s *repoLanguageLookupCountingStore) FindNodesByName(name string) []*graph.Node {
	s.mu.Lock()
	s.pointNameCalls++
	s.mu.Unlock()
	return s.Store.FindNodesByName(name)
}

func (s *repoLanguageLookupCountingStore) FindNodesByNameInRepo(name, repo string) []*graph.Node {
	s.mu.Lock()
	s.pointRepoNameCalls++
	s.mu.Unlock()
	return s.Store.FindNodesByNameInRepo(name, repo)
}

func (s *repoLanguageLookupCountingStore) AllNodes() []*graph.Node {
	s.mu.Lock()
	s.allNodesCalls++
	s.mu.Unlock()
	return s.Store.AllNodes()
}

func (s *repoLanguageLookupCountingStore) AllEdges() []*graph.Edge {
	s.mu.Lock()
	s.allEdgesCalls++
	s.mu.Unlock()
	return s.Store.AllEdges()
}

func TestWarmLookupCachePushesRepoAndCompatibleLanguageWithoutNPlusOne(t *testing.T) {
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })

	base.AddBatch([]*graph.Node{
		{ID: "mono::caller::go", Kind: graph.KindFunction, Name: "goCaller", RepoPrefix: "mono", Language: "go", FilePath: "mono/main.go"},
		{ID: "mono::caller::web", Kind: graph.KindFunction, Name: "webCaller", RepoPrefix: "mono", Language: "javascript", FilePath: "mono/main.js"},
		{ID: "mono::caller::unknown", Kind: graph.KindFunction, Name: "unknownCaller", RepoPrefix: "mono", Language: "", FilePath: "mono/generated"},
		{ID: "mono::caller::vue", Kind: graph.KindFunction, Name: "vueCaller", RepoPrefix: "mono", Language: "vue", FilePath: "mono/App.vue"},
		{ID: "mono::caller::svelte", Kind: graph.KindFunction, Name: "svelteCaller", RepoPrefix: "mono", Language: "svelte", FilePath: "mono/App.svelte"},
		{ID: "mono::caller::astro", Kind: graph.KindFunction, Name: "astroCaller", RepoPrefix: "mono", Language: "astro", FilePath: "mono/App.astro"},
		{ID: "mono::caller::html", Kind: graph.KindFunction, Name: "htmlCaller", RepoPrefix: "mono", Language: "html", FilePath: "mono/index.html"},
		{ID: "mono::python::PyOnly", Kind: graph.KindFunction, Name: "PyOnly", RepoPrefix: "mono", Language: "python", FilePath: "mono/lib.py"},
		{ID: "mono::typescript::WebFn", Kind: graph.KindFunction, Name: "WebFn", RepoPrefix: "mono", Language: "typescript", FilePath: "mono/lib.ts"},
		{ID: "mono::typescript::VueWebFn", Kind: graph.KindFunction, Name: "VueWebFn", RepoPrefix: "mono", Language: "typescript", FilePath: "mono/vue.ts"},
		{ID: "mono::javascript::SvelteWebFn", Kind: graph.KindFunction, Name: "SvelteWebFn", RepoPrefix: "mono", Language: "javascript", FilePath: "mono/svelte.js"},
		{ID: "mono::typescript::AstroWebFn", Kind: graph.KindFunction, Name: "AstroWebFn", RepoPrefix: "mono", Language: "typescript", FilePath: "mono/astro.ts"},
		{ID: "mono::javascript::HTMLWebFn", Kind: graph.KindFunction, Name: "HTMLWebFn", RepoPrefix: "mono", Language: "javascript", FilePath: "mono/html.js"},
		{ID: "mono::neutral::NeutralFn", Kind: graph.KindFunction, Name: "NeutralFn", RepoPrefix: "mono", Language: "", FilePath: "mono/generated.def"},
	}, nil)

	store := &repoLanguageLookupCountingStore{Store: base}
	resolver := New(store)
	edges := []*graph.Edge{
		{From: "mono::caller::go", To: "unresolved::PyOnly", Kind: graph.EdgeCalls, FilePath: "mono/main.go"},
		{From: "mono::caller::web", To: "unresolved::WebFn", Kind: graph.EdgeCalls, FilePath: "mono/main.js"},
		{From: "mono::caller::unknown", To: "unresolved::PyOnly", Kind: graph.EdgeCalls, FilePath: "mono/generated"},
		{From: "mono::caller::go", To: "unresolved::NeutralFn", Kind: graph.EdgeCalls, FilePath: "mono/main.go"},
		{From: "mono::caller::vue", To: "unresolved::VueWebFn", Kind: graph.EdgeCalls, FilePath: "mono/App.vue"},
		{From: "mono::caller::svelte", To: "unresolved::SvelteWebFn", Kind: graph.EdgeCalls, FilePath: "mono/App.svelte"},
		{From: "mono::caller::astro", To: "unresolved::AstroWebFn", Kind: graph.EdgeCalls, FilePath: "mono/App.astro"},
		{From: "mono::caller::html", To: "unresolved::HTMLWebFn", Kind: graph.EdgeCalls, FilePath: "mono/index.html"},
	}
	for i := 0; i < 128; i++ {
		edges = append(edges, &graph.Edge{
			From:     "mono::caller::go",
			To:       "unresolved::" + fmt.Sprintf("Missing%d", i),
			Kind:     graph.EdgeCalls,
			FilePath: "mono/main.go",
		})
	}

	if err := resolver.warmLookupCache(edges); err != nil {
		t.Fatal(err)
	}
	if got := resolver.cachedFindNodesByNameInRepoForEdge("PyOnly", "mono", edges[0]); len(got) != 0 {
		t.Fatalf("Go scope examined Python same-name candidate: %+v", got)
	}

	stats := &ResolveStats{}
	resolver.resolveFunctionCall(edges[0], "PyOnly", stats)
	if edges[0].To != "unresolved::PyOnly" {
		t.Fatalf("Go call bound across language families: %s", edges[0].To)
	}
	resolver.resolveFunctionCall(edges[1], "WebFn", stats)
	if edges[1].To != "mono::typescript::WebFn" {
		t.Fatalf("JS/TS family bridge did not bind: %s", edges[1].To)
	}
	resolver.resolveFunctionCall(edges[2], "PyOnly", stats)
	if edges[2].To != "mono::python::PyOnly" {
		t.Fatalf("unknown-language conservative fallback did not bind: %s", edges[2].To)
	}
	resolver.resolveFunctionCall(edges[3], "NeutralFn", stats)
	if edges[3].To != "mono::neutral::NeutralFn" {
		t.Fatalf("language-empty candidate was excluded: %s", edges[3].To)
	}
	for i, want := range []string{
		"mono::typescript::VueWebFn",
		"mono::javascript::SvelteWebFn",
		"mono::typescript::AstroWebFn",
		"mono::javascript::HTMLWebFn",
	} {
		edge := edges[4+i]
		resolver.resolveFunctionCall(edge, graph.UnresolvedName(edge.To), stats)
		if edge.To != want {
			t.Fatalf("unclassified template language %q did not retain repo-scoped cross-language resolution: got %s want %s", edge.FilePath, edge.To, want)
		}
	}
	for _, edge := range edges[8:] {
		resolver.resolveFunctionCall(edge, graph.UnresolvedName(edge.To), stats)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.resolverScopeCalls != 1 || store.resolverScopeCount != 4 {
		t.Fatalf("resolver scope calls/scopes = %d/%d, want one call covering 4 repo×family groups for %d edges", store.resolverScopeCalls, store.resolverScopeCount, len(edges))
	}
	if store.repoLanguageCalls != 0 {
		t.Fatalf("legacy repo-language queries = %d, want 0", store.repoLanguageCalls)
	}
	if store.globalNameBatchCalls != 0 {
		t.Fatalf("global FindNodesByNames calls = %d, want 0", store.globalNameBatchCalls)
	}
	if store.pointNameCalls != 0 || store.pointRepoNameCalls != 0 {
		t.Fatalf("point name lookups = global:%d repo:%d, want 0", store.pointNameCalls, store.pointRepoNameCalls)
	}
	if store.allNodesCalls != 0 || store.allEdgesCalls != 0 {
		t.Fatalf("bulk graph scans = AllNodes:%d AllEdges:%d, want 0", store.allNodesCalls, store.allEdgesCalls)
	}
	if store.returnedWrongLanguage {
		t.Fatal("SQLite returned a candidate outside the requested language set")
	}
}

func TestWarmLookupCacheKeepsExternLanguageFamiliesIsolated(t *testing.T) {
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })

	base.AddBatch([]*graph.Node{
		{ID: "app::go::caller", Kind: graph.KindFunction, Name: "goCaller", RepoPrefix: "app", Language: "go", FilePath: "app/main.go"},
		{ID: "app::python::caller", Kind: graph.KindFunction, Name: "pythonCaller", RepoPrefix: "app", Language: "python", FilePath: "app/main.py"},
		{ID: "dep-go::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "dep-go", Language: "go", FilePath: "depgo/pkg/shared.go"},
		{ID: "dep-python::Shared", Kind: graph.KindFunction, Name: "Shared", RepoPrefix: "dep-python", Language: "python", FilePath: "acme/pkg/shared.py"},
		{ID: "dep-neutral::Neutral", Kind: graph.KindFunction, Name: "Neutral", RepoPrefix: "dep-neutral", Language: "", FilePath: "neutral/pkg/neutral.def"},
	}, nil)

	store := &repoLanguageLookupCountingStore{Store: base}
	resolver := New(store)
	goEdge := &graph.Edge{From: "app::go::caller", To: "unresolved::extern::example.com/depgo/pkg::Shared", Kind: graph.EdgeCalls, FilePath: "app/main.go"}
	pythonEdge := &graph.Edge{From: "app::python::caller", To: "unresolved::extern::py.acme/acme/pkg::Shared", Kind: graph.EdgeCalls, FilePath: "app/main.py"}
	neutralEdge := &graph.Edge{From: "app::go::caller", To: "unresolved::extern::example.com/neutral/pkg::Neutral", Kind: graph.EdgeCalls, FilePath: "app/main.go"}
	if err := resolver.warmLookupCache([]*graph.Edge{goEdge, pythonEdge, neutralEdge}); err != nil {
		t.Fatal(err)
	}

	goCandidates, err := resolver.cachedFindExternNodesByName("Shared", goEdge)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range goCandidates {
		if candidate.Language == "python" {
			t.Fatalf("Go extern cache leaked Python candidate: %+v", candidate)
		}
	}
	pythonCandidates, err := resolver.cachedFindExternNodesByName("Shared", pythonEdge)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range pythonCandidates {
		if candidate.Language == "go" {
			t.Fatalf("Python extern cache leaked Go candidate: %+v", candidate)
		}
	}
	neutralCandidates, err := resolver.cachedFindExternNodesByName("Neutral", neutralEdge)
	if err != nil {
		t.Fatal(err)
	}
	if len(neutralCandidates) != 1 || neutralCandidates[0].Language != "" {
		t.Fatalf("Go extern cache excluded neutral candidate: %+v", neutralCandidates)
	}

	stats := &ResolveStats{}
	resolver.resolveExtern(goEdge, "example.com/depgo/pkg::Shared", stats)
	resolver.resolveExtern(pythonEdge, "py.acme/acme/pkg::Shared", stats)
	resolver.resolveExtern(neutralEdge, "example.com/neutral/pkg::Neutral", stats)
	if goEdge.To != "dep-go::Shared" || !goEdge.CrossRepo {
		t.Fatalf("Go extern resolution = %s cross_repo=%v", goEdge.To, goEdge.CrossRepo)
	}
	if pythonEdge.To != "dep-python::Shared" || !pythonEdge.CrossRepo {
		t.Fatalf("Python extern resolution = %s cross_repo=%v", pythonEdge.To, pythonEdge.CrossRepo)
	}
	if neutralEdge.To != "dep-neutral::Neutral" || !neutralEdge.CrossRepo {
		t.Fatalf("neutral extern resolution = %s cross_repo=%v", neutralEdge.To, neutralEdge.CrossRepo)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.resolverScopeCalls != 1 || store.resolverScopeCount != 2 {
		t.Fatalf("extern resolver scope calls/scopes = %d/%d, want one call for Go and Python families", store.resolverScopeCalls, store.resolverScopeCount)
	}
	if store.repoLanguageCalls != 0 || store.globalNameBatchCalls != 0 || store.pointNameCalls != 0 || store.pointRepoNameCalls != 0 {
		t.Fatalf("extern lookup fell through to legacy calls: repo_batch=%d global_batch=%d point=%d repo_point=%d", store.repoLanguageCalls, store.globalNameBatchCalls, store.pointNameCalls, store.pointRepoNameCalls)
	}
}

type resolverNameScopeErrorStore struct {
	graph.Store
	err   error
	calls int
}

func (s *resolverNameScopeErrorStore) FindNodesByResolverNameScopes([]graph.ResolverNameScope) ([]map[string][]*graph.Node, error) {
	s.calls++
	return nil, s.err
}

func TestExternLookupErrorIsNotAuthoritativeNegative(t *testing.T) {
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	base.AddNode(&graph.Node{ID: "app::caller", Kind: graph.KindFunction, Name: "caller", RepoPrefix: "app", Language: "go", FilePath: "app/main.go"})

	store := &resolverNameScopeErrorStore{Store: base, err: errors.New("injected resolver lookup failure")}
	resolver := New(store)
	edge := &graph.Edge{From: "app::caller", To: "unresolved::extern::example.com/dep/pkg::Shared", Kind: graph.EdgeCalls, FilePath: "app/main.go"}
	if err := resolver.warmLookupCache([]*graph.Edge{edge}); err != nil {
		t.Fatal(err)
	}
	if resolver.nodesByExternLanguageName != nil {
		t.Fatal("failed extern warm installed an authoritative cache")
	}
	stats := &ResolveStats{}
	resolver.resolveExtern(edge, "example.com/dep/pkg::Shared", stats)
	if edge.To != "unresolved::extern::example.com/dep/pkg::Shared" {
		t.Fatalf("failed lookup classified extern edge as %s", edge.To)
	}
	if stats.Unresolved != 1 || stats.External != 0 || store.calls != 2 {
		t.Fatalf("failed lookup stats/calls = unresolved:%d external:%d calls:%d", stats.Unresolved, stats.External, store.calls)
	}
}
