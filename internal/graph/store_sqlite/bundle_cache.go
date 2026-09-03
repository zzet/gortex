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
// bundle was computed. The entry is served only while
// fingerprints[pkgKey] still equals fp — any change to the package's
// content (a node or edge added / removed / reweighted, including a
// cross-file edge that lands on this node from elsewhere) moves the
// fingerprint and forces a recompute, so a cached bundle can never
// carry a stale edge.
type bundleCacheEntry struct {
	pkgKey string
	fp     uint64
	bundle graph.SymbolBundle
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
type bundleCache struct {
	mu           sync.Mutex
	fingerprints map[string]uint64
	entries      map[string]*bundleCacheEntry
	maxBytes     int64 // byte budget (primary bound); <= 0 disables the cache
	maxEntries   int   // count ceiling (secondary bound)
	curBytes     int64 // running sum of entries' estimated bytes
}

// newBundleCache builds an empty cache with the default budgets. The byte
// budget is overridable with GORTEX_BUNDLE_CACHE_MAX_MB=<n>; n <= 0
// disables the cache. It starts inert (every lookup a miss) until the
// daemon supplies fingerprints.
func newBundleCache() *bundleCache {
	return &bundleCache{
		fingerprints: map[string]uint64{},
		entries:      map[string]*bundleCacheEntry{},
		maxBytes:     bundleCacheMaxBytes(),
		maxEntries:   bundleCacheMaxEntries,
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
func (s *Store) SetBundleFingerprints(fps map[string]uint64) {
	if s.bundles == nil {
		return
	}
	s.bundles.refresh(fps)
}

// refresh swaps in the new fingerprint map and prunes every entry whose
// package fingerprint no longer matches, decrementing the running byte
// total by each dropped entry's estimated size.
func (c *bundleCache) refresh(fps map[string]uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fps == nil {
		fps = map[string]uint64{}
	}
	c.fingerprints = fps
	for id, e := range c.entries {
		cur, ok := fps[e.pkgKey]
		if !ok || cur != e.fp {
			delete(c.entries, id)
			c.curBytes -= e.bytes
		}
	}
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
// entry exists and its package fingerprint still matches the current one. A
// node whose package has no reported fingerprint is never served (ok is
// false) so an unvalidated bundle can never escape the cache. A stale
// entry is dropped in place and its bytes reclaimed.
func (c *bundleCache) lookup(viewGen int64, id string) (graph.SymbolBundle, bool) {
	key := bundleCacheKey(viewGen, id)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return graph.SymbolBundle{}, false
	}
	cur, ok := c.fingerprints[e.pkgKey]
	if !ok || cur != e.fp {
		// Stale or unvalidated — drop it so a later refresh doesn't
		// have to, and reclaim its bytes.
		delete(c.entries, key)
		c.curBytes -= e.bytes
		return graph.SymbolBundle{}, false
	}
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
	fp, ok := c.fingerprints[pkgKey]
	if !ok {
		return
	}
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
	c.entries[key] = &bundleCacheEntry{pkgKey: pkgKey, fp: fp, bundle: b, bytes: sz}
	c.curBytes += sz
}
