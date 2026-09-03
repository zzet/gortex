package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func mkFnNode(id, name, file string) *graph.Node {
	return &graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: file, Language: "go"}
}

// newTestBundleCache builds a cache with the default byte budget without
// consulting the environment, so the fingerprint / invalidation unit tests
// stay hermetic regardless of GORTEX_BUNDLE_CACHE_MAX_MB.
func newTestBundleCache() *bundleCache {
	return &bundleCache{
		fingerprints: map[string]uint64{},
		entries:      map[string]*bundleCacheEntry{},
		maxBytes:     bundleCacheDefaultMaxBytes,
		maxEntries:   bundleCacheMaxEntries,
	}
}

// --- unit tests over the cache logic in isolation ---

func TestBundleCache_ServesOnlyValidatedFingerprints(t *testing.T) {
	c := newTestBundleCache()

	b := graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")}

	// No fingerprint reported for the package yet -> store is a no-op
	// (conservative: never cache an unvalidated bundle).
	c.store(baseViewGeneration, b)
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("bundle was cached despite no package fingerprint")
	}

	// Report a fingerprint, then store: now it caches and serves.
	c.refresh(map[string]uint64{"pkg": 100})
	c.store(baseViewGeneration, b)
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); !ok {
		t.Fatal("bundle should be served once its package fingerprint is known")
	}
}

func TestBundleCache_InvalidatesOnFingerprintChange(t *testing.T) {
	c := newTestBundleCache()
	c.refresh(map[string]uint64{"pkg": 1})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})

	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); !ok {
		t.Fatal("expected a cache hit on the unchanged fingerprint")
	}

	// Fingerprint changes -> the entry is invalidated.
	c.refresh(map[string]uint64{"pkg": 2})
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("entry must be dropped when its package fingerprint changes")
	}
}

func TestBundleCache_CrossRepoIsolation(t *testing.T) {
	c := newTestBundleCache()
	// Two repos with the same inner directory name resolve to DIFFERENT
	// package keys because the stored file paths are repo-prefixed.
	c.refresh(map[string]uint64{
		"repoA/pkg": 10,
		"repoB/pkg": 20,
	})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("repoA/pkg/x.go::A", "A", "repoA/pkg/x.go")})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("repoB/pkg/x.go::A", "A", "repoB/pkg/x.go")})

	// Bumping only repoA's fingerprint must not touch repoB's entry.
	c.refresh(map[string]uint64{
		"repoA/pkg": 11,
		"repoB/pkg": 20,
	})
	if _, ok := c.lookup(baseViewGeneration, "repoA/pkg/x.go::A"); ok {
		t.Fatal("repoA entry should have been invalidated")
	}
	if _, ok := c.lookup(baseViewGeneration, "repoB/pkg/x.go::A"); !ok {
		t.Fatal("repoB entry must survive a repoA-only fingerprint bump")
	}
}

func TestBundlePackageKey(t *testing.T) {
	cases := map[string]string{
		"pkg/sub/x.go": "pkg/sub",
		"x.go":         "",
		"":             "",
		"repo/a/b.go":  "repo/a",
	}
	for in, want := range cases {
		if got := bundlePackageKey(in); got != want {
			t.Errorf("bundlePackageKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The key is looked up in a fingerprint map whose keys are forward-slash, so
// it must never carry an OS separator. Stated separately from the table above
// because the table only fails on a platform whose separator differs from "/",
// and this spells out why that matters rather than leaving it to be rediscovered.
func TestBundlePackageKeyNeverUsesOSSeparator(t *testing.T) {
	if filepath.Separator == '/' {
		t.Skip("separator matches the contract on this platform")
	}
	for _, in := range []string{"pkg/sub/x.go", "repoA/pkg/x.go", "a/b/c/d.go"} {
		if got := bundlePackageKey(in); strings.ContainsRune(got, filepath.Separator) {
			t.Errorf("bundlePackageKey(%q) = %q, must stay forward-slash", in, got)
		}
	}
}

// --- integration tests through the store's SearchSymbolBundles ---

func newBundleTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "b.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedBundleStore(t *testing.T, s *Store) {
	t.Helper()
	s.AddNode(mkFnNode("pkg/x.go::A", "AlphaWidget", "pkg/x.go"))
	s.AddNode(mkFnNode("pkg/x.go::B", "BetaWidget", "pkg/x.go"))
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::B", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})

	items := []graph.SymbolFTSItem{
		{NodeID: "pkg/x.go::A", Tokens: "alpha widget"},
		{NodeID: "pkg/x.go::B", Tokens: "beta widget"},
	}
	if err := s.BulkUpsertSymbolFTS("", items); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS: %v", err)
	}
	if err := s.BuildSymbolIndex(); err != nil {
		t.Fatalf("BuildSymbolIndex: %v", err)
	}
}

func bundleByID(bundles []graph.SymbolBundle) map[string]graph.SymbolBundle {
	out := make(map[string]graph.SymbolBundle, len(bundles))
	for _, b := range bundles {
		if b.Node != nil {
			out[b.Node.ID] = b
		}
	}
	return out
}

func TestSearchSymbolBundles_CacheHitOnUnchangedFingerprint(t *testing.T) {
	s := newBundleTestStore(t)
	seedBundleStore(t, s)

	// Report a fingerprint so the first query populates the cache.
	s.SetBundleFingerprints(map[string]uint64{"pkg": 1})

	first, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("first SearchSymbolBundles: %v", err)
	}
	got := bundleByID(first)
	if b, ok := got["pkg/x.go::A"]; !ok || len(b.OutEdges) != 1 {
		t.Fatalf("expected A with 1 out-edge on first query, got %+v", got["pkg/x.go::A"])
	}

	// Mutate the graph WITHOUT bumping the fingerprint: add a second
	// out-edge from A. A correct content-addressed cache serves the
	// STALE (1-edge) bundle because the fingerprint is unchanged — proof
	// the bundle came from cache, not a fresh fetch.
	s.AddNode(mkFnNode("pkg/x.go::C", "GammaWidget", "pkg/x.go"))
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::C", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})

	second, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("second SearchSymbolBundles: %v", err)
	}
	cached := bundleByID(second)["pkg/x.go::A"]
	if len(cached.OutEdges) != 1 {
		t.Fatalf("expected the cached 1-edge bundle to be served on an unchanged fingerprint, got %d edges",
			len(cached.OutEdges))
	}
}

func TestSearchSymbolBundles_MissAndRecomputeOnFingerprintChange(t *testing.T) {
	s := newBundleTestStore(t)
	seedBundleStore(t, s)
	s.SetBundleFingerprints(map[string]uint64{"pkg": 1})

	if _, err := s.SearchSymbolBundles("widget", 10); err != nil {
		t.Fatalf("warm-up query: %v", err)
	}

	// Add a real out-edge, then bump the package fingerprint to signal
	// the content changed. The next query must recompute and surface the
	// new edge.
	s.AddNode(mkFnNode("pkg/x.go::C", "GammaWidget", "pkg/x.go"))
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::C", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})
	s.SetBundleFingerprints(map[string]uint64{"pkg": 2})

	after, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("post-invalidation query: %v", err)
	}
	fresh := bundleByID(after)["pkg/x.go::A"]
	if len(fresh.OutEdges) != 2 {
		t.Fatalf("expected the recomputed 2-edge bundle after a fingerprint bump, got %d edges",
			len(fresh.OutEdges))
	}
}

func TestSearchSymbolBundles_UncachedWithoutFingerprints(t *testing.T) {
	s := newBundleTestStore(t)
	seedBundleStore(t, s)
	// No SetBundleFingerprints call -> the cache stays inert and every
	// query recomputes live. Adding an edge must show up immediately.
	first, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	if got := bundleByID(first)["pkg/x.go::A"]; len(got.OutEdges) != 1 {
		t.Fatalf("expected 1 edge live, got %d", len(got.OutEdges))
	}

	s.AddNode(mkFnNode("pkg/x.go::C", "GammaWidget", "pkg/x.go"))
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::C", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})

	second, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if got := bundleByID(second)["pkg/x.go::A"]; len(got.OutEdges) != 2 {
		t.Fatalf("uncached path must reflect the new edge live, got %d", len(got.OutEdges))
	}
}

// --- byte-budget tests ---

func TestBundleCache_ByteBudgetEvictionAtBoundary(t *testing.T) {
	c := newTestBundleCache()
	c.refresh(map[string]uint64{"pkg": 1})

	// Fixed-width ids so every entry estimates to the same size.
	mk := func(i int) graph.SymbolBundle {
		return graph.SymbolBundle{Node: mkFnNode(fmt.Sprintf("pkg/x.go::N%03d", i), "W", "pkg/x.go")}
	}
	unit := bundleEntryBytes(mk(0))
	const k = 4
	c.maxBytes = unit * k // budget holds exactly k entries

	for i := 0; i < k; i++ {
		c.store(baseViewGeneration, mk(i))
	}
	if len(c.entries) != k {
		t.Fatalf("expected %d entries filling the budget, got %d", k, len(c.entries))
	}
	if c.curBytes != unit*k {
		t.Fatalf("curBytes = %d, want %d", c.curBytes, unit*k)
	}

	// One more entry crosses the budget -> wholesale clear, only the newest
	// survives and the byte total resets to a single unit.
	c.store(baseViewGeneration, mk(k))
	if len(c.entries) != 1 {
		t.Fatalf("crossing the budget must clear wholesale to 1 entry, got %d", len(c.entries))
	}
	if c.curBytes != unit {
		t.Fatalf("curBytes after clear = %d, want %d", c.curBytes, unit)
	}
	if _, ok := c.lookup(baseViewGeneration, fmt.Sprintf("pkg/x.go::N%03d", k)); !ok {
		t.Fatal("the entry that triggered the clear must remain served")
	}
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::N000"); ok {
		t.Fatal("a pre-clear entry must be gone after the wholesale clear")
	}
}

func TestBundleCache_RefusesEntryLargerThanBudget(t *testing.T) {
	c := newTestBundleCache()
	c.refresh(map[string]uint64{"pkg": 1})
	b := graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")}
	c.maxBytes = bundleEntryBytes(b) - 1 // budget just below a single entry

	c.store(baseViewGeneration, b)
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("an entry larger than the whole budget must not be cached")
	}
	if len(c.entries) != 0 || c.curBytes != 0 {
		t.Fatalf("oversized store must leave the cache empty, got %d entries / %d bytes",
			len(c.entries), c.curBytes)
	}
}

func TestBundleCacheMaxBytes_EnvOverride(t *testing.T) {
	t.Setenv("GORTEX_BUNDLE_CACHE_MAX_MB", "128")
	if got := bundleCacheMaxBytes(); got != 128<<20 {
		t.Fatalf("env override = %d, want %d", got, 128<<20)
	}
	if c := newBundleCache(); c.maxBytes != 128<<20 {
		t.Fatalf("newBundleCache maxBytes = %d, want %d", c.maxBytes, 128<<20)
	}

	// Empty and unparseable values keep the default.
	t.Setenv("GORTEX_BUNDLE_CACHE_MAX_MB", "")
	if got := bundleCacheMaxBytes(); got != bundleCacheDefaultMaxBytes {
		t.Fatalf("empty override should keep the default, got %d", got)
	}
	t.Setenv("GORTEX_BUNDLE_CACHE_MAX_MB", "not-a-number")
	if got := bundleCacheMaxBytes(); got != bundleCacheDefaultMaxBytes {
		t.Fatalf("unparseable override should keep the default, got %d", got)
	}
}

func TestBundleCache_DisabledMode(t *testing.T) {
	t.Setenv("GORTEX_BUNDLE_CACHE_MAX_MB", "0")
	c := newBundleCache()
	if c.maxBytes != 0 {
		t.Fatalf("expected a disabled cache (maxBytes 0), got %d", c.maxBytes)
	}
	c.refresh(map[string]uint64{"pkg": 1})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})
	if len(c.entries) != 0 {
		t.Fatalf("a disabled cache must not store, got %d entries", len(c.entries))
	}
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("a disabled cache must always miss")
	}

	// A negative budget disables too.
	t.Setenv("GORTEX_BUNDLE_CACHE_MAX_MB", "-4")
	if got := bundleCacheMaxBytes(); got != 0 {
		t.Fatalf("a negative override should disable the cache (0), got %d", got)
	}
}

func TestSearchSymbolBundles_DisabledCacheStillServes(t *testing.T) {
	t.Setenv("GORTEX_BUNDLE_CACHE_MAX_MB", "0")
	s := newBundleTestStore(t)
	seedBundleStore(t, s)
	s.SetBundleFingerprints(map[string]uint64{"pkg": 1})

	res, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolBundles with the cache disabled: %v", err)
	}
	if b, ok := bundleByID(res)["pkg/x.go::A"]; !ok || len(b.OutEdges) != 1 {
		t.Fatalf("a disabled cache must still return live bundles, got %+v", b)
	}
	if s.bundles.maxBytes != 0 {
		t.Fatalf("expected the store's cache disabled, got maxBytes %d", s.bundles.maxBytes)
	}
	if len(s.bundles.entries) != 0 {
		t.Fatalf("a disabled cache must stay empty, got %d entries", len(s.bundles.entries))
	}
}

func TestBundleCache_ConcurrentReadInsert(t *testing.T) {
	c := newTestBundleCache()
	c.maxBytes = 8 << 10 // small budget so wholesale clears fire under contention
	c.refresh(map[string]uint64{"pkg": 1})

	const workers = 8
	const iters = 3000
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("pkg/x.go::N%d_%d", w, i%64)
				switch i % 3 {
				case 0:
					c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode(id, "W", "pkg/x.go")})
				case 1:
					_, _ = c.lookup(baseViewGeneration, id)
				default:
					c.refresh(map[string]uint64{"pkg": uint64(i)})
				}
			}
		}(w)
	}
	wg.Wait()

	// No goroutines remain: the running total must exactly equal the summed
	// bytes of the surviving entries (the accounting invariant), which also
	// proves it never drifted negative under contention.
	var sum int64
	for _, e := range c.entries {
		sum += e.bytes
	}
	if c.curBytes != sum {
		t.Fatalf("curBytes %d != sum of live entry bytes %d", c.curBytes, sum)
	}
	if c.curBytes > c.maxBytes {
		t.Fatalf("curBytes %d exceeds the byte budget %d", c.curBytes, c.maxBytes)
	}
}
