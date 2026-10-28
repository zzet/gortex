package indexer

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ResolveConfiguredRepos selects one configured root per prefix for startup
// and lifecycle discovery. Live ownership wins. A published catalog owner
// wins only while configured under that prefix; otherwise config order decides.
// Returned entries pin the resolved name without changing the config list.
func (mi *MultiIndexer) ResolveConfiguredRepos(ctx context.Context, entries []config.RepoEntry) []config.RepoEntry {
	canonicalize := mi.canonicalRoot
	if canonicalize == nil {
		canonicalize = pathkey.CanonicalPath
	}
	if mi.configMgr != nil {
		snapshot := mi.configMgr.RepoPathSnapshot()
		fallback := canonicalize
		canonicalize = func(root string) string { return snapshot.CanonicalRoot(root, fallback) }
	}
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
	type registeredRoot struct{ root, prefix string }
	registeredRoots := make(map[string][]registeredRoot, len(metadata))
	for prefix, meta := range metadata {
		if meta != nil {
			root := canonicalize(meta.RootPath)
			key := configuredRootKey(root)
			registeredRoots[key] = append(registeredRoots[key], registeredRoot{root, prefix})
		}
	}
	prefixes, roots := make([]string, len(entries)), make([]string, len(entries))
	configured := make(map[string][]string)
	for i, entry := range entries {
		roots[i] = canonicalize(entry.Path)
		prefixes[i] = EffectiveRepoPrefix(mi.configMgr, entry)
		for _, registered := range registeredRoots[configuredRootKey(roots[i])] {
			if pathkey.SamePathIdentity(registered.root, roots[i]) {
				prefixes[i] = registered.prefix
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
					_, published, err := catalog.DedicatedBasePublication(ctx, binding.GraphID)
					if err != nil {
						mi.logger.Warn("reading configured repository publication; using config order", zap.Error(err))
					}
					if err != nil || !published {
						break
					}
					checkout, found, err := catalog.GetCheckout(ctx, binding.OwnerCheckoutID)
					if err != nil {
						mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
					} else if found {
						root := canonicalize(checkout.RootPath)
						for _, configuredRoot := range configured[prefix] {
							if pathkey.SamePathIdentity(root, configuredRoot) {
								owner, held = checkout.RootPath, true
								break
							}
						}
					}
				}
			}
			if implicitInstance && held && !pathkey.SamePathIdentity(canonicalize(owner), abs) && !sameDirectory(owner, abs) {
				prefix = WorktreeInstanceCollisionName(prefix, abs)
				implicitInstance = false // Only one suffix, then enforce ownership normally.
				continue
			}
			break
		}
		if held && !pathkey.SamePathIdentity(canonicalize(owner), abs) && !sameDirectory(owner, abs) {
			mi.logCollision(NewPrefixCollisionError(prefix, owner, abs))
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

// configuredPrefixCollision protects configured owners that warmup has not
// loaded yet. Filter by base name before resolving workspaces: the ordinary
// one-root case needs no workspace reads or catalog ownership pass.
func (mi *MultiIndexer) configuredPrefixCollision(prefix, root string) error {
	return mi.configuredPrefixCollisionBefore(context.Background(), prefix, root, time.Now().Add(prefixOwnershipTimeout))
}

func (mi *MultiIndexer) configuredPrefixCollisionBefore(ctx context.Context, prefix, root string, before time.Time) error {
	if mi.configMgr == nil {
		return nil
	}
	candidates := mi.configMgr.RepoTrackSnapshot().InstanceCandidates(prefix)
	if len(candidates) == 1 && !strings.Contains(prefix, "@") && candidates[0].Path == root && config.ResolvePrefix(candidates[0]) == prefix {
		return nil
	}
	var entries []config.RepoEntry
	for _, entry := range candidates {
		effective, err := mi.effectiveTrackPrefix(ctx, entry, before)
		if err != nil {
			return err
		}
		if effective == prefix {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	budget, cancel := context.WithDeadline(ctx, before)
	defer cancel()
	observed := make(map[string]prefixStatResult)
	stat := func(path string) prefixStatResult {
		if result, ok := observed[path]; ok {
			return result
		}
		result := mi.prefixStats.observe(budget, path)
		observed[path] = result
		return result
	}
	same := func(a, b string) bool {
		return a == b || sameObservedDirectory(stat(a), stat(b))
	}
	owner := entries[0].Path
	// Match startup's priority: a live owner, then a published catalog owner
	// still configured under this prefix, then configuration order.
	if live := mi.GetMetadata(prefix); live != nil {
		configured := false
		for _, entry := range entries {
			if same(entry.Path, live.RootPath) {
				owner, configured = entry.Path, true
				break
			}
		}
		if !configured {
			return nil // live ownership is checked separately at admission
		}
	} else if provider, ok := mi.graph.(interface{ Catalog() *store_sqlite.Catalog }); ok && provider.Catalog() != nil {
		catalog := provider.Catalog()
		binding, found, err := catalog.GetDedicatedGraph(budget, GraphIDFor(prefix))
		if err != nil {
			mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
		}
		if err == nil && found {
			_, published, err := catalog.DedicatedBasePublication(budget, binding.GraphID)
			if err != nil {
				mi.logger.Warn("reading configured repository publication; using config order", zap.Error(err))
			}
			if err == nil && published {
				checkout, found, err := catalog.GetCheckout(budget, binding.OwnerCheckoutID)
				if err != nil {
					mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
				}
				if err == nil && found {
					for _, entry := range entries {
						if same(entry.Path, checkout.RootPath) {
							owner = entry.Path
							break
						}
					}
				}
			}
		}
	}
	if same(owner, root) {
		return nil
	}
	return newObservedPrefixCollision(prefix, owner, root, stat(owner))
}

// Bucket only; SamePathIdentity still distinguishes case-sensitive directories.
func configuredRootKey(root string) string {
	root = pathkey.Normalize(root)
	if pathkey.CaseInsensitivePaths {
		return strings.ToLower(root)
	}
	return root
}

// Each registration resolves configured ownership once. A changed repository
// snapshot invalidates the result before the coordinated recheck. Live ownership
// is checked separately under the registry lock at every admission boundary.
func (mi *MultiIndexer) configuredPrefixCheck() func(string, string) error {
	return mi.configuredPrefixCheckContext(context.Background(), time.Now().Add(prefixOwnershipTimeout))
}

func (mi *MultiIndexer) configuredPrefixCheckContext(ctx context.Context, before time.Time) func(string, string) error {
	var snapshot *config.RepoTrackSnapshot
	var checkedPrefix, checkedRoot string
	var collision error
	return func(prefix, root string) error {
		if mi.configMgr == nil {
			return nil
		}
		current := mi.configMgr.RepoTrackSnapshot()
		if current != snapshot || prefix != checkedPrefix || root != checkedRoot {
			collision = mi.configuredPrefixCollisionBefore(ctx, prefix, root, before)
			snapshot, checkedPrefix, checkedRoot = current, prefix, root
		}
		return collision
	}
}
