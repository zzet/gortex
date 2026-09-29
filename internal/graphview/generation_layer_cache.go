package graphview

import (
	"container/list"
	"context"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// defaultLayerCacheEntries bounds how many generations' mask sets one
// Materializer keeps between requests. A routed checkout composes its
// dedicated ancestry (at most MaxGenerationAncestryDepth levels) plus its
// dirty chain (at most MaxDirtyChainDepth), so this holds the whole stack of a
// few concurrently read checkouts.
const defaultLayerCacheEntries = 96

// defaultLayerCacheWeight bounds the summed mask rows (file masks, identity
// masks, edge-source markers and detached summaries) the cache retains. A full
// dedicated root carries tens of thousands; the bound keeps the retained set
// to a few tens of megabytes whatever the shape of the stacks.
const defaultLayerCacheWeight = 2_000_000

// layerCacheKey names one generation's immutable mask set. The catalog row's
// creation and publication stamps are part of it: a generation's masks never
// change once it is published, but an id is not a promise across a catalog
// that was rebuilt underneath the process, and the stamps make a reused id a
// different key rather than a stale hit.
type layerCacheKey struct {
	generation  int64
	createdAt   int64
	publishedAt int64
}

// layerCacheEntry is one cached or in-flight mask load. ready closes when the
// load settles; masks is nil when it failed, and a waiter then loads under its
// own request context instead of inheriting another request's cancellation.
type layerCacheEntry struct {
	key    layerCacheKey
	ready  chan struct{}
	masks  *GenerationLayer
	weight int
	elem   *list.Element
}

// generationLayerCache keeps the mask half of GenerationLayer per published
// generation so a view over an ancestry chain does not re-read every
// generation's masks per request.
//
// Only the immutable, construction-time part is shared: covered paths,
// context paths, tombstones, edge-source markers and detached identity
// summaries. Every open still gets a fresh GenerationLayer with its own handle,
// its own point-read memo and its own failure scope, so nothing a request
// memoizes outlives that request's view, and servableGeneration still runs on
// every open (a generation that stopped being servable is refused and dropped
// from the cache before its masks could be reused).
type generationLayerCache struct {
	mu        sync.Mutex
	entries   map[layerCacheKey]*layerCacheEntry
	lru       *list.List // front = most recently used; settled entries only
	weight    int
	maxCount  int
	maxWeight int
	hits      int64
	misses    int64
}

func newGenerationLayerCache(maxCount, maxWeight int) *generationLayerCache {
	return &generationLayerCache{
		entries:   make(map[layerCacheKey]*layerCacheEntry),
		lru:       list.New(),
		maxCount:  maxCount,
		maxWeight: maxWeight,
	}
}

// layerMaskWeight is the retention cost of one mask set, in rows.
func layerMaskWeight(l *GenerationLayer) int {
	return len(l.covered) + len(l.contextPaths) + len(l.removed) + len(l.edgeSources) + len(l.detachedNodes) + 1
}

// open returns a fresh layer over handle whose masks come from the cache,
// loading them with load on a miss. Concurrent misses on one key share a
// single load.
func (c *generationLayerCache) open(
	ctx context.Context,
	key layerCacheKey,
	handle *store_sqlite.Store,
	load func(context.Context, *store_sqlite.Store) (*GenerationLayer, error),
) (*GenerationLayer, error) {
	for {
		c.mu.Lock()
		entry, found := c.entries[key]
		if found {
			select {
			case <-entry.ready:
				if entry.masks != nil {
					c.hits++
					c.lru.MoveToFront(entry.elem)
					masks := entry.masks
					c.mu.Unlock()
					return masks.withHandle(handle), nil
				}
				// A settled failure is never kept; the loader removed it.
				c.mu.Unlock()
				continue
			default:
			}
			c.mu.Unlock()
			select {
			case <-entry.ready:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.misses++
		entry = &layerCacheEntry{key: key, ready: make(chan struct{})}
		c.entries[key] = entry
		c.mu.Unlock()

		layer, err := load(ctx, handle)

		c.mu.Lock()
		if err != nil || layer == nil {
			delete(c.entries, key)
			close(entry.ready)
			c.mu.Unlock()
			return nil, err
		}
		entry.masks = layer.masksOnly()
		entry.weight = layerMaskWeight(layer)
		if c.entries[key] == entry {
			entry.elem = c.lru.PushFront(entry)
			c.weight += entry.weight
			c.evictLocked()
		}
		close(entry.ready)
		c.mu.Unlock()
		return layer, nil
	}
}

// evictLocked drops least recently used settled entries until both bounds
// hold. The entry just inserted is at the front and survives unless it alone
// exceeds the weight bound, in which case it is not retained at all.
func (c *generationLayerCache) evictLocked() {
	for c.lru.Len() > 0 && (c.lru.Len() > c.maxCount || c.weight > c.maxWeight) {
		back := c.lru.Back()
		entry := back.Value.(*layerCacheEntry)
		c.lru.Remove(back)
		c.weight -= entry.weight
		delete(c.entries, entry.key)
	}
}

// forget drops every cached mask set of generation, whatever its stamps.
func (c *generationLayerCache) forget(generation int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if key.generation != generation || entry.elem == nil {
			continue
		}
		c.lru.Remove(entry.elem)
		c.weight -= entry.weight
		delete(c.entries, key)
	}
}

// stats reports hit and miss counts and the retained entry count.
func (c *generationLayerCache) stats() (hits, misses int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.lru.Len()
}

// masksOnly returns a layer that carries only this layer's construction-time
// mask state: no handle and no request memo. It is the cache's template.
func (l *GenerationLayer) masksOnly() *GenerationLayer {
	return &GenerationLayer{
		covered:             l.covered,
		paths:               l.paths,
		contextPaths:        l.contextPaths,
		contextList:         l.contextList,
		removed:             l.removed,
		removedID:           l.removedID,
		edgeSources:         l.edgeSources,
		detachedIDs:         l.detachedIDs,
		detachedNodes:       l.detachedNodes,
		detachedFileIndexes: l.detachedFileIndexes,
		detachedPaths:       l.detachedPaths,
		detachedRepos:       l.detachedRepos,
	}
}

// withHandle builds a fresh layer over handle sharing this template's
// read-only mask state. The maps and slices it shares are never written after
// NewGenerationLayerContext returns; every per-request cache starts empty.
func (l *GenerationLayer) withHandle(handle *store_sqlite.Store) *GenerationLayer {
	out := l.masksOnly()
	out.handle = handle
	out.nodeByID = make(map[string]*graph.Node)
	out.fileNodes = make(map[string][]*graph.Node)
	return out
}

// layerCacheFor returns the Materializer's cache, creating it on first use.
func (m *Materializer) layerCacheFor() *generationLayerCache {
	m.layerCacheOnce.Do(func() {
		m.layerCache = newGenerationLayerCache(defaultLayerCacheEntries, defaultLayerCacheWeight)
	})
	return m.layerCache
}

// ForgetGeneration drops any cached masks of generation. Retirement calls it
// so a swept generation's masks are not retained; correctness never depends on
// it, because an open re-checks servability and the key carries the catalog
// stamps.
func (m *Materializer) ForgetGeneration(generation int64) {
	if m == nil {
		return
	}
	m.layerCacheFor().forget(generation)
}

// LayerCacheStats reports the generation-layer cache's hits, misses and
// retained entries.
func (m *Materializer) LayerCacheStats() (hits, misses int64, entries int) {
	if m == nil {
		return 0, 0, 0
	}
	return m.layerCacheFor().stats()
}
