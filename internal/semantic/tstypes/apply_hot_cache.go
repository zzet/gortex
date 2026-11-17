package tstypes

import (
	"os"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// applyHotCache is the pass-scoped read-through cache shared by the per-page
// appliers of one streamed apply pass (applyStagedFacts). The streamed path
// creates a fresh applier per 32-file page across four phases, and every
// applier re-hydrated the same repo-wide type universe — name candidates and
// the inheritance frontier overlap almost completely between pages (measured:
// the per-page re-hydration was 48–62% of whole-process CPU across an ~18
// minute enrichment window). The cache dedupes those store reads while
// keeping the paging path's bounded-memory contract via a two-generation
// byte-budget rotation.
//
// Safety model, per entry class:
//   - nodes: positive-only, keyed by ID, shared POINTERS. The applier stamps
//     Meta on the node objects it holds and persists them via AddBatch, so
//     sharing pointers is what preserves same-pass stamp visibility across
//     pages and phases (a copying cache would hide earlier pages' stamps).
//     Apply never creates or deletes nodes, so residency is stable.
//   - name groups: the RAW FindNodesByNamesInRepoLanguages result per
//     (repo, name), including empty (negative) groups. Group membership can
//     only change if nodes are created mid-pass, which apply never does.
//   - adjacency: out/in edge slices per node ID, including empty results,
//     holding only the kinds the pass's appliers load (one provider, one
//     kind set per pass). Outgoing calls are never among them: appliers
//     load those per page for callers only (applier.loadCalls), so an out
//     entry is never served to a calls reader. An entry the applier mutates
//     in the store is dropped (forgetOut / forgetIn) so later pages re-read
//     it. Valid only
//     WITHIN a phase: the supers phase synthesizes inheritance
//     edges that later phases must observe (a call may resolve through an
//     extends edge another file's facts just synthesized), so the driver
//     flushes adjacency at every phase boundary via flushAdjacency.
//
// Not safe for concurrent use: one streamed apply pass runs its pages
// serially and owns its cache exclusively.
type applyHotCache struct {
	budget int64
	cur    *applyHotGen
	prev   *applyHotGen

	nodeHits, nodeMisses int64
	nameHits, nameMisses int64
	adjHits, adjMisses   int64
	fileHits, fileMisses int64
}

type applyHotGen struct {
	nodes map[string]*graph.Node
	names map[typeCandidateKey][]*graph.Node
	files map[string][]*graph.Node
	out   map[string][]*graph.Edge
	in    map[string][]*graph.Edge
	bytes int64
}

func newApplyHotGen() *applyHotGen {
	return &applyHotGen{
		nodes: make(map[string]*graph.Node),
		names: make(map[typeCandidateKey][]*graph.Node),
		files: make(map[string][]*graph.Node),
		out:   make(map[string][]*graph.Edge),
		in:    make(map[string][]*graph.Edge),
	}
}

const defaultApplyHotCacheBytes = 256 << 20

// applyHotCacheBudget reads the operator override: GORTEX_TSTYPES_HOTCACHE=0
// (or "off"/"false") disables the cache, GORTEX_TSTYPES_HOTCACHE_MB replaces
// the default 256 MiB budget.
func applyHotCacheBudget() int64 {
	v := strings.TrimSpace(os.Getenv("GORTEX_TSTYPES_HOTCACHE"))
	if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
		return 0
	}
	if mb := strings.TrimSpace(os.Getenv("GORTEX_TSTYPES_HOTCACHE_MB")); mb != "" {
		if n, err := strconv.Atoi(mb); err == nil && n > 0 {
			return int64(n) << 20
		}
	}
	return defaultApplyHotCacheBytes
}

// newApplyHotCache returns a cache with the given byte budget, or nil when
// the budget is non-positive (every method is nil-safe, so a nil cache is
// simply a pass with caching off).
func newApplyHotCache(budget int64) *applyHotCache {
	if budget <= 0 {
		return nil
	}
	return &applyHotCache{budget: budget, cur: newApplyHotGen(), prev: newApplyHotGen()}
}

// rotate keeps each generation under half the budget so retained bytes stay
// under budget plus one entry of overshoot; the previous generation keeps the
// hottest recent entries readable across the swap.
func (c *applyHotCache) rotateIfNeeded() {
	if c.cur.bytes < c.budget/2 {
		return
	}
	c.prev = c.cur
	c.cur = newApplyHotGen()
}

func applyHotNodeBytes(n *graph.Node) int64 {
	if n == nil {
		return 0
	}
	return int64(len(n.ID)+len(n.Name)+len(n.FilePath)+len(n.Language)+len(n.RepoPrefix)) + 96
}

func applyHotEdgesBytes(edges []*graph.Edge) int64 {
	total := int64(16)
	for _, e := range edges {
		if e == nil {
			continue
		}
		total += int64(len(e.From)+len(e.To)+len(e.FilePath)) + 64
	}
	return total
}

func (c *applyHotCache) getNode(id string) (*graph.Node, bool) {
	if c == nil {
		return nil, false
	}
	if n, ok := c.cur.nodes[id]; ok {
		c.nodeHits++
		return n, true
	}
	if n, ok := c.prev.nodes[id]; ok {
		// Promote so a rotation does not evict a hot entry.
		c.putNode(n)
		c.nodeHits++
		return n, true
	}
	c.nodeMisses++
	return nil, false
}

func (c *applyHotCache) putNode(n *graph.Node) {
	if c == nil || n == nil || n.ID == "" {
		return
	}
	if _, ok := c.cur.nodes[n.ID]; ok {
		return
	}
	c.rotateIfNeeded()
	c.cur.nodes[n.ID] = n
	c.cur.bytes += applyHotNodeBytes(n)
}

func (c *applyHotCache) getNames(key typeCandidateKey) ([]*graph.Node, bool) {
	if c == nil {
		return nil, false
	}
	if nodes, ok := c.cur.names[key]; ok {
		c.nameHits++
		return nodes, true
	}
	if nodes, ok := c.prev.names[key]; ok {
		c.putNames(key, nodes)
		c.nameHits++
		return nodes, true
	}
	c.nameMisses++
	return nil, false
}

func (c *applyHotCache) putNames(key typeCandidateKey, nodes []*graph.Node) {
	if c == nil {
		return
	}
	if _, ok := c.cur.names[key]; ok {
		return
	}
	c.rotateIfNeeded()
	bytes := int64(len(key.repoPrefix)+len(key.name)) + 48
	for _, n := range nodes {
		bytes += applyHotNodeBytes(n)
	}
	c.cur.names[key] = nodes
	c.cur.bytes += bytes
}

// getFiles / putFiles cache the per-file node projection preloadBounded
// hydrates each page from — the one store read the node/name/adjacency
// funnels never covered, so every page applier of all four phases re-fetched
// near-identical file sets straight from the store (the round-7 whale).
// Entries are the same shared node pointers as the nodes funnel; empty
// (negative) groups ARE cached — apply never creates nodes, so a file's
// emptiness is as stable as its membership.
func (c *applyHotCache) getFiles(file string) ([]*graph.Node, bool) {
	if c == nil {
		return nil, false
	}
	if nodes, ok := c.cur.files[file]; ok {
		c.fileHits++
		return nodes, true
	}
	if nodes, ok := c.prev.files[file]; ok {
		c.putFiles(file, nodes)
		c.fileHits++
		return nodes, true
	}
	c.fileMisses++
	return nil, false
}

func (c *applyHotCache) putFiles(file string, nodes []*graph.Node) {
	if c == nil || file == "" {
		return
	}
	if _, ok := c.cur.files[file]; ok {
		return
	}
	c.rotateIfNeeded()
	bytes := int64(len(file)) + 48
	for _, n := range nodes {
		bytes += applyHotNodeBytes(n)
	}
	c.cur.files[file] = nodes
	c.cur.bytes += bytes
}

func (c *applyHotCache) getOut(id string) ([]*graph.Edge, bool) {
	if c == nil {
		return nil, false
	}
	if edges, ok := c.cur.out[id]; ok {
		c.adjHits++
		return edges, true
	}
	if edges, ok := c.prev.out[id]; ok {
		c.putOut(id, edges)
		c.adjHits++
		return edges, true
	}
	c.adjMisses++
	return nil, false
}

func (c *applyHotCache) putOut(id string, edges []*graph.Edge) {
	if c == nil || id == "" {
		return
	}
	if _, ok := c.cur.out[id]; ok {
		return
	}
	c.rotateIfNeeded()
	c.cur.out[id] = edges
	c.cur.bytes += int64(len(id)) + applyHotEdgesBytes(edges)
}

func (c *applyHotCache) getIn(id string) ([]*graph.Edge, bool) {
	if c == nil {
		return nil, false
	}
	if edges, ok := c.cur.in[id]; ok {
		c.adjHits++
		return edges, true
	}
	if edges, ok := c.prev.in[id]; ok {
		c.putIn(id, edges)
		c.adjHits++
		return edges, true
	}
	c.adjMisses++
	return nil, false
}

func (c *applyHotCache) putIn(id string, edges []*graph.Edge) {
	if c == nil || id == "" {
		return
	}
	if _, ok := c.cur.in[id]; ok {
		return
	}
	c.rotateIfNeeded()
	c.cur.in[id] = edges
	c.cur.bytes += int64(len(id)) + applyHotEdgesBytes(edges)
}

// forgetOut drops id's cached out-adjacency from both generations. The
// applier calls it whenever it changes that list in the store, so a later
// page re-reads it instead of a pre-mutation snapshot.
func (c *applyHotCache) forgetOut(id string) {
	if c == nil {
		return
	}
	for _, gen := range []*applyHotGen{c.cur, c.prev} {
		gen.forget(gen.out, id)
	}
}

// forgetIn is forgetOut's incoming twin.
func (c *applyHotCache) forgetIn(id string) {
	if c == nil {
		return
	}
	for _, gen := range []*applyHotGen{c.cur, c.prev} {
		gen.forget(gen.in, id)
	}
}

func (g *applyHotGen) forget(adjacency map[string][]*graph.Edge, id string) {
	edges, ok := adjacency[id]
	if !ok {
		return
	}
	delete(adjacency, id)
	// A retarget may have changed an edge's strings since the entry was
	// charged, so the refund is approximate; never let it go negative.
	g.bytes -= int64(len(id)) + applyHotEdgesBytes(edges)
	if g.bytes < 0 {
		g.bytes = 0
	}
}

// applyHotCacheStats reports the funnel counters for the pass-end log line —
// they were previously write-only, which left this cache invisible to the
// completion log the resolver-side caches already have.
type applyHotCacheStats struct {
	NodeHits, NodeMisses int64
	NameHits, NameMisses int64
	AdjHits, AdjMisses   int64
	FileHits, FileMisses int64
}

func (c *applyHotCache) statsSnapshot() applyHotCacheStats {
	if c == nil {
		return applyHotCacheStats{}
	}
	return applyHotCacheStats{
		NodeHits: c.nodeHits, NodeMisses: c.nodeMisses,
		NameHits: c.nameHits, NameMisses: c.nameMisses,
		AdjHits: c.adjHits, AdjMisses: c.adjMisses,
		FileHits: c.fileHits, FileMisses: c.fileMisses,
	}
}

// flushAdjacency drops every cached adjacency entry in both generations. The
// driver calls it at phase boundaries: the supers phase synthesizes
// inheritance edges, and every later phase's frontier walk must observe them
// rather than a pre-synthesis snapshot. Node and name entries survive — apply
// never creates nodes, and the shared node pointers already carry every
// same-pass Meta stamp.
func (c *applyHotCache) flushAdjacency() {
	if c == nil {
		return
	}
	for _, gen := range []*applyHotGen{c.cur, c.prev} {
		for id, edges := range gen.out {
			gen.bytes -= int64(len(id)) + applyHotEdgesBytes(edges)
		}
		for id, edges := range gen.in {
			gen.bytes -= int64(len(id)) + applyHotEdgesBytes(edges)
		}
		if gen.bytes < 0 {
			gen.bytes = 0
		}
		gen.out = make(map[string][]*graph.Edge)
		gen.in = make(map[string][]*graph.Edge)
	}
}
