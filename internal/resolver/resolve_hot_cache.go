package resolver

import (
	"os"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Pass-scoped hot lookup cache.
//
// A full ResolveAll pass pages the pending frontier and re-hydrates its
// lookup caches from the store on every page: source nodes by ID and
// repository/language-scoped name-candidate groups. Those page caches are
// deliberately dropped between pages to bound memory — but the SAME hot IDs
// and names recur across pages (a file's edges span pages; `fmt`, `len`, and
// every popular helper appear on nearly all of them), and the cross-package
// guard then re-reads the very endpoints the compute loop just hydrated.
// On a large store whose pages no longer sit in the OS cache each of those
// repeats is a disk read, and the pass decays page over page.
//
// This cache retains, for the lifetime of ONE resolver pass, the results of
// exactly two read shapes that are immutable while the pass holds the
// resolve mutex:
//
//   - node rows by ID (positive hits only) — the resolver rewrites edges,
//     never node rows, during a pass;
//   - repository-scoped name-group candidate lists, including negatives —
//     repository definition nodes are only created by parsing, which cannot
//     run while the pass holds the mutex. Extern/AllRepos groups are NOT
//     cached: extern candidate nodes can be materialised mid-pass.
//
// An interleaving writer (a single-file edit during a chunk yield) advances
// the store mutation revision; the existing refreshAfterInterleave hook then
// flushes this cache along with the page caches, so cross-page retention
// never outlives the immutability argument. The cache is also flushed before
// the tail attribution passes (they materialise builtin/external nodes) and
// discarded when the pass returns.
//
// Memory is bounded by two-generation rotation: entries land in the current
// generation, and when its approximate byte size crosses half the budget the
// previous generation is dropped wholesale. Retained bytes therefore stay
// under the budget while the recently-hot half survives rotation.
const (
	defaultResolveHotCacheMB = 384
	resolveHotCacheEnvSize   = "GORTEX_RESOLVE_HOTCACHE_MB"
	resolveHotCacheEnvSwitch = "GORTEX_RESOLVE_HOTCACHE"
)

func resolveHotCacheEnabled() bool {
	v := os.Getenv(resolveHotCacheEnvSwitch)
	return v != "0" && !strings.EqualFold(v, "false")
}

func resolveHotCacheBudgetBytes() int64 {
	mb := int64(defaultResolveHotCacheMB)
	if v := os.Getenv(resolveHotCacheEnvSize); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			mb = n
		}
	}
	return mb << 20
}

// approxResolveNodeBytes estimates a cached node's retained footprint. It
// intentionally over-counts (fixed struct + string headers + a per-meta-entry
// charge) so the budget errs toward evicting early.
func approxResolveNodeBytes(n *graph.Node) int64 {
	if n == nil {
		return 16
	}
	size := int64(256)
	size += int64(len(n.ID) + len(n.Name) + len(n.QualName) + len(n.FilePath) + len(n.Language) + len(n.RepoPrefix) + len(n.WorkspaceID) + len(n.ProjectID))
	size += int64(len(n.Meta)) * 96
	return size
}

type hotNodeGen struct {
	nodes map[string]*graph.Node
	names map[string][]*graph.Node
	bytes int64
}

func newHotNodeGen(pendingHint int) *hotNodeGen {
	if pendingHint <= 0 {
		return &hotNodeGen{}
	}
	nameHint := min(pendingHint, 4096)
	nodeHint := 4096
	if pendingHint <= 2048 {
		nodeHint = pendingHint * 2
	}
	return &hotNodeGen{
		nodes: make(map[string]*graph.Node, nodeHint),
		names: make(map[string][]*graph.Node, nameHint),
	}
}

// resolveHotCache is used only while the owning Resolver holds its resolve
// mutex; it needs no locking of its own.
type resolveHotCache struct {
	cur, prev   *hotNodeGen
	budget      int64
	pendingHint int

	nodeHits, nodeMisses int64
	nameHits, nameMisses int64
}

func newResolveHotCache(budget int64, pendingHint int) *resolveHotCache {
	if budget <= 0 {
		budget = int64(defaultResolveHotCacheMB) << 20
	}
	return &resolveHotCache{
		cur: newHotNodeGen(pendingHint), budget: budget, pendingHint: pendingHint,
	}
}

func (c *resolveHotCache) rotateIfNeeded() {
	if c.cur.bytes < c.budget/2 {
		return
	}
	c.prev = c.cur
	c.cur = newHotNodeGen(c.pendingHint)
}

func (c *resolveHotCache) flush() {
	if c == nil {
		return
	}
	c.cur = newHotNodeGen(c.pendingHint)
	c.prev = nil
}

func (c *resolveHotCache) getNode(id string) (*graph.Node, bool) {
	if n, ok := c.cur.nodes[id]; ok {
		c.nodeHits++
		return n, true
	}
	if c.prev != nil {
		if n, ok := c.prev.nodes[id]; ok {
			// Promote so a hot entry survives the next rotation.
			if c.cur.nodes == nil {
				c.cur.nodes = make(map[string]*graph.Node)
			}
			c.cur.nodes[id] = n
			c.cur.bytes += approxResolveNodeBytes(n)
			c.nodeHits++
			return n, true
		}
	}
	c.nodeMisses++
	return nil, false
}

// peekNode returns a cached node without updating hit/miss counters or mutating
// rotation state. Used for read-only metadata lookups (such as RepoPrefix) on
// serial apply paths.
func (c *resolveHotCache) peekNode(id string) (*graph.Node, bool) {
	if c == nil || id == "" {
		return nil, false
	}
	if n, ok := c.cur.nodes[id]; ok {
		return n, true
	}
	if c.prev != nil {
		if n, ok := c.prev.nodes[id]; ok {
			return n, true
		}
	}
	return nil, false
}

// putNode caches a positive node row. Negatives stay page-local: the caller's
// authoritative-negative machinery already bounds their lifetime.
func (c *resolveHotCache) putNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	if _, exists := c.cur.nodes[n.ID]; exists {
		return
	}
	if c.cur.nodes == nil {
		c.cur.nodes = make(map[string]*graph.Node)
	}
	c.cur.nodes[n.ID] = n
	c.cur.bytes += approxResolveNodeBytes(n)
	c.rotateIfNeeded()
}

func hotNameKey(repo, languageKey, name string) string {
	return repo + "\x00" + languageKey + "\x00" + name
}

// getNames returns a cached repository-scoped candidate list. The second
// return distinguishes a cached negative (nil, true) from a miss (nil, false).
func (c *resolveHotCache) getNames(key string) ([]*graph.Node, bool) {
	if hits, ok := c.cur.names[key]; ok {
		c.nameHits++
		return hits, true
	}
	if c.prev != nil {
		if hits, ok := c.prev.names[key]; ok {
			if c.cur.names == nil {
				c.cur.names = make(map[string][]*graph.Node)
			}
			c.cur.names[key] = hits
			for _, n := range hits {
				c.cur.bytes += approxResolveNodeBytes(n)
			}
			c.nameHits++
			return hits, true
		}
	}
	c.nameMisses++
	return nil, false
}

// putNames caches one repository-scoped name group, including negatives —
// repository definition nodes cannot be created while the pass holds the
// resolve mutex, so an empty candidate list is as stable as a full one.
func (c *resolveHotCache) putNames(key string, hits []*graph.Node) {
	if _, exists := c.cur.names[key]; exists {
		return
	}
	if c.cur.names == nil {
		c.cur.names = make(map[string][]*graph.Node)
	}
	c.cur.names[key] = hits
	c.cur.bytes += int64(len(key)) + 48
	for _, n := range hits {
		c.cur.bytes += approxResolveNodeBytes(n)
	}
	c.rotateIfNeeded()
}

// cachedParallelGetNodesByIDs answers as many IDs as possible from the hot
// cache and hydrates only the misses from the store. The returned map has the
// same shape as parallelGetNodesByIDs: positives only, missing IDs absent.
func (r *Resolver) cachedParallelGetNodesByIDs(ids []string) map[string]*graph.Node {
	cache := r.hotCache
	if cache == nil {
		return r.parallelGetNodesByIDs(ids)
	}
	out := make(map[string]*graph.Node, len(ids))
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := cache.getNode(id); ok {
			out[id] = n
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fetched := r.parallelGetNodesByIDs(missing)
		for id, n := range fetched {
			if n == nil {
				continue
			}
			out[id] = n
			cache.putNode(n)
		}
	}
	return out
}
