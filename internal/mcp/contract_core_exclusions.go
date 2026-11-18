package mcp

import (
	"context"
	"math"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// contractCoreOwnedKinds are the endpoint kinds whose adjacency belongs to the
// contract analysis rather than to core.
var contractCoreOwnedKinds = []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey}

const (
	// contractCoreExclusionLimit bounds one cached identity set. A mid-sized
	// workspace holds a few hundred contract nodes per generation (tens of
	// kilobytes of identities); a set at the bound is about 2 MB with long
	// config-key identities, and a corpus past it (tens of thousands of config
	// keys) keeps its per-batch lookups instead of pinning more per reader.
	contractCoreExclusionLimit = 1 << 14
	// contractCoreExclusionReaders bounds the readers kept at once: the
	// indexed corpus and the few generations requests read directly. An entry
	// holds its store core until it is evicted. With every reader's set at the
	// bound the cache holds contractCoreExclusionReaders such sets, about 16 MB
	// in all.
	contractCoreExclusionReaders = 8
)

// contractCoreExclusionKey names the physical rows a SQLite handle reads.
// Every handle over one store core and view generation, request-bound or not,
// answers the kind predicate alike, so they share one entry.
type contractCoreExclusionKey struct {
	core       any
	generation int64
}

type contractCoreExclusionEntry struct {
	revision uint64
	// ids is the owned identity set read at revision; nil until it is read.
	ids map[string]struct{}
	// building marks the one caller reading the set at revision; skip records
	// that the set is not readable at revision (past the bound, or the kind
	// index is dropped for a bulk load).
	building, skip bool
	used           uint64
}

// contractCoreExclusionCache shares each SQLite reader's contract-owned
// identity set across batches and requests while the reader's mutation
// revision is unchanged, so classifying an adjacency batch's endpoints issues
// no statement. Sets are read-only once published.
type contractCoreExclusionCache struct {
	limit int
	// read is the bounded identity read; tests observe it.
	read    func(*store_sqlite.Store, context.Context, []graph.NodeKind, int) ([]string, bool, error)
	mu      sync.Mutex
	clock   uint64
	entries map[contractCoreExclusionKey]*contractCoreExclusionEntry
}

// defaultContractCoreExclusions is shared by every request reader.
var defaultContractCoreExclusions = newContractCoreExclusionCache(contractCoreExclusionLimit)

// newContractCoreExclusionCache keeps sets of at most limit identities.
func newContractCoreExclusionCache(limit int) *contractCoreExclusionCache {
	return &contractCoreExclusionCache{limit: limit, entries: make(map[contractCoreExclusionKey]*contractCoreExclusionEntry)}
}

// contractCoreExclusionSource reports the SQLite handle reader is, its key and
// its mutation revision. Any other reader (an overlay, a scoped or stacked
// view) keeps per-batch lookups, which apply its own masks; so does a handle
// without a core.
func contractCoreExclusionSource(reader graph.Reader) (*store_sqlite.Store, contractCoreExclusionKey, uint64, bool) {
	store, ok := reader.(*store_sqlite.Store)
	if !ok || store == nil {
		return nil, contractCoreExclusionKey{}, 0, false
	}
	core := store.SymbolSearchCoreKey()
	if core == nil {
		return nil, contractCoreExclusionKey{}, 0, false
	}
	revision, ok := readerSearchMutationRevision(store)
	if !ok {
		return nil, contractCoreExclusionKey{}, 0, false
	}
	return store, contractCoreExclusionKey{core: core, generation: store.ViewGeneration()}, revision, true
}

// owned returns reader's contract-owned identity set when one is current at
// its revision, and whether answering read the store. A nil set leaves the
// batch to a per-ID lookup.
//
// A revision's set is read on its second sighting, so a reader racing a writer
// keeps per-batch lookups instead of rereading the set after every write, and
// by one caller while the others keep per-batch lookups. The set is kept only
// when the revision held across the read; the reading caller uses it either
// way, since it answers exactly like a per-ID lookup made at that moment.
func (c *contractCoreExclusionCache) owned(ctx context.Context, reader graph.Reader) (map[string]struct{}, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	store, key, revision, ok := contractCoreExclusionSource(reader)
	if !ok {
		return nil, false, nil
	}
	ids, read := c.claim(key, revision)
	if !read {
		return ids, false, nil
	}
	readSet := (*store_sqlite.Store).NodeIDsByKindsBoundedContext
	if c.read != nil {
		readSet = c.read
	}
	list, complete, err := readSet(store, ctx, contractCoreOwnedKinds, c.limit)
	if err != nil || !complete {
		c.settle(key, revision, nil, err == nil)
		return nil, true, err
	}
	set := make(map[string]struct{}, len(list))
	for _, id := range list {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	if after, ok := readerSearchMutationRevision(store); ok && after == revision {
		c.settle(key, revision, set, false)
	} else {
		c.settle(key, revision, nil, false)
	}
	return set, true, nil
}

// claim returns the set cached for key at revision, or reports that the
// caller should read it now.
func (c *contractCoreExclusionCache) claim(key contractCoreExclusionKey, revision uint64) (map[string]struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock++
	entry := c.entries[key]
	switch {
	case entry == nil:
		c.evictLocked()
		c.entries[key] = &contractCoreExclusionEntry{revision: revision, used: c.clock}
		return nil, false
	case revision < entry.revision:
		// Read before a newer revision was seen: the entry is not for it.
		return nil, false
	case revision > entry.revision:
		*entry = contractCoreExclusionEntry{revision: revision, used: c.clock}
		return nil, false
	}
	entry.used = c.clock
	if entry.ids != nil || entry.building || entry.skip {
		return entry.ids, false
	}
	entry.building = true
	return nil, true
}

// settle ends the read claimed at revision, keeping ids, or recording with
// skip that the set is not readable at revision. With neither, the next
// sighting reads again.
func (c *contractCoreExclusionCache) settle(key contractCoreExclusionKey, revision uint64, ids map[string]struct{}, skip bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil || entry.revision != revision {
		return
	}
	entry.building = false
	entry.ids = ids
	entry.skip = skip
}

// evictLocked drops the least recently used reader when the cache is full.
func (c *contractCoreExclusionCache) evictLocked() {
	if len(c.entries) < contractCoreExclusionReaders {
		return
	}
	var oldest contractCoreExclusionKey
	oldestUsed := uint64(math.MaxUint64)
	for key, entry := range c.entries {
		if entry.used < oldestUsed {
			oldest, oldestUsed = key, entry.used
		}
	}
	delete(c.entries, oldest)
}
