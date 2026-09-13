package store_sqlite

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// bundleCacheDefaultMaxBytes bounds the total heap the bundle cache may
// retain across all cached entries. A count ceiling alone is unsafe: an
// entry holds a decoded node plus its full in/out edge lists, and both
// nodes and edges carry meta maps, so entry sizes span ~1 KB for a leaf
// symbol to multiple MB for a hub node with thousands of edges. A cap
// measured in entries therefore admits an unbounded BYTE footprint — a
// few thousand hub bundles can pin gigabytes. This cache serves point
// lookups on the symbol-search hot path; it is a latency optimisation,
// not a working set that needs to be resident, so a modest budget is
// right — 64 MiB holds the hot few thousand ordinary bundles while
// keeping a long-lived daemon's idle heap bounded. Override with
// GORTEX_BUNDLE_CACHE_MAX_MB=<n> (n <= 0 disables the cache entirely).
const bundleCacheDefaultMaxBytes = 64 << 20 // 64 MiB

// bundleCacheMaxEntries is a secondary, generous count ceiling kept
// alongside the byte budget. The byte budget is the primary bound; this
// guards the map's own structural overhead in the degenerate case of a
// flood of tiny entries (a bucket slot and pointers per entry are not
// fully reflected in a per-entry byte estimate), and keeps the
// wholesale-clear allocation predictable. It is deliberately loose: a
// half-million-symbol monorepo's hottest few thousand search hits fit
// far under it, so in normal operation the byte budget always trips
// first.
const bundleCacheMaxEntries = 50000

// bundleCacheMaxGenerations bounds how many payload view generations may hold
// a fingerprint map — and therefore cacheable entries — at one time.
//
// The cache validates a bundle against the fingerprints of the snapshot that
// produced it, so every routed generation that wants cache hits needs its own
// map resident. That set has to be bounded (gate 8): a long-lived daemon mints
// a new generation on every commit-layer publish, and an unbounded map-per-
// generation ledger would retain one package-keyed map per generation for the
// life of the process, plus every entry those maps validate.
//
// Eight is the live working set, not the ancestry bound: a request reads the
// base corpus plus the handful of checkout generations a session actually has
// routed. The least-recently-used generation is evicted with its entries when
// a ninth appears — a retired generation simply ages out, and the generation
// that keeps serving keeps its entries. Eviction costs a recompute, never a
// wrong answer.
const bundleCacheMaxGenerations = 8

const (
	// bundleEntryOverhead is a coarse fixed charge per cached entry that
	// is independent of the bundle's string content: the bundleCacheEntry
	// wrapper, the *entry and *Node pointers, the graph.Node value's flat
	// struct (its string / slice / map headers, ints, and embedded
	// time.Time), and the map bucket the node id occupies. String and map
	// *contents* are added on top. Over-estimating here only makes the
	// cache clear sooner; it never lets the footprint overshoot the budget.
	bundleEntryOverhead = 448
	// bundleEdgeOverhead is the coarse fixed charge for one *Edge in an
	// in/out slice: the pointer, the slice slot, and the Edge value's flat
	// struct. Edge string / map contents are added separately.
	bundleEdgeOverhead = 240
	// bundleMetaEntryOverhead is the fixed per-key charge for a
	// map[string]any entry (bucket slot + interface header); the key
	// length and any string value length are added on top.
	bundleMetaEntryOverhead = 48
)

// bundleCacheEntry is one node's cached bundle, tagged with the package
// it belongs to and the package fingerprint that was current when the
// bundle was computed. The entry is served only while its OWN generation's
// fingerprint map still reports fp for pkgKey — any change to the package's
// content (a node or edge added / removed / reweighted, including a
// cross-file edge that lands on this node from elsewhere) moves the
// fingerprint and forces a recompute, so a cached bundle can never
// carry a stale edge.
type bundleCacheEntry struct {
	pkgKey string
	// viewGen is the payload view generation of the handle that computed
	// the bundle. It is recorded on the entry — not only folded into the
	// map key — so a refresh prunes exactly its own generation's entries,
	// and a generation eviction takes exactly that generation's entries
	// with it.
	viewGen int64
	fp      uint64
	bundle  graph.SymbolBundle
	// bytes is the entry's estimated retained size, recorded at insert so
	// the running byte total can be adjusted in O(1) whenever the entry is
	// dropped (invalidation or a stale read).
	bytes int64
}

// bundleCache is a content-addressed, package-scoped cache over
// SearchSymbolBundles. It is keyed at the node level but validated at
// the package level: an entry is fresh exactly when the package's
// current fingerprint matches the fingerprint the entry was stored at.
//
// Correctness rests entirely on the fingerprint discipline: the daemon
// hands the cache an authoritative per-package fingerprint map after
// every analysis pass (which runs after every incremental reindex and
// every edit_file / fsnotify-driven graph mutation). The fingerprints
// are edge-aware — they fold every package's nodes AND the edges
// touching them — so any mutation that could change a cached bundle's
// in/out edges moves the relevant package fingerprint and invalidates
// the entry. A package whose fingerprint is unchanged is served from
// cache; a package the daemon has never reported a fingerprint for is
// always treated as a miss (conservative: never serve an unvalidated
// bundle).
//
// The cache is bounded by bytes (maxBytes), not by entry count, because
// entry sizes vary by orders of magnitude with a node's edge fan-out and
// meta size. maxEntries is a secondary count ceiling only. When either
// bound would be exceeded the cache is cleared wholesale rather than
// evicting individually: entries are cheap to recompute (one batched
// fetch), and a wholesale clear keeps the bookkeeping O(1) and free of an
// LRU's per-entry ordering overhead. maxBytes <= 0 disables the cache —
// stores become no-ops and every lookup misses (reads still recompute
// live through the caller's fallback path).
//
// One core is shared by every handle over the same database, so ONE
// fingerprint map cannot decide freshness for all of them: a map describes
// exactly one snapshot — the payload view generation of the handle it was
// installed through — and says nothing about any other generation composed
// over it. A bundle computed at another generation and validated against that
// map would be a selected graph answered from another snapshot's cache data,
// which is exactly the mixed view a per-generation key exists to prevent.
//
// The maps are therefore held per generation: fingerprints[g] is the
// authoritative package map for generation g and the ONLY map g's entries are
// ever validated against. A generation with no installed map is inert — it
// stores nothing and every lookup misses — so an uninitialised cache validates
// nothing and generation zero is never assumed to be a described snapshot.
// Routed generations get cache hits of their own without borrowing the base's
// validation, and a base reindex retires base entries only.
//
// The number of resident generations is bounded (bundleCacheMaxGenerations),
// least-recently-used first, so a daemon that mints a generation per publish
// cannot accumulate maps or entries without limit.
type bundleCache struct {
	mu sync.Mutex
	// fingerprints maps a payload view generation to the authoritative
	// package fingerprint map for that snapshot. A generation absent from
	// this map is described by nothing and is never cached or served.
	fingerprints map[int64]*bundleFingerprintSet
	// fpSeq is the monotonic use counter behind the generation LRU. Every
	// refresh, hit and store stamps its generation with the next value, so
	// eviction drops the generation nothing has touched in longest.
	fpSeq uint64
	// maxGenerations bounds len(fingerprints); <= 0 means unbounded (tests
	// only — newBundleCache always sets it).
	maxGenerations int
	entries        map[string]*bundleCacheEntry
	maxBytes       int64 // byte budget (primary bound); <= 0 disables the cache
	maxEntries     int   // count ceiling (secondary bound)
	curBytes       int64 // running sum of entries' estimated bytes
}

// bundleFingerprintSet is one snapshot's authoritative package fingerprint
// map plus its position in the generation LRU.
type bundleFingerprintSet struct {
	fps  map[string]uint64
	used uint64
}

// newBundleCache builds an empty cache with the default budgets. The byte
// budget is overridable with GORTEX_BUNDLE_CACHE_MAX_MB=<n>; n <= 0
// disables the cache. It starts inert (every lookup a miss) until the
// daemon supplies fingerprints.
func newBundleCache() *bundleCache {
	return &bundleCache{
		fingerprints:   map[int64]*bundleFingerprintSet{},
		entries:        map[string]*bundleCacheEntry{},
		maxBytes:       bundleCacheMaxBytes(),
		maxEntries:     bundleCacheMaxEntries,
		maxGenerations: bundleCacheMaxGenerations,
	}
}

// bundleCacheMaxBytes resolves the byte budget from the environment,
// falling back to the default. GORTEX_BUNDLE_CACHE_MAX_MB is read in
// mebibytes; a value <= 0 returns 0 to disable the cache, and an
// unparseable value is ignored (keeps the default).
func bundleCacheMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("GORTEX_BUNDLE_CACHE_MAX_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return 0
			}
			return int64(n) << 20
		}
	}
	return bundleCacheDefaultMaxBytes
}

// bundleEntryBytes conservatively estimates a bundle's retained heap for
// the byte budget: a fixed per-entry charge plus the node's string and
// meta contents plus each in/out edge's fixed charge and its string and
// meta contents. Computed once at insert so the overflow check is a cheap
// scalar comparison.
func bundleEntryBytes(b graph.SymbolBundle) int64 {
	n := int64(bundleEntryOverhead)
	if b.Node != nil {
		n += nodeStringBytes(b.Node)
		n += metaBytes(b.Node.Meta)
	}
	for _, e := range b.InEdges {
		n += edgeBytes(e)
	}
	for _, e := range b.OutEdges {
		n += edgeBytes(e)
	}
	return n
}

// nodeStringBytes sums the byte lengths of a node's string fields (its
// heap-backed content, on top of the fixed struct overhead counted in
// bundleEntryOverhead).
func nodeStringBytes(nd *graph.Node) int64 {
	return int64(len(nd.ID) + len(nd.Name) + len(nd.QualName) + len(nd.FilePath) +
		len(string(nd.Kind)) + len(nd.Language) + len(nd.RepoPrefix) +
		len(nd.WorkspaceID) + len(nd.ProjectID) + len(nd.AbsoluteFilePath) +
		len(nd.Origin))
}

// edgeBytes estimates one edge's retained heap: the fixed per-edge charge
// plus its string fields and meta contents.
func edgeBytes(e *graph.Edge) int64 {
	if e == nil {
		return bundleEdgeOverhead
	}
	n := int64(bundleEdgeOverhead)
	n += int64(len(e.From) + len(e.To) + len(string(e.Kind)) + len(e.FilePath) +
		len(e.ConfidenceLabel) + len(e.Origin) + len(e.Tier) + len(e.Context) +
		len(e.ReturnUsage) + len(e.Via) + len(e.Alias))
	n += metaBytes(e.Meta)
	return n
}

// metaBytes estimates a meta map's retained heap: a fixed charge per key
// plus the key length and, for string values, the value length. Non-string
// values fold into the fixed charge — meta values are overwhelmingly short
// scalars, and a coarse estimate only over-counts, which is safe.
func metaBytes(m map[string]any) int64 {
	if len(m) == 0 {
		return 0
	}
	var n int64
	for k, v := range m {
		n += int64(len(k) + bundleMetaEntryOverhead)
		if s, ok := v.(string); ok {
			n += int64(len(s))
		}
	}
	return n
}

// SetBundleFingerprints installs the authoritative per-package
// fingerprint map and drops any cached entry whose package fingerprint
// has changed (or whose package is no longer reported). This is the
// invalidation entry point: the daemon calls it after each analysis
// pass with the fresh fingerprints derived from the live graph, so a
// reindex that altered a package's nodes or edges retires exactly the
// affected bundles while leaving untouched packages cached.
//
// fps is keyed by package key (the directory the package's files live
// in, repo-prefixed in multi-repo because the node file paths are).
//
// The fingerprints describe the snapshot THIS handle reads: the daemon
// derives them from the graph it just analysed, and that graph is whatever
// the handle it installs them through serves. The handle's payload view
// generation is therefore the identity of the fingerprinted snapshot, and the
// map is installed UNDER that generation, so no other generation's bundles can
// be validated against it — and, symmetrically, installing one generation's
// map no longer retires another generation's entries.
func (s *Store) SetBundleFingerprints(fps map[string]uint64) {
	if s.bundles == nil {
		return
	}
	s.bundles.refresh(s.viewGen, fps)
}

// refresh installs the new fingerprint map as the authoritative map for
// viewGen and prunes the entries of THAT generation which the new map cannot
// validate — one whose package fingerprint moved, or whose package the new map
// no longer reports. Entries belonging to other generations are untouched:
// they are validated against their own generation's map, which this call says
// nothing about. Each drop decrements the running byte total by the entry's
// estimated size.
//
// Installing a map also makes viewGen the most recently used generation, and
// evicts the least recently used one (with its entries) if that pushes the
// resident set past maxGenerations.
func (c *bundleCache) refresh(viewGen int64, fps map[string]uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fps == nil {
		fps = map[string]uint64{}
	}
	if c.fingerprints == nil {
		c.fingerprints = map[int64]*bundleFingerprintSet{}
	}
	set := &bundleFingerprintSet{fps: fps}
	c.fingerprints[viewGen] = set
	c.touchLocked(set)
	for id, e := range c.entries {
		if e.viewGen != viewGen {
			continue
		}
		cur, ok := fps[e.pkgKey]
		if !ok || cur != e.fp {
			delete(c.entries, id)
			c.curBytes -= e.bytes
		}
	}
	c.evictGenerationsLocked()
}

// touchLocked stamps a fingerprint set as the most recently used generation.
// The caller holds c.mu.
func (c *bundleCache) touchLocked(set *bundleFingerprintSet) {
	c.fpSeq++
	set.used = c.fpSeq
}

// evictGenerationsLocked drops least-recently-used generations — their
// fingerprint map and every entry they own — until the resident set is within
// maxGenerations. This is the cache's ancestry bound: a daemon that mints a
// generation per publish retains only the generations still being read. The
// caller holds c.mu.
func (c *bundleCache) evictGenerationsLocked() {
	if c.maxGenerations <= 0 {
		return
	}
	for len(c.fingerprints) > c.maxGenerations {
		var oldestGen int64
		var oldestUse uint64
		first := true
		for gen, set := range c.fingerprints {
			if first || set.used < oldestUse {
				oldestGen, oldestUse, first = gen, set.used, false
			}
		}
		if first {
			return
		}
		delete(c.fingerprints, oldestGen)
		for id, e := range c.entries {
			if e.viewGen == oldestGen {
				delete(c.entries, id)
				c.curBytes -= e.bytes
			}
		}
	}
}

// fingerprintsLocked returns the authoritative fingerprint map for viewGen, or
// nil when no map has been installed for that snapshot. It is the cache's
// snapshot-identity gate: a lookup or a store at a generation nothing
// describes is refused outright, so a selected graph is never served bundles
// another snapshot computed. The caller holds c.mu.
func (c *bundleCache) fingerprintsLocked(viewGen int64) *bundleFingerprintSet {
	if c.fingerprints == nil {
		return nil
	}
	return c.fingerprints[viewGen]
}

// bundlePackageKey derives the package key for a node's file path. It
// mirrors the analysis layer's packageKey so the cache and the
// daemon-supplied fingerprint map agree on package identity: the
// directory the file lives in (repo-prefixed in multi-repo because the
// stored file paths are), or "" for a file at the repo root / a node
// with no path.
func bundlePackageKey(filePath string) string {
	if filePath == "" {
		return ""
	}
	// path.Dir, not filepath.Dir: stored file paths are forward-slash by
	// contract, and so are the keys of the fingerprint map this key is looked
	// up in. filepath.Dir cleans to the OS separator, which on Windows undoes
	// the ToSlash and yields "repoA\pkg" for a map keyed "repoA/pkg" — every
	// entry then reads as unfingerprinted and the cache never serves a hit.
	dir := path.Dir(filepath.ToSlash(filePath))
	if dir == "." {
		return ""
	}
	return dir
}

// bundleCacheKey namespaces an entry by the payload view generation of the
// handle that computed it. One core is shared by every handle over the same
// database, so a node id alone would let one generation's bundle — its own
// node, its own in/out edges — answer another generation's lookup.
func bundleCacheKey(viewGen int64, id string) string {
	return strconv.FormatInt(viewGen, 10) + "\x00" + id
}

// lookup returns the cached bundle for id in viewGen when it is fresh — the
// installed fingerprint map describes viewGen, the entry exists, and its
// package fingerprint still matches the current one. A node whose package
// has no reported fingerprint is never served (ok is false) so an
// unvalidated bundle can never escape the cache, and a generation the
// fingerprints do not describe misses outright rather than borrowing
// another snapshot's validation. A stale entry is dropped in place and its
// bytes reclaimed.
func (c *bundleCache) lookup(viewGen int64, id string) (graph.SymbolBundle, bool) {
	key := bundleCacheKey(viewGen, id)
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.fingerprintsLocked(viewGen)
	if set == nil {
		return graph.SymbolBundle{}, false
	}
	e, ok := c.entries[key]
	if !ok {
		return graph.SymbolBundle{}, false
	}
	cur, ok := set.fps[e.pkgKey]
	if !ok || cur != e.fp {
		// Stale or unvalidated — drop it so a later refresh doesn't
		// have to, and reclaim its bytes.
		delete(c.entries, key)
		c.curBytes -= e.bytes
		return graph.SymbolBundle{}, false
	}
	// A served generation is a live one: keep it ahead of a generation
	// nothing reads in the LRU, so a routed view that keeps answering
	// requests is not evicted by base reindexes it never consults.
	c.touchLocked(set)
	return e.bundle, true
}

// store records a freshly computed bundle, tagged with its package's
// current fingerprint. A node whose package has no reported fingerprint
// is NOT cached (it could not be validated on read-back), keeping the
// cache conservative. The cache is bounded by bytes: when admitting the
// new entry would push the running total over the byte budget (or the
// count over the secondary ceiling) the cache is cleared wholesale
// before the insert. A single bundle that on its own exceeds the whole
// budget — a hub node with thousands of edges, exactly the pathological
// case a byte cap exists to keep out of long-lived memory — is refused
// outright rather than pinned. With maxBytes <= 0 the cache is disabled
// and every store is a no-op.
func (c *bundleCache) store(viewGen int64, b graph.SymbolBundle) {
	if b.Node == nil {
		return
	}
	key := bundleCacheKey(viewGen, b.Node.ID)
	pkgKey := bundlePackageKey(b.Node.FilePath)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxBytes <= 0 {
		return
	}
	set := c.fingerprintsLocked(viewGen)
	if set == nil {
		// No fingerprints describe this snapshot, so the bundle could never
		// be validated on read-back. Caching it would only pin bytes for an
		// entry no lookup can ever serve.
		return
	}
	fp, ok := set.fps[pkgKey]
	if !ok {
		return
	}
	c.touchLocked(set)
	sz := bundleEntryBytes(b)
	if sz > c.maxBytes {
		// One entry larger than the entire budget would blow the bound and
		// be evicted by the very next insert's wholesale clear anyway.
		return
	}
	if old, ok := c.entries[key]; ok {
		// Replacing an existing entry — discount its bytes and drop it so
		// curBytes and the count check track the live set.
		c.curBytes -= old.bytes
		delete(c.entries, key)
	}
	if len(c.entries) > 0 && (c.curBytes+sz > c.maxBytes || len(c.entries) >= c.maxEntries) {
		c.entries = make(map[string]*bundleCacheEntry)
		c.curBytes = 0
	}
	c.entries[key] = &bundleCacheEntry{pkgKey: pkgKey, viewGen: viewGen, fp: fp, bundle: b, bytes: sz}
	c.curBytes += sz
}
