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
	for p, before := range s.inputs {
		data, err := os.ReadFile(p)
		var now [sha256.Size]byte
		if err == nil {
			now = sha256.Sum256(data)
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
	s := workingAliases(root, idx.contentProof, paths)
	if _, delta := idx.graph.(*graph.DeltaWriter); delta {
		// The delta planner owns its frontier. It does not adopt the live
		// graph's receipt or certify a rebind for the checkout.
		if prior := idx.appliedAliases; prior != "" && prior != s.digest {
			return &editDeltaRefusedError{reason: "TS alias policy changed"}
		}
		idx.appliedAliases = s.digest
		idx.passAliases.Store(s)
		return nil
	}
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

func (idx *Indexer) finishAliasPass(result *IndexResult) error {
	s := idx.passAliases.Load()
	if s == nil || s.digest == idx.appliedAliases || result == nil ||
		result.mutationErr != nil || len(result.Errors) != 0 || len(result.FailedFiles) != 0 {
		return nil
	}
	if store, ok := idx.graph.(aliasStateStore); ok {
		if err := store.SetTSAliasDigest(idx.repoPrefix, s.digest); err != nil {
			return err
		}
	}
	idx.appliedAliases = s.digest
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
