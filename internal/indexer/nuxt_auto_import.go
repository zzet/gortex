package indexer

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/parser/nuxtimports"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"github.com/zzet/gortex/internal/resolver"
)

type cachedNuxtImports struct {
	collection *nuxtimports.Collection
	reads      map[string]string // nil for a lazy load outside a mutation pass
}

var nuxtImportCache = struct {
	sync.Mutex
	byRoot map[string]cachedNuxtImports
}{byRoot: make(map[string]cachedNuxtImports)}

func loadNuxtImports(root string, proof *buildContentProof) *nuxtimports.Collection {
	nuxtImportCache.Lock()
	entry, exists := nuxtImportCache.byRoot[root]
	if exists {
		nuxtImportCache.Unlock()
		if proof.active() {
			entry.collection.ListPaths(func(rel string) {
				proof.record(filepath.Join(root, filepath.FromSlash(rel)), entry.reads[rel])
			})
		}
		return entry.collection
	}
	defer nuxtImportCache.Unlock()
	proof.noteUnboundedReader()
	coll := nuxtimports.Load(root)
	nuxtImportCache.byRoot[root] = cachedNuxtImports{collection: coll}
	return coll
}

func invalidateNuxtImports(root string) {
	nuxtImportCache.Lock()
	delete(nuxtImportCache.byRoot, root)
	nuxtImportCache.Unlock()
}

func resolveNuxtBinding(coll *nuxtimports.Collection, prefix, caller, name string) (string, string) {
	if prefix != "" {
		if !strings.HasPrefix(caller, prefix+"/") {
			return "", ""
		}
		caller = strings.TrimPrefix(caller, prefix+"/")
	}
	binding := coll.Resolve(caller, name)
	if binding.File == "" {
		return "", ""
	}
	return prefixTSAliasTarget(prefix, binding.File), binding.Name
}

func (idx *Indexer) resolveNuxtAutoImport(caller, name string) (string, string) {
	tree, ref := idx.manifestTreeWithRef()
	if ref == nil {
		return resolveNuxtBinding(loadNuxtImports(tree.root(), idx.contentProof), idx.repoPrefix, caller, name)
	}
	idx.nuxtMu.Lock()
	if idx.nuxtRef != ref {
		src, _ := tree.(tsalias.Tree)
		idx.nuxtColl = nuxtimports.LoadTree(src)
		idx.nuxtRef = ref
	}
	coll := idx.nuxtColl
	idx.nuxtMu.Unlock()
	return resolveNuxtBinding(coll, idx.repoPrefix, caller, name)
}

func (mi *MultiIndexer) nuxtAutoImportResolver() resolver.NuxtAutoImportResolver {
	return func(caller, name string) (string, string) {
		for _, meta := range mi.AllMetadata() {
			if meta.RepoPrefix != "" && strings.HasPrefix(caller, meta.RepoPrefix+"/") {
				if idx := mi.GetIndexer(meta.RepoPrefix); idx != nil && idx.contentSrc.Load() != nil {
					return idx.resolveNuxtAutoImport(caller, name)
				}
				return resolveNuxtBinding(loadNuxtImports(meta.RootPath, nil), meta.RepoPrefix, caller, name)
			}
		}
		return "", ""
	}
}
