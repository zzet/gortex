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
		fingerprints:   map[int64]*bundleFingerprintSet{},
		entries:        map[string]*bundleCacheEntry{},
		maxBytes:       bundleCacheDefaultMaxBytes,
		maxEntries:     bundleCacheMaxEntries,
		maxGenerations: bundleCacheMaxGenerations,
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
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 100})
	c.store(baseViewGeneration, b)
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); !ok {
		t.Fatal("bundle should be served once its package fingerprint is known")
	}
}

func TestBundleCache_InvalidatesOnFingerprintChange(t *testing.T) {
	c := newTestBundleCache()
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})

	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); !ok {
		t.Fatal("expected a cache hit on the unchanged fingerprint")
	}

	// Fingerprint changes -> the entry is invalidated.
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 2})
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("entry must be dropped when its package fingerprint changes")
	}
}

func TestBundleCache_CrossRepoIsolation(t *testing.T) {
	c := newTestBundleCache()
	// Two repos with the same inner directory name resolve to DIFFERENT
	// package keys because the stored file paths are repo-prefixed.
	c.refresh(baseViewGeneration, map[string]uint64{
		"repoA/pkg": 10,
		"repoB/pkg": 20,
	})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("repoA/pkg/x.go::A", "A", "repoA/pkg/x.go")})
	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("repoB/pkg/x.go::A", "A", "repoB/pkg/x.go")})

	// Bumping only repoA's fingerprint must not touch repoB's entry.
	c.refresh(baseViewGeneration, map[string]uint64{
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
	s, err := openPristine(t, filepath.Join(t.TempDir(), "b.sqlite"))
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
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})

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
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})
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
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})
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
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})

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
					c.refresh(baseViewGeneration, map[string]uint64{"pkg": uint64(i)})
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

// --- snapshot-identity tests (the fingerprint map speaks for one view) ---

// A fingerprint map describes exactly one snapshot: the payload view
// generation of the handle it was installed through. Another generation's
// bundle must never be validated against it — that is a selected graph
// answered from a different snapshot's cache data.
func TestBundleCache_RefusesGenerationTheFingerprintsDoNotDescribe(t *testing.T) {
	const otherGen = int64(7)
	c := newTestBundleCache()
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})

	b := graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")}

	// The package IS fingerprinted, but for the base snapshot only. A store
	// from a handle on another generation must be refused outright.
	c.store(otherGen, b)
	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); ok {
		t.Fatal("a generation the fingerprints do not describe must never be served from cache")
	}
	if len(c.entries) != 0 || c.curBytes != 0 {
		t.Fatalf("a refused store must leave the cache empty, got %d entries / %d bytes",
			len(c.entries), c.curBytes)
	}

	// The described snapshot still caches and serves normally.
	c.store(baseViewGeneration, b)
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); !ok {
		t.Fatal("the fingerprinted snapshot must still be served from cache")
	}
	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); ok {
		t.Fatal("another generation must not read the fingerprinted snapshot's entry")
	}
}

// Each generation owns its own fingerprint map, so installing one snapshot's
// fingerprints prunes only that snapshot's entries. A routed generation that
// nobody re-analysed keeps serving its own cached bundles across an unrelated
// base reindex — without that, a routed view is permanently uncacheable
// because the base pass is the one that runs on every mutation.
func TestBundleCache_RefreshPrunesOnlyItsOwnGeneration(t *testing.T) {
	const otherGen = int64(7)
	c := newTestBundleCache()
	c.refresh(otherGen, map[string]uint64{"pkg": 1})
	c.store(otherGen, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})
	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); !ok {
		t.Fatal("the fingerprinted generation should have cached its bundle")
	}
	bytesAfterStore := c.curBytes

	// Another snapshot reports its own fingerprints. Generation 7's entry is
	// validated against generation 7's map, which this call did not touch.
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 99})
	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); !ok {
		t.Fatal("another generation's refresh must not retire this generation's entries")
	}
	if c.curBytes != bytesAfterStore {
		t.Fatalf("byte total must be unchanged by another generation's refresh, got %d want %d",
			c.curBytes, bytesAfterStore)
	}

	// Cross-generation reads still miss: the base has its own (different)
	// fingerprint and its own key namespace.
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("the base generation must not read generation 7's entry")
	}

	// And a refresh for generation 7 itself DOES retire its entries when the
	// package fingerprint moves.
	c.refresh(otherGen, map[string]uint64{"pkg": 2})
	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); ok {
		t.Fatal("a generation's own fingerprint change must retire its entry")
	}
	if c.curBytes != 0 {
		t.Fatalf("retiring the only entry must reclaim its bytes, got %d", c.curBytes)
	}
}

// A generation whose package fingerprints the new map no longer reports is
// retired too — a package that disappeared from the snapshot can never be
// validated again.
func TestBundleCache_RefreshRetiresUnreportedPackages(t *testing.T) {
	const gen = int64(3)
	c := newTestBundleCache()
	c.refresh(gen, map[string]uint64{"pkg": 1})
	c.store(gen, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})
	if _, ok := c.lookup(gen, "pkg/x.go::A"); !ok {
		t.Fatal("expected the entry to be cached")
	}
	c.refresh(gen, map[string]uint64{"other": 1})
	if _, ok := c.lookup(gen, "pkg/x.go::A"); ok {
		t.Fatal("an entry whose package the new map does not report must be retired")
	}
	if c.curBytes != 0 {
		t.Fatalf("retired entry must reclaim its bytes, got %d", c.curBytes)
	}
}

// Gate 8: the resident generation set is bounded. A daemon that mints a
// generation per publish must not accumulate fingerprint maps — or the
// entries they validate — without limit.
func TestBundleCache_BoundsResidentGenerations(t *testing.T) {
	c := newTestBundleCache()
	total := bundleCacheMaxGenerations + 4
	for g := 1; g <= total; g++ {
		gen := int64(g)
		c.refresh(gen, map[string]uint64{"pkg": 1})
		c.store(gen, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})
	}
	if len(c.fingerprints) != bundleCacheMaxGenerations {
		t.Fatalf("resident generations = %d, want the %d-generation bound",
			len(c.fingerprints), bundleCacheMaxGenerations)
	}
	if len(c.entries) != bundleCacheMaxGenerations {
		t.Fatalf("entries = %d: an evicted generation must take its entries with it",
			len(c.entries))
	}
	if c.curBytes <= 0 {
		t.Fatalf("surviving entries must still account bytes, got %d", c.curBytes)
	}
	// The oldest generations are the ones gone; the newest survive.
	if _, ok := c.lookup(1, "pkg/x.go::A"); ok {
		t.Fatal("the least recently used generation must have been evicted")
	}
	if _, ok := c.lookup(int64(total), "pkg/x.go::A"); !ok {
		t.Fatal("the most recently used generation must survive")
	}
}

// Eviction is least-RECENTLY-USED, not oldest-installed: a routed generation
// that keeps serving lookups outlives generations nobody reads. Without this
// a daemon publishing a generation per commit would evict the view a session
// is actively reading.
func TestBundleCache_GenerationEvictionIsLRUNotFIFO(t *testing.T) {
	c := newTestBundleCache()
	const routed = int64(1)
	c.refresh(routed, map[string]uint64{"pkg": 1})
	c.store(routed, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})

	// Fill the rest of the bound, touching the routed generation after each
	// new arrival the way a session reading it would.
	for g := 2; g <= bundleCacheMaxGenerations; g++ {
		c.refresh(int64(g), map[string]uint64{"pkg": 1})
		if _, ok := c.lookup(routed, "pkg/x.go::A"); !ok {
			t.Fatalf("routed generation lost its entry at %d", g)
		}
	}
	// One more generation forces an eviction. The routed generation was used
	// most recently, so generation 2 — installed early and never read — goes.
	c.refresh(int64(bundleCacheMaxGenerations+1), map[string]uint64{"pkg": 1})
	if _, ok := c.lookup(routed, "pkg/x.go::A"); !ok {
		t.Fatal("the most recently READ generation must survive an eviction")
	}
	if c.describesGeneration(2) {
		t.Fatal("the least recently used generation must have been evicted")
	}
}

// describesGeneration reports whether a fingerprint map is installed for
// viewGen, with the lock taken, for tests that assert on residency.
func (c *bundleCache) describesGeneration(viewGen int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fingerprintsLocked(viewGen) != nil
}

// An uninitialised cache validates nothing: generation zero is not assumed
// to be the described snapshot before any fingerprints are installed.
func TestBundleCache_InertUntilFingerprintsAreInstalled(t *testing.T) {
	c := newTestBundleCache()

	// The gate itself: an uninitialised cache describes NO generation, and in
	// particular not generation zero, whose numeric value a zero-valued
	// generation field would otherwise match. Asserted on fingerprintsLocked
	// directly because that is where the claim lives — store() and lookup()
	// below also refuse, but they would refuse on the empty fingerprint /
	// entry maps even with the gate gone, so they cannot pin it.
	c.mu.Lock()
	describesBase := c.fingerprintsLocked(baseViewGeneration) != nil
	c.mu.Unlock()
	if describesBase {
		t.Fatal("an uninitialised cache must describe no snapshot, generation zero included")
	}

	c.store(baseViewGeneration, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("a cache with no installed fingerprint map must always miss")
	}
	if len(c.fingerprints) != 0 {
		t.Fatal("no fingerprint map may exist until refresh installs one")
	}

	// After a refresh for generation zero it describes exactly that snapshot,
	// so the gate is an identity test and not a blanket refusal.
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})
	c.mu.Lock()
	describesBase = c.fingerprintsLocked(baseViewGeneration) != nil
	describesOther := c.fingerprintsLocked(7) != nil
	c.mu.Unlock()
	if !describesBase {
		t.Fatal("after installing generation zero's fingerprints the cache must describe generation zero")
	}
	if describesOther {
		t.Fatal("generation zero's fingerprints must not describe generation 7")
	}
}

// Production entrypoint: fingerprints installed through a handle on one
// generation must not validate another generation's bundles served through
// Store.SearchSymbolBundles. The base handle here has no fingerprints of its
// own, so every query recomputes live and a new edge shows up immediately —
// the mirror of TestSearchSymbolBundles_CacheHitOnUnchangedFingerprint,
// which proves the same store DOES cache when the generations agree.
func TestSearchSymbolBundles_FingerprintsOfAnotherGenerationNeverValidate(t *testing.T) {
	s := newBundleTestStore(t)
	seedBundleStore(t, s)

	derived := s.AtGeneration(7)
	if derived == nil {
		t.Fatal("AtGeneration(7) returned nil")
	}
	if derived.ViewGeneration() != 7 {
		t.Fatalf("derived handle generation = %d, want 7", derived.ViewGeneration())
	}
	// The daemon installs the fingerprints it derived from generation 7's
	// graph. The base corpus is a different snapshot.
	derived.SetBundleFingerprints(map[string]uint64{"pkg": 1})

	first, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("first SearchSymbolBundles: %v", err)
	}
	if b := bundleByID(first)["pkg/x.go::A"]; len(b.OutEdges) != 1 {
		t.Fatalf("expected A with 1 out-edge on the first query, got %d", len(b.OutEdges))
	}

	// Mutate the base graph without touching the fingerprints. A cache that
	// accepted another generation's fingerprints would serve the stale
	// 1-edge bundle here.
	s.AddNode(mkFnNode("pkg/x.go::C", "GammaWidget", "pkg/x.go"))
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::C", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})

	second, err := s.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("second SearchSymbolBundles: %v", err)
	}
	if b := bundleByID(second)["pkg/x.go::A"]; len(b.OutEdges) != 2 {
		t.Fatalf("the base corpus must recompute live under another generation's fingerprints, got %d edges",
			len(b.OutEdges))
	}
	if len(s.bundles.entries) != 0 {
		t.Fatalf("no base-generation bundle may be cached under generation 7's fingerprints, got %d entries",
			len(s.bundles.entries))
	}
}

// lookup enforces the snapshot gate itself rather than trusting that only
// refresh and store ever populate the map. An entry that belongs to a
// snapshot the installed fingerprints do not describe must never be served,
// however it came to be there.
func TestBundleCache_LookupRefusesForeignGenerationEntry(t *testing.T) {
	const otherGen = int64(7)
	c := newTestBundleCache()
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})

	b := graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")}
	// Inject directly: the package fingerprint matches the installed map, so
	// only the snapshot identity separates this entry from a valid one.
	c.entries[bundleCacheKey(otherGen, "pkg/x.go::A")] = &bundleCacheEntry{
		pkgKey:  "pkg",
		viewGen: otherGen,
		fp:      1,
		bundle:  b,
		bytes:   bundleEntryBytes(b),
	}

	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); ok {
		t.Fatal("lookup served an entry from a snapshot the fingerprints do not describe")
	}
}

// Two generations that BOTH have fingerprints installed still never share an
// entry. The old single-map cache could only express "one described snapshot",
// so this case did not exist: the second install took the cache over. With a
// map per generation the isolation has to be re-proven with both described.
func TestBundleCache_DescribedGenerationsNeverShareEntries(t *testing.T) {
	const otherGen = int64(7)
	c := newTestBundleCache()
	c.refresh(baseViewGeneration, map[string]uint64{"pkg": 1})
	c.refresh(otherGen, map[string]uint64{"pkg": 1})

	b := graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")}
	c.store(otherGen, b)

	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); ok {
		t.Fatal("a described base generation must not read another generation's entry")
	}
	if _, ok := c.lookup(otherGen, "pkg/x.go::A"); !ok {
		t.Fatal("the storing generation must serve its own entry")
	}

	// Symmetric: the base stores its own bundle and the two coexist.
	c.store(baseViewGeneration, b)
	if _, ok := c.lookup(baseViewGeneration, "pkg/x.go::A"); !ok {
		t.Fatal("the base generation must serve its own entry")
	}
	if len(c.entries) != 2 {
		t.Fatalf("two generations must hold two entries, got %d", len(c.entries))
	}
}

// Production entrypoint, the restored behaviour: a routed (derived) generation
// gets bundle cache HITS of its own, and keeps them across a base reindex.
//
// The base analysis pass runs on every graph mutation, so a cache that held
// one fingerprint map for the whole core retired the routed generation's
// entries on every base pass — routed views were effectively uncacheable.
// Revert-red: with refresh pruning across generations again, the second query
// below recomputes and reports the freshly added edge.
func TestSearchSymbolBundles_RoutedGenerationKeepsItsCacheAcrossABaseReindex(t *testing.T) {
	base, derived := openGenerationReadPair(t)
	pkg := bundlePackageKey(genReadFileA)

	base.SetBundleFingerprints(map[string]uint64{pkg: 1})
	derived.SetBundleFingerprints(map[string]uint64{pkg: 1})

	first, err := derived.SearchSymbolBundles("Shared"+genOneMark, 4)
	if err != nil {
		t.Fatalf("warm routed bundles: %v", err)
	}
	warm, ok := bundleByID(first)[genReadShared]
	if !ok || warm.Node == nil {
		t.Fatalf("routed query returned no bundle for %s: %+v", genReadShared, first)
	}
	warmEdges := len(warm.OutEdges)

	// Generation 1 gains an out-edge without its OWN fingerprint moving, so a
	// cache hit must still serve the pre-mutation (warmEdges) bundle. That is
	// the hit PROBE, not a licence to serve stale bundles: the cache's contract
	// is that a producer which changes a package's content moves that package's
	// fingerprint (refresh then retires the entry), and this fixture
	// deliberately violates it so a hit is observable at all.
	const addedID = "repo::pkg/a.go::AddedGenOne"
	derived.AddNode(&graph.Node{
		ID: addedID, Kind: graph.KindFunction, Name: "AddedGenOne",
		FilePath: genReadFileA, RepoPrefix: genReadRepo, Language: "go",
	})
	derived.AddEdge(&graph.Edge{
		From: genReadShared, To: addedID, Kind: graph.EdgeCalls, FilePath: genReadFileA,
	})

	// …and the base analysis pass reports its own, moved, fingerprints. This is
	// the step the old single-map cache could not survive.
	base.SetBundleFingerprints(map[string]uint64{pkg: 2})

	second, err := derived.SearchSymbolBundles("Shared"+genOneMark, 4)
	if err != nil {
		t.Fatalf("second routed bundles: %v", err)
	}
	got, ok := bundleByID(second)[genReadShared]
	if !ok || got.Node == nil {
		t.Fatalf("routed re-query returned no bundle for %s: %+v", genReadShared, second)
	}
	if len(got.OutEdges) != warmEdges {
		t.Fatalf("routed generation must still be served from its own cache after a base reindex: out-edges %d, want the cached %d",
			len(got.OutEdges), warmEdges)
	}
}

// The other direction of the same fence: with both generations fingerprinted,
// the base handle still recomputes from the base corpus and never sees the
// routed generation's payload.
func TestSearchSymbolBundles_BaseNeverReadsARoutedGenerationsEntries(t *testing.T) {
	base, derived := openGenerationReadPair(t)
	pkg := bundlePackageKey(genReadFileA)
	base.SetBundleFingerprints(map[string]uint64{pkg: 1})
	derived.SetBundleFingerprints(map[string]uint64{pkg: 1})

	if _, err := derived.SearchSymbolBundles("Shared"+genOneMark, 4); err != nil {
		t.Fatalf("warm routed bundles: %v", err)
	}
	bundles, err := base.SearchSymbolBundles("Shared"+genZeroMark, 4)
	if err != nil {
		t.Fatalf("base bundles: %v", err)
	}
	if len(bundles) == 0 {
		t.Fatal("base bundle search returned nothing")
	}
	for _, b := range bundles {
		if b.Node == nil {
			continue
		}
		if strings.Contains(b.Node.Name, genOneMark) {
			t.Fatalf("base handle served a routed generation's bundle: %+v", b.Node)
		}
	}
}

// The generation LRU (touchLocked / evictGenerationsLocked) is written on the
// read path too, so every lookup, store and refresh now mutates cache state.
// Under concurrent traffic across more generations than the bound admits, the
// byte accounting must still close exactly and the resident set must stay
// within the bound.
func TestBundleCache_ConcurrentAcrossGenerations(t *testing.T) {
	c := newTestBundleCache()
	c.maxBytes = 8 << 10 // small budget so wholesale clears fire under contention
	generations := bundleCacheMaxGenerations + 3
	for g := 0; g < generations; g++ {
		c.refresh(int64(g), map[string]uint64{"pkg": 1})
	}

	const workers = 8
	const iters = 2000
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				gen := int64((w + i) % generations)
				id := fmt.Sprintf("pkg/x.go::N%d_%d", w, i%64)
				switch i % 4 {
				case 0:
					c.store(gen, graph.SymbolBundle{Node: mkFnNode(id, "W", "pkg/x.go")})
				case 1, 2:
					_, _ = c.lookup(gen, id)
				default:
					c.refresh(gen, map[string]uint64{"pkg": uint64(i)})
				}
			}
		}(w)
	}
	wg.Wait()

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
	if len(c.fingerprints) > bundleCacheMaxGenerations {
		t.Fatalf("resident generations %d exceeds the bound %d",
			len(c.fingerprints), bundleCacheMaxGenerations)
	}
	// No entry may survive its generation's eviction.
	for id, e := range c.entries {
		if _, ok := c.fingerprints[e.viewGen]; !ok {
			t.Fatalf("entry %q survives evicted generation %d", id, e.viewGen)
		}
	}
}

// refresh reclaims its own generation's stale entries AT REFRESH TIME, not
// lazily on the next lookup. lookup drops a stale entry it happens to be asked
// for, so an assertion taken after a lookup cannot tell the two apart; the
// entries a refresh retires are usually the ones nobody looks up again, and
// those are exactly the bytes that would otherwise be pinned for the life of
// the cache. Asserted here without a single intervening lookup.
func TestBundleCache_RefreshReclaimsStaleEntriesWithoutALookup(t *testing.T) {
	const gen = int64(5)
	c := newTestBundleCache()
	c.refresh(gen, map[string]uint64{"pkg": 1, "keep": 1})

	c.store(gen, graph.SymbolBundle{Node: mkFnNode("keep/y.go::K", "K", "keep/y.go")})
	keepOnlyBytes := c.curBytes
	c.store(gen, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})
	if len(c.entries) != 2 || c.curBytes <= keepOnlyBytes {
		t.Fatalf("expected two stored entries, got %d entries / %d bytes (keep-only %d)",
			len(c.entries), c.curBytes, keepOnlyBytes)
	}

	// "pkg" moved; "keep" did not. No lookup runs between the refresh and the
	// assertions, so what survives is what refresh itself decided to keep.
	c.refresh(gen, map[string]uint64{"pkg": 2, "keep": 1})

	if len(c.entries) != 1 {
		t.Fatalf("refresh must retire the stale entry in place, got %d entries", len(c.entries))
	}
	if _, ok := c.entries[bundleCacheKey(gen, "keep/y.go::K")]; !ok {
		t.Fatal("refresh must keep the entry its new map still validates")
	}
	if c.curBytes != keepOnlyBytes {
		t.Fatalf("refresh must reclaim the retired entry's bytes, got %d want %d",
			c.curBytes, keepOnlyBytes)
	}
}

// The refresh-time reclamation has two arms — a package whose fingerprint
// moved, and a package the new map no longer reports at all — and only the
// first is exercised by a fingerprint bump. The second arm is invisible to any
// assertion that reads through lookup, because lookup re-runs the same
// validation lazily (bundle_cache.go: the `!ok || cur != e.fp` check appears in
// both), so a refresh that kept the unreported entry would still look correct
// from the outside while pinning its bytes forever. This test therefore asserts
// on the entry set directly, with no intervening lookup.
func TestBundleCache_RefreshReclaimsUnreportedPackagesWithoutALookup(t *testing.T) {
	const gen = int64(7)
	c := newTestBundleCache()
	c.refresh(gen, map[string]uint64{"keep": 1, "gone": 1})

	c.store(gen, graph.SymbolBundle{Node: mkFnNode("keep/y.go::K", "K", "keep/y.go")})
	keepOnlyBytes := c.curBytes
	c.store(gen, graph.SymbolBundle{Node: mkFnNode("gone/z.go::G", "G", "gone/z.go")})
	if len(c.entries) != 2 || c.curBytes <= keepOnlyBytes {
		t.Fatalf("expected two stored entries, got %d entries / %d bytes (keep-only %d)",
			len(c.entries), c.curBytes, keepOnlyBytes)
	}

	// "gone" is absent from the new map entirely: its fingerprint did not move,
	// its package simply stopped being reported (deleted, or moved out of the
	// analysed set). "keep" is unchanged.
	c.refresh(gen, map[string]uint64{"keep": 1})

	if len(c.entries) != 1 {
		t.Fatalf("refresh must retire the entry whose package the new map no longer reports, got %d entries",
			len(c.entries))
	}
	if _, ok := c.entries[bundleCacheKey(gen, "keep/y.go::K")]; !ok {
		t.Fatal("refresh must keep the entry its new map still validates")
	}
	if c.curBytes != keepOnlyBytes {
		t.Fatalf("refresh must reclaim the unreported entry's bytes, got %d want %d",
			c.curBytes, keepOnlyBytes)
	}
}

// A generation that is being WRITTEN is live even before anything reads it
// back: the store path stamps the generation LRU too. Without that stamp a
// routed generation whose bundles are being computed right now is evicted ahead
// of a generation that was installed earlier and has sat idle ever since —
// throwing away the work in flight. Nothing here reads through lookup, because
// lookup does its own touch and would mask the store-side one.
func TestBundleCache_StoreKeepsItsGenerationLive(t *testing.T) {
	c := newTestBundleCache()
	const written = int64(1)
	c.refresh(written, map[string]uint64{"pkg": 1})

	// Fill the rest of the bound with generations installed AFTER the written
	// one and never used again.
	for g := 2; g <= bundleCacheMaxGenerations; g++ {
		c.refresh(int64(g), map[string]uint64{"pkg": 1})
	}
	// The only use the written generation gets is the write itself.
	c.store(written, graph.SymbolBundle{Node: mkFnNode("pkg/x.go::A", "A", "pkg/x.go")})

	// One more generation pushes the resident set past the bound.
	c.refresh(int64(bundleCacheMaxGenerations+1), map[string]uint64{"pkg": 1})

	if !c.describesGeneration(written) {
		t.Fatal("the generation being written must survive an eviction: store stamps the generation LRU")
	}
	if _, ok := c.entries[bundleCacheKey(written, "pkg/x.go::A")]; !ok {
		t.Fatal("the written generation's entry must survive with its generation")
	}
	if c.describesGeneration(2) {
		t.Fatal("the least recently used generation — installed early, never used — must be the eviction victim")
	}
}
