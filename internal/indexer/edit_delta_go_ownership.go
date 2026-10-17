package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"iter"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// The Go package ownership a delta's resolver consults is derived from the
// repository's Go file inventory (every Go file's directory) and the go.mod /
// go.work manifests above those directories. The inventory is what makes it
// expensive: the resolver's own factory feeds it a full-row scan of every file
// node in the repository, once per resolver pass, so a delta paid that scan
// twice (the catch-up and the affected-by pass) on every save.
//
// A delta's view is the immutable stack below plus the files it writes. The
// stack's inventory is therefore kept across deltas over the same stack (like
// the contract registry and the base projection cache), and each delta
// overlays its own change set as its view holds it. The manifests are still
// read from the delta's source on every delta, so an edited go.mod is never
// served stale. Within one delta the prepared lookup is shared by every pass:
// the delta's file set and source do not change between them.

type editDeltaGoInventoryEntry struct {
	key   string
	files []string // graph paths of the stack's Go files, sorted
}

var editDeltaGoInventories struct {
	sync.Mutex
	entries []editDeltaGoInventoryEntry // most recent last
}

func cachedEditDeltaGoInventory(key string) ([]string, bool) {
	editDeltaGoInventories.Lock()
	defer editDeltaGoInventories.Unlock()
	for i := len(editDeltaGoInventories.entries) - 1; i >= 0; i-- {
		entry := editDeltaGoInventories.entries[i]
		if entry.key != key {
			continue
		}
		editDeltaGoInventories.entries = append(append(editDeltaGoInventories.entries[:i:i], editDeltaGoInventories.entries[i+1:]...), entry)
		return entry.files, true
	}
	return nil, false
}

func storeEditDeltaGoInventory(key string, files []string) {
	editDeltaGoInventories.Lock()
	defer editDeltaGoInventories.Unlock()
	editDeltaGoInventories.entries = append(editDeltaGoInventories.entries, editDeltaGoInventoryEntry{key: key, files: files})
	if over := len(editDeltaGoInventories.entries) - editDeltaContractCacheEntries; over > 0 {
		editDeltaGoInventories.entries = append([]editDeltaGoInventoryEntry(nil), editDeltaGoInventories.entries[over:]...)
	}
}

// resetEditDeltaGoInventories empties the cache (tests).
func resetEditDeltaGoInventories() {
	editDeltaGoInventories.Lock()
	editDeltaGoInventories.entries = nil
	editDeltaGoInventories.Unlock()
}

// goInventoryOf lists the graph paths of the Go file nodes in nodes, sorted.
func goInventoryOf(nodes iter.Seq[*graph.Node]) []string {
	var files []string
	for node := range nodes {
		if node != nil && node.Kind == graph.KindFile && node.Language == "go" {
			files = append(files, node.FilePath)
		}
	}
	sort.Strings(files)
	return files
}

// editDeltaGoOwnership is one delta's ownership factory state.
type editDeltaGoOwnership struct {
	idx     *Indexer
	key     string
	changed []string // graph paths of the delta's change set
	// base is the stack's inventory, read from the delta's view before any
	// write when the cache does not hold it.
	base     []string
	baseHit  bool
	prepared map[string]resolver.GoPackageOwnershipLookup
	ready    bool
	calls    int // factory calls
	prepares int // lookups actually prepared
	// manifestChange is set when the change set holds a module or workspace
	// manifest: the ownership then depends on the working tree's manifests,
	// and neither reuses nor keeps a lookup across deltas.
	manifestChange bool
	reused         bool // the lookup came from an earlier delta
}

// editDeltaGoOwnershipLookups keeps prepared ownership lookups across deltas.
// A lookup is a pure function of the Go file set (each file's directory) and
// the module and workspace manifests on the directories' ancestor chains,
// read from the checkout. Those are the commit's unless the change set holds
// one (then nothing is reused), so the key is the checkout root, the commit
// the checkout stands on (its HEAD, not the stack: consecutive edits stack
// generations but stand on one commit) and the exact file set. Preparing one reads a manifest candidate per directory
// ancestor from the working tree: about 15 ms per edit on this repository.
var editDeltaGoOwnershipLookups struct {
	sync.Mutex
	entries map[string]map[string]resolver.GoPackageOwnershipLookup
	keys    []string
}

// editDeltaGoOwnershipLookupEntries bounds the kept lookups.
const editDeltaGoOwnershipLookupEntries = 16

func cachedEditDeltaGoOwnership(key string) (map[string]resolver.GoPackageOwnershipLookup, bool) {
	editDeltaGoOwnershipLookups.Lock()
	defer editDeltaGoOwnershipLookups.Unlock()
	v, ok := editDeltaGoOwnershipLookups.entries[key]
	return v, ok
}

func storeEditDeltaGoOwnership(key string, lookup map[string]resolver.GoPackageOwnershipLookup) {
	editDeltaGoOwnershipLookups.Lock()
	defer editDeltaGoOwnershipLookups.Unlock()
	if editDeltaGoOwnershipLookups.entries == nil {
		editDeltaGoOwnershipLookups.entries = make(map[string]map[string]resolver.GoPackageOwnershipLookup)
	}
	if _, ok := editDeltaGoOwnershipLookups.entries[key]; !ok {
		editDeltaGoOwnershipLookups.keys = append(editDeltaGoOwnershipLookups.keys, key)
	}
	editDeltaGoOwnershipLookups.entries[key] = lookup
	for len(editDeltaGoOwnershipLookups.keys) > editDeltaGoOwnershipLookupEntries {
		delete(editDeltaGoOwnershipLookups.entries, editDeltaGoOwnershipLookups.keys[0])
		editDeltaGoOwnershipLookups.keys = editDeltaGoOwnershipLookups.keys[1:]
	}
}

func resetEditDeltaGoOwnershipLookups() {
	editDeltaGoOwnershipLookups.Lock()
	defer editDeltaGoOwnershipLookups.Unlock()
	editDeltaGoOwnershipLookups.entries, editDeltaGoOwnershipLookups.keys = nil, nil
}

// commitKey is the commit the delta's checkout stands on, from the build's
// HEAD sample; the stack key when the build carries none.
func (o *editDeltaGoOwnership) commitKey() string {
	if p := o.idx.headProvenance; p != nil && p.sha != "" {
		return "commit\x00" + p.sha
	}
	return "stack\x00" + o.key
}

// goOwnershipLookupKey is the cache key of a lookup over files: the commit
// (or stack), the checkout root and a digest of the sorted file set.
func goOwnershipLookupKey(stackKey, root string, files []string) string {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return stackKey + "\x00" + root + "\x00" + hex.EncodeToString(h.Sum(nil))
}

// primeEditDeltaGoOwnership installs the per-stack ownership factory on idx's
// resolver. It must run before the delta writes: on a cache miss it reads the
// stack's inventory from idx's view, which is then still the stack. A change
// set with no Go path leaves the resolver's own factory in place.
func primeEditDeltaGoOwnership(idx *Indexer, key string, changedRel []string) *editDeltaGoOwnership {
	if idx == nil || idx.resolver == nil || key == "" {
		return nil
	}
	changed := make([]string, 0, len(changedRel))
	goChange := false
	for _, rel := range changedRel {
		if strings.HasSuffix(rel, ".go") {
			goChange = true
		}
		changed = append(changed, idx.prefixPath(rel))
	}
	if !goChange {
		return nil
	}
	manifestChange := changeSetHasManifest(changedRel)
	// Over a dirty chain the inventory is kept for the stack below the chain:
	// the paths the chain speaks for are overlaid like the change set's, and
	// a manifest the chain edited is a manifest change.
	var chainTouched []string
	var belowChain graph.Reader
	if dw, ok := idx.graph.(*graph.DeltaWriter); ok && dw.ChainLayers() > 0 {
		below, split := dw.ChainSplitBase()
		if !split {
			return nil
		}
		belowChain = below
		for p := range dw.ChainTouchedPaths() {
			chainTouched = append(chainTouched, p)
			if changeSetHasManifest([]string{p}) {
				manifestChange = true
			}
		}
		sort.Strings(chainTouched)
		changed = append(changed, chainTouched...)
	}
	o := &editDeltaGoOwnership{idx: idx, key: key, changed: changed, manifestChange: manifestChange}
	_, o.baseHit = cachedEditDeltaGoInventory(key)
	// A pre-warm reading the same inventory is waited for, not repeated.
	singleFlight("inventory\x00"+key, func() bool { _, ok := cachedEditDeltaGoInventory(key); return ok }, func() {
		files := goInventoryOf(graph.NodesInScopeSeq(idx.graph, []string{idx.repoPrefix}, nil, graph.KindFile))
		if belowChain != nil {
			files = goInventoryBelowChain(files, chainTouched, belowChain, idx.repoPrefix)
		}
		storeEditDeltaGoInventory(key, files)
	})
	o.base, _ = cachedEditDeltaGoInventory(key)
	idx.resolver.SetGoPackageOwnershipFactory(o.factory)
	return o
}

// currentFiles is the delta view's Go inventory: the stack's, with the change
// set's paths as the view holds them now.
func (o *editDeltaGoOwnership) currentFiles() iter.Seq[*graph.Node] {
	set := make(map[string]struct{}, len(o.base)+len(o.changed))
	for _, p := range o.base {
		set[p] = struct{}{}
	}
	for _, p := range o.changed {
		delete(set, p)
	}
	for _, nodes := range o.idx.graph.GetFileNodesByPaths(o.changed) {
		for _, node := range nodes {
			if node != nil && node.Kind == graph.KindFile && node.Language == "go" && node.RepoPrefix == o.idx.repoPrefix {
				set[node.FilePath] = struct{}{}
			}
		}
	}
	prefix := o.idx.repoPrefix
	return func(yield func(*graph.Node) bool) {
		for p := range set {
			if !yield(&graph.Node{ID: p, Kind: graph.KindFile, Language: "go", RepoPrefix: prefix, FilePath: p}) {
				return
			}
		}
	}
}

func (o *editDeltaGoOwnership) factory(ctx context.Context, prefixes []string, files iter.Seq[*graph.Node]) (map[string]resolver.GoPackageOwnershipLookup, error) {
	// The indexer prepares ownership for its own repository only
	// (Indexer.prepareGoPackageOwnership targets idx.repoPrefix); another
	// requested prefix (a source with no repository among the pending) gets
	// no lookup either way, so it must not cost a scan of every requested
	// repository's files.
	own := false
	for _, prefix := range prefixes {
		if prefix == o.idx.repoPrefix {
			own = true
		}
	}
	if !own {
		return nil, ctx.Err()
	}
	prefixes = []string{o.idx.repoPrefix}
	o.calls++
	if o.ready {
		return o.prepared, nil
	}
	started := time.Now()
	current := o.currentFiles()
	var lookupKey string
	if !o.manifestChange {
		var paths []string
		for node := range current {
			paths = append(paths, node.FilePath)
		}
		lookupKey = goOwnershipLookupKey(o.commitKey(), o.idx.rootPath, paths)
		if cached, ok := cachedEditDeltaGoOwnership(lookupKey); ok {
			o.prepared, o.ready, o.reused = cached, true, true
			return cached, nil
		}
	}
	o.prepares++
	prepared, err := o.idx.prepareGoPackageOwnership(ctx, prefixes, current)
	if err != nil {
		return nil, err
	}
	o.prepared, o.ready = prepared, true
	if lookupKey != "" {
		storeEditDeltaGoOwnership(lookupKey, prepared)
	}
	if o.idx.logger != nil {
		o.idx.logger.Info("edit delta: go package ownership",
			zap.Bool("inventory_cached", o.baseHit),
			zap.Int("stack_go_files", len(o.base)),
			zap.Int64("prepare_ms", time.Since(started).Milliseconds()))
	}
	return prepared, nil
}

// goInventoryBelowChain is the Go inventory of the stack below a dirty chain
// from the delta's view's inventory (viewFiles, read before the delta
// writes): every path the chain speaks for (touched) is answered by the stack
// below the chain instead.
func goInventoryBelowChain(viewFiles, touched []string, below graph.Reader, repoPrefix string) []string {
	skip := make(map[string]struct{}, len(touched))
	for _, p := range touched {
		skip[p] = struct{}{}
	}
	set := make(map[string]struct{}, len(viewFiles))
	for _, p := range viewFiles {
		if _, t := skip[p]; !t {
			set[p] = struct{}{}
		}
	}
	for _, p := range touched {
		for _, node := range below.GetFileNodes(p) {
			if node != nil && node.Kind == graph.KindFile && node.Language == "go" && node.RepoPrefix == repoPrefix {
				set[node.FilePath] = struct{}{}
			}
		}
	}
	files := make([]string, 0, len(set))
	for p := range set {
		files = append(files, p)
	}
	sort.Strings(files)
	return files
}
