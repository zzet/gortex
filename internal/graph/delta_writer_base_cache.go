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
