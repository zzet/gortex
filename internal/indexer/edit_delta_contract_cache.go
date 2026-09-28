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
// A working-tree edit stands on the dirty chain the previous edits published
// (commitLayerBase.chainDepth): the per-stack caches are keyed by the stack
// below the chain, so consecutive edits on one commit share them, and each
// delta composes the chain's layers per read (graph.DeltaWriter's
// SetChainGenerations). The contract registry alone is not composed per read
// and stays keyed by the whole stack.
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
	// Over a dirty chain the key is the stack below the chain: a delta
	// composes the chain's layers over the kept answers per read, so every
	// consecutive edit on one commit shares them.
	stack := layer.commitStack()
	if len(stack) == 0 {
		return "", false
	}
	for _, gen := range stack {
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

// editDeltaRegistryKey is the contract registry's key for a delta over base:
// the whole stack's, the dirty chain included, since the registry is not
// composed per read.
func editDeltaRegistryKey(base graph.Reader, store any, repoPrefix, workspaceID, projectID string) (string, bool) {
	return editDeltaContractCacheKey(wholeStack(base), store, repoPrefix, workspaceID, projectID)
}

// wholeStack is base keyed by its whole stack, the dirty chain included: the
// key of a cache the chain is not composed over per read.
func wholeStack(base graph.Reader) graph.Reader {
	if layer, ok := base.(commitLayerBase); ok && layer.chainDepth > 0 {
		layer.chainDepth = 0
		return layer
	}
	return base
}

// installEditDeltaStackCache installs the per-stack projection cache on a
// delta over base. Over a dirty chain (base's chainDepth) the cache is the
// one kept for the stack below the chain and the delta is told the chain's
// generations, so it composes them per read; a view whose layers do not end
// in those generations is keyed by its whole stack instead. keyBase is the
// base the delta's per-stack keys are then taken from, and cacheBase the view
// its caches are loaded from (the view below the chain, or base).
func installEditDeltaStackCache(dw *graph.DeltaWriter, base graph.Reader, store any) (keyBase, cacheBase graph.Reader) {
	keyBase, cacheBase = base, base
	if layer, ok := base.(commitLayerBase); ok && layer.chainDepth > 0 {
		if key, ok := editDeltaBaseCacheKey(base, store); ok {
			chain := layer.chainGenerations()
			epochs := make([]uint64, len(chain))
			if withEpochs, ok := store.(interface{ GenerationCorrectionEpoch(int64) uint64 }); ok {
				for i, gen := range chain {
					epochs[i] = withEpochs.GenerationCorrectionEpoch(gen)
				}
			}
			dw.SetChainGenerations(chain, epochs)
			dw.SetBaseProjectionCache(editDeltaBaseCache(key))
			if below, split := dw.ChainSplitBase(); split {
				dw.SetChainLayerRows(editDeltaChainLayerRowsFor(store))
				return base, below
			}
			dw.SetChainGenerations(nil, nil)
			dw.SetBaseProjectionCache(nil)
		}
		keyBase, cacheBase = wholeStack(base), base
	}
	if key, ok := editDeltaBaseCacheKey(keyBase, store); ok {
		dw.SetBaseProjectionCache(editDeltaBaseCache(key))
	}
	return keyBase, cacheBase
}

// The dirty chain's layers kept across deltas (graph.ChainLayerRows), one
// keeper per store: a chain layer is a published generation of that store,
// keyed there by its id and correction epoch.
var editDeltaChainLayers struct {
	sync.Mutex
	byStore map[any]*graph.ChainLayerRows
}

// editDeltaChainLayerRowsFor returns store's keeper, creating it on first use.
func editDeltaChainLayerRowsFor(store any) *graph.ChainLayerRows {
	editDeltaChainLayers.Lock()
	defer editDeltaChainLayers.Unlock()
	if editDeltaChainLayers.byStore == nil {
		editDeltaChainLayers.byStore = make(map[any]*graph.ChainLayerRows)
	}
	keeper := editDeltaChainLayers.byStore[store]
	if keeper == nil {
		keeper = graph.NewChainLayerRows(0, 0)
		editDeltaChainLayers.byStore[store] = keeper
	}
	return keeper
}

// forgetEditDeltaChainLayer drops a generation that stopped being servable
// from store's keeper.
func forgetEditDeltaChainLayer(store any, generation int64) {
	editDeltaChainLayers.Lock()
	keeper := editDeltaChainLayers.byStore[store]
	editDeltaChainLayers.Unlock()
	keeper.Forget(generation)
}

// resetEditDeltaChainLayers empties every keeper (tests).
func resetEditDeltaChainLayers() {
	editDeltaChainLayers.Lock()
	editDeltaChainLayers.byStore = nil
	editDeltaChainLayers.Unlock()
}

// Carrying the registry forward.
//
// The contract registry is not composed per read over a dirty chain
// (editDeltaRegistryKey), so a chained edit would reload it on every new
// stack. A delta keeps its registry exact as it goes
// (commitIncrementalContractFiles), and the generation it publishes is
// exactly the stack the next edit over it stands on: the delta's final
// registry is filed under that stack's key when the generation publishes. A
// correction of the generation moves its epoch, and with it the key.

// editDeltaRegistryCarry returns the function the build calls with the
// published generation's id, or nil when idx's registry was not keyed.
func editDeltaRegistryCarry(idx *Indexer, base graph.Reader, store any, repoPrefix, workspaceID, projectID string) func(int64) {
	layer, ok := base.(commitLayerBase)
	if !ok || len(layer.stack) == 0 || idx == nil || idx.contractRegistry == nil {
		return nil
	}
	if _, keyed := editDeltaRegistryKey(base, store, repoPrefix, workspaceID, projectID); !keyed {
		return nil
	}
	snapshot := copyContracts(idx.contractRegistry.ByRepo(repoPrefix))
	stack := append([]int64(nil), layer.stack...)
	return func(generation int64) {
		if generation <= 0 {
			return
		}
		next := commitLayerBase{stack: append(append([]int64(nil), stack...), generation)}
		key, ok := editDeltaRegistryKey(next, store, repoPrefix, workspaceID, projectID)
		if !ok {
			return
		}
		storeEditDeltaContractRegistry(key, snapshot)
	}
}

// storeEditDeltaContractRegistry files contracts under key, replacing an
// entry already there.
func storeEditDeltaContractRegistry(key string, list []contracts.Contract) {
	editDeltaContractCache.Lock()
	defer editDeltaContractCache.Unlock()
	for i, entry := range editDeltaContractCache.entries {
		if entry.key == key {
			editDeltaContractCache.entries = append(editDeltaContractCache.entries[:i:i], editDeltaContractCache.entries[i+1:]...)
			break
		}
	}
	editDeltaContractCache.entries = append(editDeltaContractCache.entries, editDeltaContractCacheEntry{key: key, contracts: list})
	if over := len(editDeltaContractCache.entries) - editDeltaContractCacheEntries; over > 0 {
		editDeltaContractCache.entries = append([]editDeltaContractCacheEntry(nil), editDeltaContractCache.entries[over:]...)
	}
}
