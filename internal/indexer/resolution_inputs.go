package indexer

import (
	"errors"
	"os"
	"path"
	"path/filepath"

	"github.com/zzet/gortex/internal/gitstate"
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
		for _, name := range [...]string{"tsconfig.json", "jsconfig.json", ".nuxt/imports.d.ts"} {
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

func (idx *Indexer) refreshResolutionInputs(root, point string) *resolutionInputs {
	if idx.contentSrc.Load() != nil {
		return nil
	}
	prior := idx.appliedResolution
	idx.aliasResolutionChanged = idx.resolutionRetry
	current := prior
	if !prior.unchangedForPoint(root, point, idx.contentProof) {
		idx.contentProof.noteUnboundedReader()
		t := &resolutionTree{Tree: tsalias.WorkingTree(root), root: root, reads: make(map[string]string), proof: idx.contentProof}
		t.Tree.Files(func(rel string) { t.paths = append(t.paths, rel) })
		current = &resolutionInputs{aliases: tsalias.LoadTree(t), nuxt: nuxtimports.LoadTree(t), reads: t.reads}
		t.proof, t.paths, t.reads = nil, nil, nil
		if prior != nil && (!prior.aliases.Equal(current.aliases) || !prior.nuxt.Equal(current.nuxt)) {
			idx.aliasResolutionChanged = true
		}
	}
	tsAliasCacheMu.Lock()
	tsAliasCache[root] = current.aliases
	tsAliasCacheMu.Unlock()
	nuxtImportCache.Lock()
	nuxtImportCache.byRoot[root] = cachedNuxtImports{collection: current.nuxt, reads: current.reads}
	nuxtImportCache.Unlock()
	return current
}

func (idx *Indexer) finishResolutionInputs(inputs *resolutionInputs, success bool) {
	if inputs != nil {
		idx.resolutionRetry = !success && idx.aliasResolutionChanged
		if success {
			idx.appliedResolution = inputs
		}
	}
	idx.aliasResolutionChanged = false
}

func (idx *Indexer) mustReresolve(file string) bool {
	return idx.aliasResolutionChanged && closureJSTSPath(file)
}
