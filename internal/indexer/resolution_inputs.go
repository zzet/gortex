package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/nuxtimports"
	"github.com/zzet/gortex/internal/parser/tsalias"
)

// Shared lookup caches describe the checkout, not what any one graph has applied.
// reads includes malformed configs and absent extends parents: repairing either
// can change resolution without changing a caller or an already effective scope.
type resolutionInputs struct {
	aliases *tsalias.Collection
	nuxt    *nuxtimports.Collection
	reads   map[string]string
	digest  string
}

// Both loaders consume one discovery walk. Only Exists escapes the load (for
// multi-target alias resolution); it always probes the working tree itself.
type resolutionTree struct {
	tsalias.Tree
	root  string
	dir   *os.Root
	paths []string
	reads map[string]string
	proof *buildContentProof
}

func (t *resolutionTree) Files(visit func(string)) {
	for _, rel := range t.paths {
		visit(rel)
	}
}

func (t *resolutionTree) Read(rel string) ([]byte, bool) {
	data, sum := readResolutionInput(t.dir, t.root, rel, t.proof)
	t.reads[rel] = sum
	return data, sum != "" && sum != absentContent
}

func readResolutionInput(dir *os.Root, root, rel string, proof *buildContentProof) ([]byte, string) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if dir == nil {
		proof.record(abs, "")
		return nil, ""
	}
	data, err := dir.ReadFile(filepath.FromSlash(rel))
	var sum string
	switch {
	case err == nil:
		sum = gitstate.BlobSHA256(data)
	case errors.Is(err, os.ErrNotExist):
		sum = absentContent
	}
	proof.record(abs, sum)
	return data, sum
}

// A point save is not a discovery pass. Recheck every known input by content
// (not mtime), and check the caller's ancestors for a newly prepared project or
// alias scope. A directory event or explicit reconcile still discovers all roots.
func (in *resolutionInputs) unchangedForPoint(root, point string, proof *buildContentProof) bool {
	if in == nil || point == "" {
		return false
	}
	if !filepath.IsAbs(point) {
		point = filepath.Join(root, point)
	}
	rel, err := filepath.Rel(root, point)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	if info, err := os.Stat(point); err != nil || !info.Mode().IsRegular() {
		return false
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return false
	}
	defer confined.Close()
	for input, prior := range in.reads {
		if _, sum := readResolutionInput(confined, root, input, proof); sum == "" || prior != sum {
			return false
		}
	}
	tsNames, nuxtNames := tsalias.ConfigFileNames(), nuxtimports.ConfigFileNames()
	names := append(append(tsNames[:], nuxtNames[:]...), nuxtimports.GeneratedList)
	for dir := path.Dir(filepath.ToSlash(rel)); ; dir = path.Dir(dir) {
		for _, name := range names {
			candidate := path.Join(dir, name)
			if _, known := in.reads[candidate]; known {
				continue
			}
			abs := filepath.Join(root, filepath.FromSlash(candidate))
			_, err := confined.Stat(filepath.FromSlash(candidate))
			if !errors.Is(err, os.ErrNotExist) {
				return false
			}
			proof.record(abs, absentContent)
		}
		if dir == "." {
			return true
		}
	}
}

type resolutionInputStore interface {
	ResolutionInputDigest(repo string) (string, error)
	SetResolutionInputDigest(repo, digest string) error
}

func discoverResolutionInputs(root string, proof *buildContentProof) (*resolutionInputs, error) {
	proof.noteUnboundedReader()
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	t := &resolutionTree{Tree: nuxtimports.WorkingTree(root), root: root, dir: dir, reads: make(map[string]string), proof: proof}
	t.Tree.Files(func(rel string) { t.paths = append(t.paths, rel) })
	current := &resolutionInputs{aliases: tsalias.LoadScopes(t.Files, t.Read, t.Tree.Exists), nuxt: nuxtimports.LoadTree(t), reads: t.reads}
	h := sha256.New()
	for _, rel := range slices.Sorted(maps.Keys(current.reads)) {
		writeFingerprintString(h, rel)
		writeFingerprintString(h, current.reads[rel])
	}
	current.digest = hex.EncodeToString(h.Sum(nil))
	return current, nil
}

type resolutionCompletion struct {
	inputs   *resolutionInputs
	success  bool
	failed   []string // absolute paths; empty on a failure of unknown extent
	warm     bool
	frontier []string // nil means a deferred full build
}

type resolutionRetry struct {
	inputs *resolutionInputs
	files  map[string]bool // relative paths; nil means the pass did not complete discovery
}

func (idx *Indexer) resolutionStore() resolutionInputStore {
	s, _ := idx.graph.(resolutionInputStore)
	return s
}

// A clean source census does not cover ignored generator outputs.
func (idx *Indexer) censusResolutionCurrent(root string) bool {
	if idx.manifestTree().sourced() {
		return true
	}
	current, err := discoverResolutionInputs(root, nil)
	if err != nil {
		return false
	}
	store := idx.resolutionStore()
	if store == nil {
		prior := idx.appliedResolution.Load()
		return prior != nil && prior.digest == current.digest
	}
	stored, err := store.ResolutionInputDigest(idx.repoPrefix)
	if err == nil && stored == current.digest {
		idx.appliedResolution.Store(current)
		cacheResolutionInputs(root, current)
		return true
	}
	return false
}

func (idx *Indexer) refreshResolutionInputs(root, point string) (*resolutionInputs, error) {
	if idx.manifestTree().sourced() {
		return nil, nil
	}
	idx.resolutionMu.Lock()
	if pending := idx.warmResolution; pending != nil {
		// A new mutation overtook deferred warmup. Its incomplete pass cannot
		// certify this graph; recover exactly as after a restart.
		idx.resolutionRetry = &resolutionRetry{inputs: pending.inputs}
		idx.warmResolution = nil
	}
	idx.resolutionMu.Unlock()
	prior := idx.appliedResolution.Load()
	retry := idx.resolutionRetry
	if retry != nil {
		prior = retry.inputs
	}
	idx.resolutionInputsChanged = retry != nil && retry.files == nil
	current := prior
	if !prior.unchangedForPoint(root, point, idx.contentProof) {
		var err error
		current, err = discoverResolutionInputs(root, idx.contentProof)
		if err != nil {
			return nil, err
		}
		if prior != nil {
			idx.resolutionInputsChanged = idx.resolutionInputsChanged || !prior.aliases.Equal(current.aliases) || !prior.nuxt.Equal(current.nuxt)
		}
	}
	if store := idx.resolutionStore(); store != nil && prior == nil {
		stored, err := store.ResolutionInputDigest(idx.repoPrefix)
		if err != nil {
			return nil, err
		}
		if stored != current.digest {
			// A brand new scoped index has no callers to repair. Only
			// widen the request when this graph already owns JS/TS files.
			idx.resolutionInputsChanged = stored != "" || idx.hasResolutionCallers()
		}
	}
	cacheResolutionInputs(root, current)
	idx.activeResolution.Store(current)
	return current, nil
}

// Invalidate the certificate before binding rows can be replaced. A deferred
// resolver can leave a gap even when the input bytes themselves did not change.
func (idx *Indexer) fenceResolutionWrites(inputs *resolutionInputs, bindingWrites bool) error {
	if inputs == nil || (!idx.resolutionInputsChanged && !bindingWrites) {
		return nil
	}
	if store := idx.resolutionStore(); store != nil {
		if err := store.SetResolutionInputDigest(idx.repoPrefix, ""); err != nil {
			return err
		}
	}
	idx.appliedResolution.Store(nil)
	return nil
}

func (idx *Indexer) hasResolutionCallers() bool {
	var repos []string
	if idx.repoPrefix != "" {
		repos = []string{idx.repoPrefix}
	}
	for file := range graph.FileNodeIdentitiesSeq(idx.graph, repos) {
		if file.RepoPrefix == idx.repoPrefix && closureJSTSPath(file.FilePath) {
			return true
		}
	}
	return false
}

var resolutionCache = struct {
	sync.Mutex
	byRoot map[string]*resolutionInputs
}{byRoot: make(map[string]*resolutionInputs)}

func workingResolutionInputs(root string, proof *buildContentProof) *resolutionInputs {
	resolutionCache.Lock()
	inputs := resolutionCache.byRoot[root]
	if inputs == nil {
		var err error
		inputs, err = discoverResolutionInputs(root, proof)
		if err != nil {
			resolutionCache.Unlock()
			return &resolutionInputs{}
		}
		resolutionCache.byRoot[root] = inputs
		resolutionCache.Unlock()
		return inputs
	}
	resolutionCache.Unlock()
	if proof.active() {
		for rel, sum := range inputs.reads {
			proof.record(filepath.Join(root, filepath.FromSlash(rel)), sum)
		}
	}
	return inputs
}

func resolutionInputsForTree(tree manifestTree) *resolutionInputs {
	if tree.sourced() {
		src, _ := tree.(tsalias.Tree)
		return &resolutionInputs{aliases: tsalias.LoadTree(src), nuxt: nuxtimports.LoadTree(src)}
	}
	proven, _ := tree.(provenManifestTree)
	return workingResolutionInputs(tree.root(), proven.proof)
}

func invalidateResolutionInputs(root string) {
	resolutionCache.Lock()
	delete(resolutionCache.byRoot, root)
	resolutionCache.Unlock()
}

func cacheResolutionInputs(root string, current *resolutionInputs) {
	resolutionCache.Lock()
	resolutionCache.byRoot[root] = current
	resolutionCache.Unlock()
}

func (idx *Indexer) finishResolutionPass(work resolutionCompletion) (passErr error) {
	if dropped := idx.failedBindingWrites; len(dropped) > 0 {
		idx.failedBindingWrites = nil
		// Preserve a failure of unknown extent. Otherwise retry only callers
		// whose invalidation might have been dropped. Restart sees the fence.
		if work.success || len(work.failed) > 0 {
			for _, file := range dropped {
				if rel, owned := idx.graphPathRelKey(file); owned {
					work.failed = append(work.failed, filepath.Join(idx.rootPath, filepath.FromSlash(rel)))
				}
			}
		}
		work.success, work.warm = false, false
		passErr = fmt.Errorf("module invalidation did not apply; %d caller edges need retry", len(dropped))
	}
	if work.inputs == nil {
		return passErr
	}
	prior := idx.appliedResolution.Load()
	needsStamp := prior == nil || prior.digest != work.inputs.digest || idx.resolutionRetry != nil || idx.resolutionInputsChanged
	idx.resolutionInputsChanged = false
	if !work.warm {
		defer idx.activeResolution.Store(nil)
	}
	if !needsStamp && !work.warm && passErr == nil {
		return nil
	}
	if !work.success {
		retry := &resolutionRetry{inputs: work.inputs}
		if len(work.failed) != 0 {
			retry.files = make(map[string]bool, len(work.failed))
			for _, file := range work.failed {
				retry.files[idx.relKey(file)] = true
			}
		}
		idx.resolutionRetry = retry
		idx.appliedResolution.Store(nil)
	} else {
		idx.resolutionRetry = nil
	}
	if work.warm {
		idx.resolutionMu.Lock()
		idx.warmResolution = &work
		idx.resolutionMu.Unlock()
		return nil
	}
	if !work.success {
		return passErr
	}
	if err := idx.stampResolution(work.inputs); err != nil {
		idx.resolutionRetry = &resolutionRetry{inputs: work.inputs}
		return err
	}
	return nil
}

func (idx *Indexer) stampResolution(inputs *resolutionInputs) error {
	if store := idx.resolutionStore(); store != nil {
		if err := store.SetResolutionInputDigest(idx.repoPrefix, inputs.digest); err != nil {
			return err
		}
	}
	idx.appliedResolution.Store(inputs)
	return nil
}

func (idx *Indexer) inputsStaleForFile(rel string) bool {
	return closureJSTSPath(rel) && (idx.resolutionInputsChanged || idx.resolutionRetry != nil && idx.resolutionRetry.files[rel])
}

// Capture the passes before the warm resolver starts. A repository mutated in
// the meantime cancels its old completion in refreshResolutionInputs.
func (mi *MultiIndexer) warmResolutionPasses(scope map[string]struct{}) map[*Indexer]*resolutionCompletion {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	var pending map[*Indexer]*resolutionCompletion
	for prefix, idx := range mi.indexers {
		if _, ok := scope[prefix]; len(scope) > 0 && !ok {
			continue
		}
		idx.resolutionMu.Lock()
		work := idx.warmResolution
		idx.resolutionMu.Unlock()
		if work != nil {
			if pending == nil {
				pending = make(map[*Indexer]*resolutionCompletion)
			}
			pending[idx] = work
		}
	}
	return pending
}

func finishWarmResolutions(pending map[*Indexer]*resolutionCompletion) error {
	for idx, work := range pending {
		idx.resolutionMu.Lock()
		var err error
		if idx.warmResolution == work {
			if work.success {
				err = idx.stampResolution(work.inputs)
			}
			if err == nil {
				idx.warmResolution = nil
				idx.activeResolution.Store(nil)
			}
		}
		idx.resolutionMu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
