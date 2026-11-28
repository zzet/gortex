package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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
	root       string // checkout root the inputs are under
	collection *tsalias.Collection
	digest     string
	inputs     map[string][sha256.Size]byte // absolute config paths; zero hash for unreadable/missing
	// stats lets a save skip rereading an input whose size and mtime are
	// unchanged. Guarded by the cache slot's mutex.
	stats map[string]aliasInputStat
}

// aliasInputStat is an input's identity at load time; missing marks a probe
// that did not exist.
type aliasInputStat struct {
	size    int64
	mtime   int64
	missing bool
}

func statAliasInput(dir *os.Root, rel string) aliasInputStat {
	info, err := dir.Stat(rel)
	if err != nil {
		return aliasInputStat{missing: true}
	}
	return aliasInputStat{size: info.Size(), mtime: info.ModTime().UnixNano()}
}

// racyStatWindow covers file systems whose mtimes are coarser than the clock.
const racyStatWindow = 2 * time.Second

// recordStats captures each input's stat after a load that began at start.
// The stat is taken after the read, so an input modified since start, or one
// whose existence no longer matches its read, may have been rewritten in
// between: its stat is left out and the next save rereads it.
func (s *aliasSnapshot) recordStats(start time.Time) {
	s.stats = make(map[string]aliasInputStat, len(s.inputs))
	dir, err := os.OpenRoot(s.root)
	if err != nil {
		return
	}
	defer dir.Close()
	racy := start.Add(-racyStatWindow).UnixNano()
	var absent [sha256.Size]byte
	for p, hash := range s.inputs {
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			continue
		}
		stat := statAliasInput(dir, rel)
		if stat.missing != (hash == absent) || !stat.missing && stat.mtime >= racy {
			continue
		}
		s.stats[p] = stat
	}
}

// aliasDiscoveryWalks counts working-copy discovery walks per root; tests use
// it to prove a pass stayed off the walk.
var aliasDiscoveryWalks sync.Map // root -> *atomic.Int64

func noteAliasDiscoveryWalk(root string) {
	counter, _ := aliasDiscoveryWalks.LoadOrStore(root, new(atomic.Int64))
	counter.(*atomic.Int64).Add(1)
}

func aliasDiscoveryWalkCount(root string) int64 {
	if counter, ok := aliasDiscoveryWalks.Load(root); ok {
		return counter.(*atomic.Int64).Load()
	}
	return 0
}

// relInputs keys the inputs by repo-relative slash path with a hex hash, the
// form persisted beside the digest. A missing probe maps to "".
func (s *aliasSnapshot) relInputs(root string) map[string]string {
	out := make(map[string]string, len(s.inputs))
	var zero [sha256.Size]byte
	for p, hash := range s.inputs {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		if hash == zero {
			out[filepath.ToSlash(rel)] = ""
		} else {
			out[filepath.ToSlash(rel)] = hex.EncodeToString(hash[:])
		}
	}
	return out
}

func aliasSlot(root string) *aliasCacheSlot {
	tsAliasCacheMu.Lock()
	defer tsAliasCacheMu.Unlock()
	slot := tsAliasCache[root]
	if slot == nil {
		slot = new(aliasCacheSlot)
		tsAliasCache[root] = slot
	}
	return slot
}

// cachedAliasInput reports whether p is a config input of the cached snapshot.
func cachedAliasInput(root, p string) bool {
	tsAliasCacheMu.Lock()
	slot := tsAliasCache[root]
	tsAliasCacheMu.Unlock()
	if slot == nil {
		return false
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.snapshot == nil {
		return false
	}
	_, ok := slot.snapshot.inputs[p]
	return ok
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
	racy := time.Now().Add(-racyStatWindow).UnixNano()
	var absent [sha256.Size]byte
	for p, before := range s.inputs {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return true
		}
		stat := statAliasInput(dir, rel)
		if recorded, ok := s.stats[p]; ok && recorded == stat {
			continue
		}
		var now [sha256.Size]byte
		if data, err := dir.ReadFile(rel); err == nil {
			now = sha256.Sum256(data)
		}
		if now != before {
			return true
		}
		// Touched, not changed. A stat inside the racy window could still
		// match a later same-size rewrite, so it is not remembered.
		if s.stats != nil && stat.missing == (now == absent) && (stat.missing || stat.mtime < racy) {
			s.stats[p] = stat
		}
	}
	return false
}

// A lookup passes no changed paths; mutation passes validate inputs, with a
// directory scope (including the root) also discovering newly added configs.
func workingAliases(root string, proof *buildContentProof, paths []string) *aliasSnapshot {
	slot := aliasSlot(root)
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
	noteAliasDiscoveryWalk(root)
	start := time.Now()
	s := &aliasSnapshot{root: root, inputs: make(map[string][sha256.Size]byte)}
	s.collection = tsalias.LoadObserved(root, func(rel string, data []byte, found bool) {
		var hash [sha256.Size]byte
		if found {
			hash = sha256.Sum256(data)
		}
		s.inputs[filepath.Join(root, filepath.FromSlash(rel))] = hash
	})
	s.digest = s.collection.Digest()
	s.recordStats(start)
	slot.snapshot = s
	return s
}

// restoreAliases rebuilds a snapshot from inputs persisted with digest,
// without walking the checkout. It returns nil, and leaves the cache alone,
// unless every recorded input still has its recorded content and the scopes
// among them reproduce digest exactly.
func restoreAliases(root, digest string, inputs map[string]string) *aliasSnapshot {
	if digest == "" || inputs == nil {
		return nil
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer dir.Close()
	start := time.Now()
	scopes := make([]string, 0, len(inputs))
	for rel, want := range inputs {
		got := ""
		if data, err := dir.ReadFile(filepath.FromSlash(rel)); err == nil {
			sum := sha256.Sum256(data)
			got = hex.EncodeToString(sum[:])
		}
		if got != want {
			return nil
		}
		if got != "" && tsalias.IsScopeConfig(rel) {
			scopes = append(scopes, rel)
		}
	}
	s := &aliasSnapshot{root: root, inputs: make(map[string][sha256.Size]byte, len(inputs))}
	s.collection = tsalias.LoadFiles(root, scopes, func(rel string, data []byte, found bool) {
		var hash [sha256.Size]byte
		if found {
			hash = sha256.Sum256(data)
		}
		s.inputs[filepath.Join(root, filepath.FromSlash(rel))] = hash
	})
	s.digest = s.collection.Digest()
	if s.digest != digest {
		return nil
	}
	s.recordStats(start)
	slot := aliasSlot(root)
	slot.mu.Lock()
	if slot.snapshot == nil {
		slot.snapshot = s
	} else {
		s = slot.snapshot // a concurrent pass already holds a fresher view
	}
	slot.mu.Unlock()
	return s
}

type aliasStateStore interface {
	TSAliasDigest(repo string) (string, error)
	SetTSAliasDigest(repo, digest string) error
}

// aliasInputStore persists the config inputs behind a certified digest so a
// warm restart can recheck them instead of walking the checkout.
type aliasInputStore interface {
	TSAliasInputs(repo string) (map[string]string, error)
	SetTSAliasInputs(repo string, inputs map[string]string) error
}

// tsAliasCallerInventory answers whether a repository may hold JS/TS callers.
type tsAliasCallerInventory interface {
	MayHaveTSAliasCallers(ctx context.Context, repo string) (bool, error)
}

// mayHaveAliasCallers reports whether any stored caller could consume an alias.
func (idx *Indexer) mayHaveAliasCallers(ctx context.Context) (bool, error) {
	if inv, ok := idx.graph.(tsAliasCallerInventory); ok {
		return inv.MayHaveTSAliasCallers(ctx, idx.repoPrefix)
	}
	idx.mtimeMu.RLock()
	defer idx.mtimeMu.RUnlock()
	for file := range idx.fileMtimes {
		if closureJSTSPath(file) {
			return true, nil
		}
	}
	return false, nil
}

// aliasPassNeeded reports whether a working-copy mutation can consume or
// change an alias binding. Without a JS/TS caller, a JS/TS or config path in
// scope, a known config input, or a held full pass, discovery is skipped; a
// lookup a later parse makes still loads the scopes lazily.
func (idx *Indexer) aliasPassNeeded(ctx context.Context, root string, paths []string, scopeHasJSTS bool) (bool, error) {
	if scopeHasJSTS || idx.deferredAliases.Load() != nil {
		return true, nil
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if closureJSTSPath(p) || tsalias.IsConfigFileName(filepath.Base(p)) || cachedAliasInput(root, p) {
			return true, nil
		}
	}
	return idx.mayHaveAliasCallers(ctx)
}

// beginAliasPass pins the alias policy for one mutation. scopeHasJSTS tells
// a full pass that its walk admitted a JS/TS file; scoped passes infer it
// from paths.
func (idx *Indexer) beginAliasPass(ctx context.Context, root string, paths []string, scopeHasJSTS bool) error {
	if idx.contentSource() != nil {
		return nil // immutable source uses its own memo, never the checkout
	}
	if _, delta := idx.graph.(*graph.DeltaWriter); delta {
		s := idx.deltaAliases
		if s == nil {
			return nil
		}
		idx.aliasStateMu.Lock()
		defer idx.aliasStateMu.Unlock()
		// The delta planner owns its frontier. It does not adopt the live
		// graph's receipt or certify a rebind for the checkout.
		if prior := idx.appliedAliases; prior != "" && prior != s.digest {
			return &editDeltaRefusedError{reason: "TS alias policy changed"}
		}
		idx.appliedAliases = s.digest
		idx.passAliases.Store(s)
		return nil
	}
	needed, err := idx.aliasPassNeeded(ctx, root, paths, scopeHasJSTS)
	if err != nil {
		return err
	}
	if !needed {
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
// unrelated save keeps the resolver's best-effort behavior. Callers hold
// aliasStateMu.
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

func (idx *Indexer) hasUncertifiedAliasPass() bool {
	s := idx.passAliases.Load()
	if s == nil {
		return false
	}
	idx.aliasStateMu.Lock()
	defer idx.aliasStateMu.Unlock()
	return s.digest != idx.appliedAliases
}

func (idx *Indexer) finishAliasPass(result *IndexResult) error {
	idx.aliasStateMu.Lock()
	defer idx.aliasStateMu.Unlock()
	s, err := idx.aliasesToCertify(result)
	if s == nil {
		return err
	}
	return idx.certifyAliasesLocked(s)
}

// holdAliasPass parks a deferred full pass until the shared resolve succeeds.
func (idx *Indexer) holdAliasPass(result *IndexResult) error {
	idx.aliasStateMu.Lock()
	defer idx.aliasStateMu.Unlock()
	s, err := idx.aliasesToCertify(result)
	if s != nil {
		idx.deferredAliases.Store(s)
	}
	return err
}

// certifyAliasesLocked records s as applied; the caller holds aliasStateMu.
// The inputs go first: a crash before the digest write leaves the digest
// cleared, so the inputs are never trusted on their own.
func (idx *Indexer) certifyAliasesLocked(s *aliasSnapshot) error {
	if store, ok := idx.graph.(aliasStateStore); ok {
		if inputs, ok := store.(aliasInputStore); ok && s.root != "" {
			if err := inputs.SetTSAliasInputs(idx.repoPrefix, s.relInputs(s.root)); err != nil {
				return err
			}
		}
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
	if err := idx.certifyAliasesLocked(s); err != nil {
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
// Before republishing a restored JS/TS graph as clean, check its receipt:
// recheck the inputs recorded with the digest, and walk only when that fails.
func (idx *Indexer) restoredAliasCaller(root string, mtimes map[string]int64) (string, error) {
	caller := ""
	for file := range mtimes {
		if closureJSTSPath(file) {
			caller = file
			break
		}
	}
	if caller == "" {
		return "", nil
	}
	store, ok := idx.graph.(aliasStateStore)
	if !ok {
		return caller, nil
	}
	idx.aliasStateMu.Lock()
	defer idx.aliasStateMu.Unlock()
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	if err != nil {
		return "", err
	}
	if digest == "" {
		return caller, nil
	}
	inputStore, hasInputs := store.(aliasInputStore)
	if hasInputs {
		inputs, err := inputStore.TSAliasInputs(idx.repoPrefix)
		if err != nil {
			return "", err
		}
		if restoreAliases(root, digest, inputs) != nil {
			idx.appliedAliases = digest
			return "", nil
		}
	}
	s := workingAliases(root, nil, []string{root})
	if digest != s.digest {
		return caller, nil
	}
	if hasInputs {
		// Record the inputs so the next restart rechecks instead of walking.
		if err := inputStore.SetTSAliasInputs(idx.repoPrefix, s.relInputs(root)); err != nil {
			return "", err
		}
	}
	idx.appliedAliases = digest
	return "", nil
}

// The direct full pass upserts raw extraction rows and resolves after every
// worker has finished. Remove each caller's old outgoing rows only once its
// replacement is ready. EvictFile would also remove other files' incoming
// edges, including ones already written by another worker.
func (idx *Indexer) removePriorAliasEdges(file string) {
	if file == "" {
		return
	}
	removePriorAliasEdgesBatch(idx.graph, []string{file})
}

func removePriorAliasEdgesBatch(store graph.Store, files []string) {
	if store == nil || len(files) == 0 {
		return
	}
	var ids []string
	var nodeFiles map[string]string
	{
		fileNodes := store.GetFileNodesByPaths(files)
		if len(fileNodes) == 0 {
			return
		}
		nodeFiles = make(map[string]string)
		for file, nodes := range fileNodes {
			if file == "" {
				continue
			}
			for _, n := range nodes {
				if n == nil || n.ID == "" {
					continue
				}
				if _, seen := nodeFiles[n.ID]; !seen {
					ids = append(ids, n.ID)
					nodeFiles[n.ID] = file
				}
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	var prior []*graph.Edge
	for fromID, edges := range store.GetOutEdgesByNodeIDs(ids) {
		file := nodeFiles[fromID]
		if file == "" {
			continue
		}
		for _, e := range edges {
			if e != nil && e.FilePath == file {
				prior = append(prior, e)
			}
		}
	}
	graph.RemoveEdgesExact(store, prior)
}
