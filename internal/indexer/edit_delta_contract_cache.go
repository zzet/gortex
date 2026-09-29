package indexer

import (
	"fmt"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// The contract registry a per-file delta starts from, kept across deltas.
//
// A delta's engine loads its repository's contract registry from the view
// below (ensureIncrementalContractRegistry): an indexed read of every
// provides / consumes edge and contract node of the repository, 170-310 ms per
// delta on a real repository. Successive deltas of one checkout stand on the
// same view below (every build is direct over the checkout's commit
// generation until the commit moves), and a view whose generations are all
// immutable reads the same rows every time: payload generations are sealed
// when published, and catalog generation IDs are never reused (AUTOINCREMENT).
// Generation zero is the one mutable generation, so a view that composes it
// (commitLayerBase.stack is nil) is never cached.
//
// The registry is loaded before the engine writes anything, so it is the
// view below's registry exactly; each delta gets its own copy, which its
// engine then edits.

// editDeltaContractCacheEntries bounds the registries kept: one per checkout
// commit a daemon is building over at once is the working set.
const editDeltaContractCacheEntries = 8

type editDeltaContractCacheEntry struct {
	key       string
	contracts []contracts.Contract
}

var editDeltaContractCache struct {
	sync.Mutex
	entries []editDeltaContractCacheEntry // most recent last
}

// editDeltaContractCacheKey is the identity of the registry a delta over base
// would load, false when base is not an immutable stack.
func editDeltaContractCacheKey(base graph.Reader, store any, repoPrefix, workspaceID, projectID string) (string, bool) {
	layer, ok := base.(commitLayerBase)
	if !ok || len(layer.stack) == 0 || repoPrefix == "" {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%p\x00%s\x00%s\x00%s", store, repoPrefix, workspaceID, projectID)
	for _, gen := range layer.stack {
		if gen <= 0 {
			return "", false
		}
		fmt.Fprintf(&b, "\x00%d", gen)
	}
	return b.String(), true
}

// seedEditDeltaContractRegistry installs the cached registry of base on idx,
// or loads it (from idx's view, before any write) and caches it. It reports
// whether the registry came from the cache.
func seedEditDeltaContractRegistry(idx *Indexer, key string) bool {
	editDeltaContractCache.Lock()
	for i := len(editDeltaContractCache.entries) - 1; i >= 0; i-- {
		entry := editDeltaContractCache.entries[i]
		if entry.key != key {
			continue
		}
		editDeltaContractCache.entries = append(append(editDeltaContractCache.entries[:i:i], editDeltaContractCache.entries[i+1:]...), entry)
		editDeltaContractCache.Unlock()
		idx.contractRegistry = registryOf(entry.contracts)
		return true
	}
	editDeltaContractCache.Unlock()

	loaded := idx.ensureIncrementalContractRegistry()
	snapshot := copyContracts(loaded.ByRepo(idx.repoPrefix))
	editDeltaContractCache.Lock()
	editDeltaContractCache.entries = append(editDeltaContractCache.entries, editDeltaContractCacheEntry{key: key, contracts: snapshot})
	if over := len(editDeltaContractCache.entries) - editDeltaContractCacheEntries; over > 0 {
		editDeltaContractCache.entries = append([]editDeltaContractCacheEntry(nil), editDeltaContractCache.entries[over:]...)
	}
	editDeltaContractCache.Unlock()
	return false
}

// resetEditDeltaContractCache empties the cache (tests).
func resetEditDeltaContractCache() {
	editDeltaContractCache.Lock()
	editDeltaContractCache.entries = nil
	editDeltaContractCache.Unlock()
}

// registryOf builds a registry holding a private copy of list, in order.
func registryOf(list []contracts.Contract) *contracts.Registry {
	reg := contracts.NewRegistry()
	for _, c := range copyContracts(list) {
		reg.Add(c)
	}
	return reg
}

// copyContracts copies list deeply enough that no two registries share a
// mutable Meta value.
func copyContracts(list []contracts.Contract) []contracts.Contract {
	out := make([]contracts.Contract, len(list))
	for i, c := range list {
		c.Meta = copyContractMetaMap(c.Meta)
		out[i] = c
	}
	return out
}

func copyContractMetaMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = copyContractMetaValue(v)
	}
	return out
}

func copyContractMetaValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return copyContractMetaMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyContractMetaValue(e)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	default:
		return v
	}
}

// The bottom store's projection cache (graph.BaseProjectionCache), one per
// immutable stack, kept across deltas like the contract registry: the
// resolver's directory index and import reachability read the same rows from
// the same stack on every delta over it.
var editDeltaBaseCaches struct {
	sync.Mutex
	keys   []string // most recent last
	caches map[string]*graph.BaseProjectionCache
}

// editDeltaBaseCacheKey is editDeltaContractCacheKey without the repository
// scope: the cache holds the bottom store's answers for every repository.
func editDeltaBaseCacheKey(base graph.Reader, store any) (string, bool) {
	return editDeltaContractCacheKey(base, store, "*", "", "")
}

// editDeltaBaseCache returns the stack's cache, creating it on first use.
func editDeltaBaseCache(key string) *graph.BaseProjectionCache {
	editDeltaBaseCaches.Lock()
	defer editDeltaBaseCaches.Unlock()
	if editDeltaBaseCaches.caches == nil {
		editDeltaBaseCaches.caches = make(map[string]*graph.BaseProjectionCache)
	}
	if c, ok := editDeltaBaseCaches.caches[key]; ok {
		for i, k := range editDeltaBaseCaches.keys {
			if k == key {
				editDeltaBaseCaches.keys = append(append(editDeltaBaseCaches.keys[:i:i], editDeltaBaseCaches.keys[i+1:]...), key)
				break
			}
		}
		return c
	}
	c := graph.NewBaseProjectionCache()
	editDeltaBaseCaches.caches[key] = c
	editDeltaBaseCaches.keys = append(editDeltaBaseCaches.keys, key)
	for len(editDeltaBaseCaches.keys) > editDeltaContractCacheEntries {
		delete(editDeltaBaseCaches.caches, editDeltaBaseCaches.keys[0])
		editDeltaBaseCaches.keys = editDeltaBaseCaches.keys[1:]
	}
	return c
}
