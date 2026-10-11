package graph

import (
	"sort"
	"strings"
	"sync"
)

// BaseProjectionCache memoizes answers the store at the bottom of a delta's
// stack gives to the resolver's per-pass indexes: each caller file's direct
// import targets (ProjectImportAdjacency) and the file identities of a set of
// repositories (FileNodeIdentitiesSeq, the directory index). The bottom store
// of a delta over an immutable generation stack answers them identically for
// every delta over that stack, so the indexer keeps one cache per stack and
// hands it to each delta (SetBaseProjectionCache). Only the bottom store's
// raw answers are kept: the layers above it and the delta's own rows are
// composed over them live, exactly as without the cache.
type BaseProjectionCache struct {
	mu        sync.Mutex
	imports   map[string][]string
	fileIDs   map[string][]FileNodeIdentity
	hits      int
	misses    int
	importHit int
	importMis int
	// stackImports is the stack-level answer (the layers below the delta
	// composed over the bottom store), per caller path.
	stackImports   map[string]stackImportEntry
	stackImportHit int
	stackImportMis int
	// stackFileIDs is the stack-level directory-index answer per repository
	// set.
	stackFileIDs map[string][]FileNodeIdentity
	stackFileHit int
	stackFileMis int
	// stackNodes is the stack-level per-file node answer of one kind set.
	stackNodes   map[string][]*Node
	stackNodeHit int
	stackNodeMis int
	// stackNameRows is the stack-level name answer per scope and name.
	stackNameRows map[string][]*Node
	stackNameHit  int
	stackNameMis  int
	// stackFacts is the stack's reference facts per repository and target.
	stackFacts   map[string][]RefFact
	stackFactHit int
	stackFactMis int
	// stackPlaces is the stack's node placement per identity (nil: absent).
	stackPlaces   map[string]*NodePlacement
	stackPlaceHit int
	stackPlaceMis int
	// stackRepos is the stack's repository listing.
	stackRepos     []string
	stackReposRead bool
	// stackLayerAdj is each layer below the delta's adjacency by identity
	// (delta_writer_stack_layer_adjacency.go).
	stackLayerAdj    map[stackLayerAdjacencyKey]map[string][]*Edge
	stackLayerAdjHit int
	stackLayerAdjMis int
	// stackInIDs is the incoming identities below the delta per target
	// identity (delta_writer_in_identities.go).
	stackInIDs   map[string][]EdgeIdentity
	stackInIDHit int
	stackInIDMis int
	// stackPathNodes is the file nodes below the delta per path
	// (delta_writer_compose.go stackComposedFileNodes).
	stackPathNodes map[string][]*Node
	stackPathHit   int
	stackPathMis   int
	// stackBaseAdj is the bottom store's adjacency by identity and direction
	// (delta_writer_stack_base_adjacency.go).
	stackBaseAdj    [2]map[string][]*Edge
	stackBaseAdjHit int
	stackBaseAdjMis int
	stackBaseAdjLen int
	// stackBaseScoped is the bottom store's scoped edge rows per (repositories,
	// files, kinds) scope (delta_writer_stack_base_scoped.go).
	stackBaseScoped    map[string][]ScopedEdgeRow
	stackBaseScopedHit int
	stackBaseScopedMis int
	stackBaseScopedLen int
	// stackRecorded is the view below's recorded edges per path
	// (delta_writer_stack_recorded.go).
	stackRecorded    map[string][]*Edge
	stackRecordedHit int
	stackRecordedMis int
	stackRecordedLen int
}

// NewBaseProjectionCache returns an empty cache.
func NewBaseProjectionCache() *BaseProjectionCache {
	return &BaseProjectionCache{imports: make(map[string][]string), fileIDs: make(map[string][]FileNodeIdentity)}
}

// Stats reports the directory-index and import-adjacency hits and misses so
// far.
func (c *BaseProjectionCache) Stats() (fileHits, fileMisses, importHits, importMisses int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.importHit, c.importMis
}

// importAdjacency answers paths from the cache, loading the missing ones in
// one call. complete=false from the load is passed through and caches nothing.
func (c *BaseProjectionCache) importAdjacency(paths []string, load func([]string) (map[string][]string, bool)) (map[string][]string, bool) {
	out := make(map[string][]string, len(paths))
	var missing []string
	c.mu.Lock()
	for _, p := range paths {
		if targets, ok := c.imports[p]; ok {
			if len(targets) > 0 {
				out[p] = targets
			}
			c.importHit++
			continue
		}
		missing = append(missing, p)
	}
	c.importMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, true
	}
	loaded, complete := load(missing)
	if !complete {
		return nil, false
	}
	c.mu.Lock()
	for _, p := range missing {
		targets := append([]string(nil), loaded[p]...)
		c.imports[p] = targets
		if len(targets) > 0 {
			out[p] = targets
		}
	}
	c.mu.Unlock()
	return out, true
}

// fileIdentities answers one repository set from the cache or loads it.
func (c *BaseProjectionCache) fileIdentities(repos []string, load func() []FileNodeIdentity) []FileNodeIdentity {
	key := append([]string(nil), repos...)
	sort.Strings(key)
	k := strings.Join(key, "\x00")
	c.mu.Lock()
	if rows, ok := c.fileIDs[k]; ok {
		c.hits++
		c.mu.Unlock()
		return rows
	}
	c.misses++
	c.mu.Unlock()
	rows := load()
	c.mu.Lock()
	c.fileIDs[k] = rows
	c.mu.Unlock()
	return rows
}

// SetBaseProjectionCache installs the cache of the delta's bottom store. The
// caller guarantees the bottom store is immutable for as long as the cache
// is shared (one cache per immutable generation stack).
func (dw *DeltaWriter) SetBaseProjectionCache(c *BaseProjectionCache) { dw.baseCache = c }

// stackImportEntry is one path's import targets as the stack below a delta
// composes them. filtered marks targets the bottom store answered and the
// layers' identity check filtered (the delta's own layer must filter them
// too); otherwise they are the composition's rows as read.
type stackImportEntry struct {
	targets  []string
	filtered bool
}

// stackImportAdjacency answers paths from the stack-level cache, loading the
// missing ones in one call. complete=false from the load caches nothing.
func (c *BaseProjectionCache) stackImportAdjacency(paths []string, load func([]string) (map[string]stackImportEntry, bool)) (map[string]stackImportEntry, bool) {
	out := make(map[string]stackImportEntry, len(paths))
	var missing []string
	c.mu.Lock()
	if c.stackImports == nil {
		c.stackImports = make(map[string]stackImportEntry)
	}
	for _, p := range paths {
		if entry, ok := c.stackImports[p]; ok {
			out[p] = entry
			c.stackImportHit++
			continue
		}
		missing = append(missing, p)
	}
	c.stackImportMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, true
	}
	loaded, complete := load(missing)
	if !complete {
		return nil, false
	}
	c.mu.Lock()
	for _, p := range missing {
		entry := loaded[p]
		entry.targets = append([]string(nil), entry.targets...)
		c.stackImports[p] = entry
		out[p] = entry
	}
	c.mu.Unlock()
	return out, true
}

// StackStats reports the stack-level import-adjacency hits and misses.
func (c *BaseProjectionCache) StackStats() (importHits, importMisses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackImportHit, c.stackImportMis
}

// stackFileIdentities answers one repository set's file identities as the
// layers below a delta compose them over the bottom store, or loads them.
func (c *BaseProjectionCache) stackFileIdentities(repos []string, load func() []FileNodeIdentity) []FileNodeIdentity {
	key := append([]string(nil), repos...)
	sort.Strings(key)
	k := strings.Join(key, "\x00")
	c.mu.Lock()
	if rows, ok := c.stackFileIDs[k]; ok {
		c.stackFileHit++
		c.mu.Unlock()
		return rows
	}
	c.stackFileMis++
	c.mu.Unlock()
	rows := load()
	c.mu.Lock()
	if c.stackFileIDs == nil {
		c.stackFileIDs = make(map[string][]FileNodeIdentity)
	}
	c.stackFileIDs[k] = rows
	c.mu.Unlock()
	return rows
}

// StackFileStats reports the stack-level directory-index hits and misses.
func (c *BaseProjectionCache) StackFileStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackFileHit, c.stackFileMis
}

// stackFileNodes answers files' nodes of one kind set as the layers below a
// delta compose them, loading the missing files in one call.
// A file the load leaves out of its answer is not kept, and is returned in
// uncached for the caller to read itself.
func (c *BaseProjectionCache) stackFileNodes(files []string, kindKey string, load func([]string) map[string][]*Node) (out map[string][]*Node, uncached []string) {
	out = make(map[string][]*Node, len(files))
	var missing []string
	c.mu.Lock()
	if c.stackNodes == nil {
		c.stackNodes = make(map[string][]*Node)
	}
	for _, f := range files {
		if nodes, ok := c.stackNodes[f+"\x00"+kindKey]; ok {
			out[f] = nodes
			c.stackNodeHit++
			continue
		}
		missing = append(missing, f)
	}
	c.stackNodeMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	loaded := load(missing)
	c.mu.Lock()
	for _, f := range missing {
		if _, ok := loaded[f]; !ok {
			uncached = append(uncached, f)
			continue
		}
		nodes := make([]*Node, 0, len(loaded[f]))
		for _, n := range loaded[f] {
			nodes = append(nodes, cloneDeltaNode(n))
		}
		c.stackNodes[f+"\x00"+kindKey] = nodes
		out[f] = nodes
	}
	c.mu.Unlock()
	return out, uncached
}

// StackNodeStats reports the stack-level per-file node hits and misses.
func (c *BaseProjectionCache) StackNodeStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackNodeHit, c.stackNodeMis
}

// stackNames answers names in one repository/language scope as the layers
// below a delta compose them, loading the missing names in one call. An
// absent name is kept as known-absent.
func (c *BaseProjectionCache) stackNames(scopeKey string, names []string, load func([]string) map[string][]*Node) map[string][]*Node {
	out := make(map[string][]*Node, len(names))
	var missing []string
	c.mu.Lock()
	if c.stackNameRows == nil {
		c.stackNameRows = make(map[string][]*Node)
	}
	for _, name := range names {
		if rows, ok := c.stackNameRows[scopeKey+"\x00"+name]; ok {
			out[name] = rows
			c.stackNameHit++
			continue
		}
		missing = append(missing, name)
	}
	c.stackNameMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	loaded := load(missing)
	c.mu.Lock()
	for _, name := range missing {
		rows := loaded[name]
		c.stackNameRows[scopeKey+"\x00"+name] = rows
		out[name] = rows
	}
	c.mu.Unlock()
	return out
}

// StackNameStats reports the stack-level name hits and misses.
func (c *BaseProjectionCache) StackNameStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackNameHit, c.stackNameMis
}

// stackRefFactsByTargets answers the stack's reference facts into targets,
// per target, loading the missing targets in one call. An error caches
// nothing.
func (c *BaseProjectionCache) stackRefFactsByTargets(repo string, targets []string, load func(string, []string) (map[string][]RefFact, error)) (map[string][]RefFact, error) {
	byTarget := make(map[string][]RefFact, len(targets))
	var missing []string
	c.mu.Lock()
	if c.stackFacts == nil {
		c.stackFacts = make(map[string][]RefFact)
	}
	for _, id := range targets {
		if facts, ok := c.stackFacts[repo+"\x00"+id]; ok {
			byTarget[id] = facts
			c.stackFactHit++
			continue
		}
		missing = append(missing, id)
	}
	c.stackFactMis += len(missing)
	c.mu.Unlock()
	if len(missing) > 0 {
		loaded, err := load(repo, missing)
		if err != nil {
			return nil, err
		}
		fresh := make(map[string][]RefFact, len(missing))
		for _, facts := range loaded {
			for _, fact := range facts {
				fresh[fact.ToID] = append(fresh[fact.ToID], fact)
			}
		}
		c.mu.Lock()
		for _, id := range missing {
			c.stackFacts[repo+"\x00"+id] = fresh[id]
			byTarget[id] = fresh[id]
		}
		c.mu.Unlock()
	}
	out := make(map[string][]RefFact)
	for _, id := range targets {
		for _, fact := range byTarget[id] {
			out[fact.FilePath] = append(out[fact.FilePath], fact)
		}
	}
	return out, nil
}

// StackRefFactStats reports the stack-level reference-fact hits and misses.
func (c *BaseProjectionCache) StackRefFactStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackFactHit, c.stackFactMis
}

// stackPlacements answers identities' placements as the layers below a delta
// compose them, loading the missing ones in one call. An absent identity is
// kept as known-absent.
func (c *BaseProjectionCache) stackPlacements(ids []string, load func([]string) map[string]NodePlacement) map[string]NodePlacement {
	out := make(map[string]NodePlacement, len(ids))
	var missing []string
	c.mu.Lock()
	if c.stackPlaces == nil {
		c.stackPlaces = make(map[string]*NodePlacement)
	}
	for _, id := range ids {
		if placement, ok := c.stackPlaces[id]; ok {
			if placement != nil {
				out[id] = *placement
			}
			c.stackPlaceHit++
			continue
		}
		missing = append(missing, id)
	}
	c.stackPlaceMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	loaded := load(missing)
	c.mu.Lock()
	for _, id := range missing {
		if placement, ok := loaded[id]; ok {
			kept := placement
			c.stackPlaces[id] = &kept
			out[id] = placement
		} else {
			c.stackPlaces[id] = nil
		}
	}
	c.mu.Unlock()
	return out
}

// StackPlacementStats reports the stack-level placement hits and misses.
func (c *BaseProjectionCache) StackPlacementStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackPlaceHit, c.stackPlaceMis
}

// stackRepoPrefixes answers the stack's repository listing, reading it once.
func (c *BaseProjectionCache) stackRepoPrefixes(load func() []string) []string {
	c.mu.Lock()
	if c.stackReposRead {
		repos := c.stackRepos
		c.mu.Unlock()
		return repos
	}
	c.mu.Unlock()
	repos := load()
	sort.Strings(repos)
	c.mu.Lock()
	c.stackRepos, c.stackReposRead = repos, true
	c.mu.Unlock()
	return repos
}

// stackFileNodesByPath answers the stack's per-path file nodes below the delta
// from the cache or loads the missing paths (a path with no nodes is kept
// empty).
func (c *BaseProjectionCache) stackFileNodesByPath(paths []string, load func([]string) map[string][]*Node) map[string][]*Node {
	out := make(map[string][]*Node, len(paths))
	var missing []string
	c.mu.Lock()
	if c.stackPathNodes == nil {
		c.stackPathNodes = make(map[string][]*Node)
	}
	for _, p := range paths {
		if nodes, ok := c.stackPathNodes[p]; ok {
			out[p] = nodes
			c.stackPathHit++
			continue
		}
		missing = append(missing, p)
	}
	c.stackPathMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	loaded := load(missing)
	c.mu.Lock()
	for _, p := range missing {
		nodes := loaded[p]
		if nodes == nil {
			nodes = []*Node{}
		}
		c.stackPathNodes[p] = nodes
		out[p] = nodes
	}
	c.mu.Unlock()
	return out
}

// StackFileNodeStats reports the stack-level per-path file-node hits and
// misses.
func (c *BaseProjectionCache) StackFileNodeStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackPathHit, c.stackPathMis
}
