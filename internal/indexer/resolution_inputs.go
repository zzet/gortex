package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	data, sum := readResolutionInput(t.root, rel, t.proof)
	t.reads[rel] = sum
	return data, sum != "" && sum != absentContent
}

func readResolutionInput(root, rel string, proof *buildContentProof) ([]byte, string) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
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
	for input, prior := range in.reads {
		if _, sum := readResolutionInput(root, input, proof); sum == "" || prior != sum {
			return false
		}
	}
	for dir := path.Dir(filepath.ToSlash(rel)); ; dir = path.Dir(dir) {
		for _, name := range [...]string{"tsconfig.json", "jsconfig.json", nuxtimports.GeneratedList} {
			candidate := path.Join(dir, name)
			if _, known := in.reads[candidate]; known {
				continue
			}
			abs := filepath.Join(root, filepath.FromSlash(candidate))
			_, err := os.Stat(abs)
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

func discoverResolutionInputs(root string, proof *buildContentProof) *resolutionInputs {
	proof.noteUnboundedReader()
	t := &resolutionTree{Tree: tsalias.WorkingTree(root, nuxtimports.GeneratedList), root: root, reads: make(map[string]string), proof: proof}
	t.Tree.Files(func(rel string) { t.paths = append(t.paths, rel) })
	current := &resolutionInputs{aliases: tsalias.LoadTree(t), nuxt: nuxtimports.LoadTree(t), reads: t.reads}
	t.proof, t.paths, t.reads = nil, nil, nil
	h := sha256.New()
	for _, rel := range slices.Sorted(maps.Keys(current.reads)) {
		writeFingerprintString(h, rel)
		writeFingerprintString(h, current.reads[rel])
	}
	current.digest = hex.EncodeToString(h.Sum(nil))
	return current
}

// A clean source census does not cover ignored generator outputs.
func (idx *Indexer) censusResolutionCurrent(root string) bool {
	if idx.contentSrc.Load() != nil {
		return true
	}
	current := discoverResolutionInputs(root, nil)
	store, ok := idx.graph.(resolutionInputStore)
	if !ok {
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
	if idx.contentSrc.Load() != nil {
		return nil, nil
	}
	prior := idx.appliedResolution.Load()
	idx.aliasResolutionChanged = idx.resolutionRetry
	current := prior
	if !prior.unchangedForPoint(root, point, idx.contentProof) {
		current = discoverResolutionInputs(root, idx.contentProof)
		if prior != nil {
			idx.aliasResolutionChanged = idx.aliasResolutionChanged || !prior.aliases.Equal(current.aliases) || !prior.nuxt.Equal(current.nuxt)
		}
	}
	if store, ok := idx.graph.(resolutionInputStore); ok && (prior == nil || prior.digest != current.digest || idx.resolutionRetry) {
		stored, err := store.ResolutionInputDigest(idx.repoPrefix)
		if err != nil {
			return nil, err
		}
		if prior == nil && stored != current.digest {
			// A brand new scoped index has no callers to repair. Only
			// widen the request when this graph already owns JS/TS files.
			idx.aliasResolutionChanged = stored != "" || idx.resolutionRetry || idx.hasResolutionCallers()
		}
		if stored != "" && stored != current.digest && idx.aliasResolutionChanged {
			// Fence before the first graph write, not after a partial rebuild.
			if err := store.SetResolutionInputDigest(idx.repoPrefix, ""); err != nil {
				return nil, err
			}
		}
	}
	cacheResolutionInputs(root, current)
	return current, nil
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
		inputs = discoverResolutionInputs(root, proof)
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

func (idx *Indexer) finishResolutionInputs(inputs *resolutionInputs, success bool) error {
	if inputs != nil {
		idx.resolutionRetry = !success && idx.aliasResolutionChanged
		if success {
			prior := idx.appliedResolution.Load()
			if store, ok := idx.graph.(resolutionInputStore); ok && (prior == nil || prior.digest != inputs.digest || idx.aliasResolutionChanged) {
				if err := store.SetResolutionInputDigest(idx.repoPrefix, inputs.digest); err != nil {
					idx.resolutionRetry = true
					return err
				}
			}
			idx.appliedResolution.Store(inputs)
		} else if idx.resolutionRetry {
			idx.appliedResolution.Store(nil)
		}
	}
	idx.aliasResolutionChanged = false
	return nil
}

func (idx *Indexer) mustReresolve(file string) bool {
	return idx.aliasResolutionChanged && closureJSTSPath(file)
}
