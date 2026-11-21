package indexer

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// Only checkout lookups use the root cache. A pass pins one immutable snapshot;
// the graph's applied digest is independent of lookups and cache eviction.
var (
	tsAliasCacheMu sync.Mutex
	tsAliasCache   = make(map[string]*aliasCacheSlot)
)

type aliasCacheSlot struct {
	mu       sync.Mutex
	snapshot *aliasSnapshot
}

type aliasSnapshot struct {
	collection *tsalias.Collection
	digest     string
	inputs     map[string][sha256.Size]byte // absolute config paths; zero hash for unreadable/missing
}

func invalidateTSAliasCollection(root string) {
	tsAliasCacheMu.Lock()
	delete(tsAliasCache, root)
	tsAliasCacheMu.Unlock()
}

func loadTSAliasCollection(root string) *tsalias.Collection {
	return workingAliases(root, nil, nil).collection
}

func (s *aliasSnapshot) needsDiscovery(root string, paths []string) bool {
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if p == root || tsalias.IsConfigFileName(filepath.Base(p)) {
			return true
		}
		// Directory-create notifications can introduce nested configs.
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return true
		}
	}
	// Recheck through the root, as LoadObserved read them.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return true
	}
	defer dir.Close()
	for p, before := range s.inputs {
		var now [sha256.Size]byte
		if rel, err := filepath.Rel(root, p); err == nil {
			if data, err := dir.ReadFile(rel); err == nil {
				now = sha256.Sum256(data)
			}
		}
		if now != before {
			return true
		}
	}
	return false
}

// A lookup passes no changed paths; mutation passes validate inputs, with a
// directory scope (including the root) also discovering newly added configs.
func workingAliases(root string, proof *buildContentProof, paths []string) *aliasSnapshot {
	tsAliasCacheMu.Lock()
	slot := tsAliasCache[root]
	if slot == nil {
		slot = new(aliasCacheSlot)
		tsAliasCache[root] = slot
	}
	tsAliasCacheMu.Unlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if s := slot.snapshot; s != nil && (len(paths) == 0 || !s.needsDiscovery(root, paths)) {
		if proof.active() {
			for p := range s.inputs {
				proof.record(p, "")
			}
		}
		return s
	}
	proof.noteUnboundedReader() // only the actual directory walk needs the full sample
	s := &aliasSnapshot{inputs: make(map[string][sha256.Size]byte)}
	s.collection = tsalias.LoadObserved(root, func(rel string, data []byte, found bool) {
		var hash [sha256.Size]byte
		if found {
			hash = sha256.Sum256(data)
		}
		s.inputs[filepath.Join(root, filepath.FromSlash(rel))] = hash
	})
	s.digest = s.collection.Digest()
	slot.snapshot = s
	return s
}

type aliasStateStore interface {
	TSAliasDigest(repo string) (string, error)
	SetTSAliasDigest(repo, digest string) error
}

func (idx *Indexer) beginAliasPass(root string, paths []string) error {
	if idx.contentSource() != nil {
		return nil // immutable source uses its own memo, never the checkout
	}
	if _, delta := idx.graph.(*graph.DeltaWriter); delta {
		s := idx.deltaAliases
		if s == nil {
			return nil
		}
		// The delta planner owns its frontier. It does not adopt the live
		// graph's receipt or certify a rebind for the checkout.
		if prior := idx.appliedAliases; prior != "" && prior != s.digest {
			return &editDeltaRefusedError{reason: "TS alias policy changed"}
		}
		idx.appliedAliases = s.digest
		idx.passAliases.Store(s)
		return nil
	}
	s := workingAliases(root, idx.contentProof, paths)
	idx.aliasStateMu.Lock()
	defer idx.aliasStateMu.Unlock()
	// This mutation overtakes any full pass still waiting on the shared resolve.
	idx.deferredAliases.Store(nil)
	prior := idx.appliedAliases
	store, durable := idx.graph.(aliasStateStore)
	if durable && prior == "" {
		var err error
		prior, err = store.TSAliasDigest(idx.repoPrefix)
		if err != nil {
			return err
		}
	}
	if prior != s.digest {
		// A crash after partially rebinding A -> B must also be retried if
		// the config has since reverted to A.
		if durable && prior != "" {
			if err := store.SetTSAliasDigest(idx.repoPrefix, ""); err != nil {
				return err
			}
		}
		idx.appliedAliases = ""
		idx.aliasResolutionChanged = prior != "" || idx.trackedFileCount() != 0 || idx.graph.NodeCount() != 0
	} else {
		idx.appliedAliases = prior
	}
	idx.passAliases.Store(s)
	return nil
}

// aliasesToCertify returns the policy this pass rebound, or nil when it has
// nothing to record. A failed whole-graph resolve surfaces only here, so an
// unrelated save keeps the resolver's best-effort behavior.
func (idx *Indexer) aliasesToCertify(result *IndexResult) (*aliasSnapshot, error) {
	s := idx.passAliases.Load()
	if s == nil || s.digest == idx.appliedAliases || result == nil || result.mutationErr != nil {
		return nil, nil
	}
	if result.resolutionErr != nil {
		return nil, result.resolutionErr
	}
	// The failure ledger retries a failed file with the current policy. Waiting
	// for it would restage every JS/TS caller on each save until it reads.
	if (len(result.Errors) != 0 || len(result.FailedFiles) != 0) && !result.failuresRetryable {
		return nil, nil
	}
	return s, nil
}

func (idx *Indexer) finishAliasPass(result *IndexResult) error {
	s, err := idx.aliasesToCertify(result)
	if s == nil {
		return err
	}
	return idx.certifyAliases(s)
}

// holdAliasPass parks a deferred full pass until the shared resolve succeeds.
func (idx *Indexer) holdAliasPass(result *IndexResult) error {
	s, err := idx.aliasesToCertify(result)
	if s != nil {
		idx.deferredAliases.Store(s)
	}
	return err
}

func (idx *Indexer) certifyAliases(s *aliasSnapshot) error {
	if store, ok := idx.graph.(aliasStateStore); ok {
		if err := store.SetTSAliasDigest(idx.repoPrefix, s.digest); err != nil {
			return err
		}
	}
	idx.appliedAliases = s.digest
	return nil
}

// deferredAliasPasses captures held passes before the shared resolver starts,
// so a pass parked after that point is not certified by a resolve that may
// have missed its edges.
func (mi *MultiIndexer) deferredAliasPasses(scope map[string]struct{}) map[*Indexer]*aliasSnapshot {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	var pending map[*Indexer]*aliasSnapshot
	for prefix, idx := range mi.indexers {
		if _, ok := scope[prefix]; len(scope) > 0 && !ok {
			continue
		}
		if s := idx.deferredAliases.Load(); s != nil {
			if pending == nil {
				pending = make(map[*Indexer]*aliasSnapshot)
			}
			pending[idx] = s
		}
	}
	return pending
}

// The shared resolve may run with or without the repository lanes held, so
// certification serializes with beginAliasPass on aliasStateMu instead.
func certifyDeferredAliases(pending map[*Indexer]*aliasSnapshot) error {
	for idx, s := range pending {
		if err := idx.certifyDeferredAlias(s); err != nil {
			return err
		}
	}
	return nil
}

func (idx *Indexer) certifyDeferredAlias(s *aliasSnapshot) error {
	idx.aliasStateMu.Lock()
	defer idx.aliasStateMu.Unlock()
	if idx.deferredAliases.Load() != s {
		return nil // a newer mutation owns the receipt
	}
	if err := idx.certifyAliases(s); err != nil {
		return err
	}
	idx.deferredAliases.Store(nil)
	return nil
}

func (idx *Indexer) clearAliasPass() {
	idx.passAliases.Store(nil)
	idx.aliasResolutionChanged = false
}

// The mtime census may omit inherited configs (for example under .nuxt).
// Before republishing a restored JS/TS graph as clean, check its receipt.
func (idx *Indexer) restoredAliasCaller(root string, mtimes map[string]int64) (string, error) {
	for file := range mtimes {
		if !closureJSTSPath(file) {
			continue
		}
		s := workingAliases(root, nil, []string{root})
		if store, ok := idx.graph.(aliasStateStore); ok {
			idx.aliasStateMu.Lock()
			defer idx.aliasStateMu.Unlock()
			digest, err := store.TSAliasDigest(idx.repoPrefix)
			if err != nil {
				return "", err
			}
			if digest == s.digest {
				idx.appliedAliases = digest
				return "", nil
			}
		}
		return file, nil
	}
	return "", nil
}

// The direct full pass upserts raw extraction rows and resolves after every
// worker has finished. Remove this caller's old outgoing rows only once its
// replacement is ready. EvictFile would also remove other files' incoming
// edges, including ones already written by another worker.
func (idx *Indexer) removePriorAliasEdges(file string) {
	nodes := idx.graph.GetFileNodes(file)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	var prior []*graph.Edge
	for _, edges := range idx.graph.GetOutEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if e.FilePath == file {
				prior = append(prior, e)
			}
		}
	}
	graph.RemoveEdgesExact(idx.graph, prior)
}
