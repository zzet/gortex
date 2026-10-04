package indexer

import (
	"iter"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// The resolver's dependency-module index reads a repository's dep:: contract
// identities (one per go.mod requirement). Through a delta that was a composed
// full-row read of every contract node of the repository (3.3 s on a large
// store), the same on every delta over a stack: dep:: contracts come from the
// module manifests, and a per-file delta over source files re-derives none.
// They are kept per stack and repository, unless the delta's change set holds
// a manifest (go.mod, go.work), when the delta's own view is read.

type editDeltaDepEntry struct {
	key  string
	rows []graph.RepoNodeIdentity
}

var editDeltaDepCache struct {
	sync.Mutex
	entries []editDeltaDepEntry // most recent last
}

func cachedEditDeltaDeps(key string) ([]graph.RepoNodeIdentity, bool) {
	editDeltaDepCache.Lock()
	defer editDeltaDepCache.Unlock()
	for i := len(editDeltaDepCache.entries) - 1; i >= 0; i-- {
		if editDeltaDepCache.entries[i].key == key {
			return editDeltaDepCache.entries[i].rows, true
		}
	}
	return nil, false
}

func storeEditDeltaDeps(key string, rows []graph.RepoNodeIdentity) {
	editDeltaDepCache.Lock()
	defer editDeltaDepCache.Unlock()
	editDeltaDepCache.entries = append(editDeltaDepCache.entries, editDeltaDepEntry{key: key, rows: rows})
	if over := len(editDeltaDepCache.entries) - 4*editDeltaContractCacheEntries; over > 0 {
		editDeltaDepCache.entries = append([]editDeltaDepEntry(nil), editDeltaDepCache.entries[over:]...)
	}
}

// resetEditDeltaDeps empties the cache (tests).
func resetEditDeltaDeps() {
	editDeltaDepCache.Lock()
	editDeltaDepCache.entries = nil
	editDeltaDepCache.Unlock()
}

// depContractRows reads the dep:: contract identities of one repository.
func depContractRows(g graph.Store, repo string) []graph.RepoNodeIdentity {
	var rows []graph.RepoNodeIdentity
	for row := range graph.RepoNodeIdentitiesSeq(g, []string{repo}, graph.KindContract) {
		if strings.HasPrefix(row.ID, "dep::") {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// changeSetHasManifest reports whether a change set holds a module manifest.
func changeSetHasManifest(changedRel []string) bool {
	for _, rel := range changedRel {
		switch path.Base(rel) {
		case "go.mod", "go.work":
			return true
		}
	}
	return false
}

// installEditDeltaDeps sets the per-stack dependency-contract source on idx's
// resolver; a change set holding a manifest installs nothing, and neither
// does a dirty chain below the delta that speaks for a manifest or a dep::
// identity (chainTouched, graph paths): the rows are kept for the stack below
// the chain.
func installEditDeltaDeps(idx *Indexer, key string, changedRel []string, chainTouched map[string]struct{}) {
	if idx == nil || idx.resolver == nil || key == "" || changeSetHasManifest(changedRel) {
		return
	}
	for p := range chainTouched {
		if p == "dep" || changeSetHasManifest([]string{p}) {
			return
		}
	}
	view := idx.graph
	source := func(repos []string) iter.Seq[graph.RepoNodeIdentity] {
		return func(yield func(graph.RepoNodeIdentity) bool) {
			for _, repo := range repos {
				repoKey := key + "\x00" + repo
				singleFlight("deps\x00"+repoKey, func() bool { _, ok := cachedEditDeltaDeps(repoKey); return ok }, func() {
					storeEditDeltaDeps(repoKey, depContractRows(view, repo))
				})
				rows, _ := cachedEditDeltaDeps(repoKey)
				for _, row := range rows {
					if !yield(row) {
						return
					}
				}
			}
		}
	}
	idx.resolver.SetDepContractSource(source)
	editDeltaDepSources.Store(idx.resolver, source)
}

// editDeltaDepSources records, per resolver, the installed source (tests).
var editDeltaDepSources sync.Map // *resolver.Resolver -> func([]string) iter.Seq[graph.RepoNodeIdentity]
