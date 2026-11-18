package indexer

import (
	"context"
	"iter"
	"path"

	"golang.org/x/mod/modfile"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

type goPackageSource struct {
	// A nonnil ref records explicit source authority, even when its full
	// manifest source is unavailable. Only a nil ref permits filesystem mode.
	ref  *contentSourceRef
	root string
}

// setContentSourceWithManifests is setup-only. Parsing may be narrowed to a
// closure, while manifest authority remains the full selected target. Both
// references move together; no reader infers the parent of a source wrapper.
func (idx *Indexer) setContentSourceWithManifests(src, manifests source.ContentSource) {
	if src == nil {
		idx.contentSrc.Store(nil)
		return
	}
	idx.contentSrc.Store(&contentSourceRef{src: src, manifests: manifests})
}

func (idx *Indexer) prepareGoPackageOwnership(ctx context.Context, prefixes []string, files iter.Seq[*graph.Node]) (map[string]resolver.GoPackageOwnershipLookup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	targets := make(map[string]goPackageSource)
	for _, prefix := range prefixes {
		if prefix == idx.repoPrefix {
			targets[prefix] = goPackageSource{ref: idx.contentSrc.Load(), root: idx.rootPath}
		} else if mi := idx.repositoryMutationOwner; mi != nil {
			// A provider save also resolves surviving callers in other repos.
			if caller := mi.GetIndexer(prefix); caller != nil {
				targets[prefix] = goPackageSource{ref: caller.contentSrc.Load(), root: caller.rootPath}
			}
		}
	}
	return prepareGoPackageSources(ctx, targets, files)
}

func (mi *MultiIndexer) prepareGoPackageOwnership(ctx context.Context, prefixes []string, files iter.Seq[*graph.Node]) (map[string]resolver.GoPackageOwnershipLookup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	targets := make(map[string]goPackageSource)
	for _, prefix := range prefixes {
		// GetIndexer holds mi.mu only for this lookup. No graph enumeration,
		// Git/filesystem I/O or resolver calls occur under the registry lock.
		idx := mi.GetIndexer(prefix)
		if idx != nil && idx.repoPrefix == prefix {
			targets[prefix] = goPackageSource{ref: idx.contentSrc.Load(), root: idx.rootPath}
		}
	}
	return prepareGoPackageSources(ctx, targets, files)
}

func prepareGoPackageSources(ctx context.Context, targets map[string]goPackageSource, files iter.Seq[*graph.Node]) (map[string]resolver.GoPackageOwnershipLookup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 || files == nil {
		return nil, nil
	}
	// One scoped graph iteration, independent of repository count. It finishes
	// before source reads so a cursor/connection is not held across manifest I/O.
	byRepo := make(map[string]map[string]string)
	for file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file == nil || file.Kind != graph.KindFile || file.Language != "go" {
			continue
		}
		if _, covered := targets[file.RepoPrefix]; !covered {
			continue
		}
		rel, owned := builderRelPath(file.RepoPrefix, file.FilePath)
		if !owned {
			continue
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}
		if byRepo[file.RepoPrefix] == nil {
			byRepo[file.RepoPrefix] = make(map[string]string)
		}
		byRepo[file.RepoPrefix][file.FilePath] = dir
	}
	lookups := make(map[string]resolver.GoPackageOwnershipLookup, len(targets))
	for prefix, target := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		goFiles := byRepo[prefix]
		if len(goFiles) == 0 {
			continue
		}
		lookup, err := target.prepare(ctx, prefix, goFiles)
		if err != nil {
			return nil, err
		}
		lookups[prefix] = lookup
	}
	return lookups, nil
}

func (target goPackageSource) prepare(ctx context.Context, prefix string, goFiles map[string]string) (resolver.GoPackageOwnershipLookup, error) {
	if target.ref != nil {
		// This is the decisive no-filesystem-fallback branch for Git/sparse
		// snapshots. Parser admission and manifest authority share one ref.
		return goPackageOwnershipFromFiles(ctx, target.ref.manifests, prefix, goFiles)
	}
	if target.root == "" {
		return nil, nil
	}
	fs, err := source.NewFilesystemSource(target.root)
	if err != nil {
		return nil, nil
	}
	defer fs.Close()
	return goPackageOwnershipFromFiles(ctx, fs, prefix, goFiles)
}

// Standalone builders have no MultiIndexer, but still own their root module.
// Read its identity from the same full manifest source as package ownership.
func (idx *Indexer) ownGoModules() map[string]string {
	if idx.rootPath == "" {
		return nil
	}
	data, ok := idx.manifestTree().readFile("go.mod")
	if !ok {
		return nil
	}
	module := modfile.ModulePath(data)
	if module == "" {
		return nil
	}
	return map[string]string{idx.repoPrefix: module}
}
