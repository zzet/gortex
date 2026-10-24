package indexer

import (
	"context"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ResolveConfiguredRepos selects one root per prefix before parallel warmup.
// Live ownership wins; catalog ownership wins only for a configured root
// under that prefix. Otherwise config order decides.
// Returned entries pin the resolved name without changing the config list.
func (mi *MultiIndexer) ResolveConfiguredRepos(ctx context.Context, entries []config.RepoEntry) []config.RepoEntry {
	owners := make(map[string]string)
	metadata := mi.AllMetadata()
	for prefix, meta := range metadata {
		if meta != nil {
			owners[prefix] = meta.RootPath
		}
	}
	var catalog *store_sqlite.Catalog
	if provider, ok := mi.graph.(interface{ Catalog() *store_sqlite.Catalog }); ok {
		catalog = provider.Catalog()
	}
	registeredRoots := make(map[string]string, len(metadata))
	for prefix, meta := range metadata {
		if meta != nil {
			registeredRoots[pathkey.CanonicalPath(meta.RootPath)] = prefix
		}
	}
	prefixes, roots := make([]string, len(entries)), make([]string, len(entries))
	configured := make(map[string][]string)
	for i, entry := range entries {
		roots[i] = pathkey.CanonicalPath(entry.Path)
		prefixes[i] = EffectiveRepoPrefix(mi.configMgr, entry)
		for root, registered := range registeredRoots {
			if pathkey.SamePathIdentity(root, roots[i]) {
				prefixes[i] = registered
				break
			}
		}
		configured[prefixes[i]] = append(configured[prefixes[i]], roots[i])
	}
	seen := make(map[string]bool)
	out := make([]config.RepoEntry, 0, len(entries))
	for i, entry := range entries {
		abs, prefix := roots[i], prefixes[i]
		if prefix == "" || prefix == "." {
			continue
		}
		var owner string
		var held bool
		implicitInstance := entry.Name == "" && prefix != config.ResolvePrefix(entry)
		for {
			owner, held = owners[prefix]
			if !held && catalog != nil {
				binding, found, err := catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
				if err != nil {
					mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
				} else if found {
					checkout, found, err := catalog.GetCheckout(ctx, binding.OwnerCheckoutID)
					if err != nil {
						mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
					} else if found {
						root := pathkey.CanonicalPath(checkout.RootPath)
						for _, configuredRoot := range configured[prefix] {
							if pathkey.SamePathIdentity(root, configuredRoot) {
								owner, held = checkout.RootPath, true
								break
							}
						}
					}
				}
			}
			if implicitInstance && held && !pathkey.SamePathIdentity(pathkey.CanonicalPath(owner), abs) && !sameDirectory(owner, abs) {
				prefix = WorktreeInstanceCollisionName(prefix, abs)
				implicitInstance = false // Only one suffix, then enforce ownership normally.
				continue
			}
			break
		}
		if held && !pathkey.SamePathIdentity(pathkey.CanonicalPath(owner), abs) && !sameDirectory(owner, abs) {
			mi.logCollision(&PrefixCollisionError{Prefix: prefix, ExistingPath: owner, Path: abs})
			continue
		}
		if seen[prefix] {
			continue
		}
		owners[prefix], seen[prefix] = abs, true
		entry.Name = prefix
		out = append(out, entry)
	}
	return out
}
