package indexer

import (
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/parser/nuxtimports"
	"github.com/zzet/gortex/internal/resolver"
)

var nuxtImportCache = struct {
	sync.Mutex
	byRoot map[string]*nuxtimports.Collection
}{byRoot: make(map[string]*nuxtimports.Collection)}

func loadNuxtImports(root string) *nuxtimports.Collection {
	nuxtImportCache.Lock()
	defer nuxtImportCache.Unlock()
	if coll, exists := nuxtImportCache.byRoot[root]; exists {
		return coll
	}
	coll := nuxtimports.Load(root)
	nuxtImportCache.byRoot[root] = coll
	return coll
}

func refreshNuxtImports(root string) bool {
	nuxtImportCache.Lock()
	defer nuxtImportCache.Unlock()
	previous, loaded := nuxtImportCache.byRoot[root]
	current := nuxtimports.Load(root)
	nuxtImportCache.byRoot[root] = current
	return loaded && !previous.Equal(current)
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
	return prefixTSAliasTarget(prefix, binding.Stem()), binding.Name
}

func (idx *Indexer) resolveNuxtAutoImport(caller, name string) (string, string) {
	tree, ref := idx.manifestTreeWithRef()
	if ref == nil {
		idx.contentProof.noteUnboundedReader()
		return resolveNuxtBinding(loadNuxtImports(tree.root()), idx.repoPrefix, caller, name)
	}
	idx.nuxtMu.Lock()
	if idx.nuxtRef != ref {
		idx.nuxtColl = nuxtimports.LoadTree(tree.(sourceManifestTree))
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
				return resolveNuxtBinding(loadNuxtImports(meta.RootPath), meta.RepoPrefix, caller, name)
			}
		}
		return "", ""
	}
}
