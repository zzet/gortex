package indexer

import (
	"context"
	"errors"
	"io/fs"
	"iter"
	"path"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

// buildGoPackageOwnership stages immutable facts from the full target source.
// files must describe the same graph epoch; sparse parser admission is not an
// authority for missing manifests. No source, graph or filesystem is retained
// by the returned candidate callback.
func buildGoPackageOwnership(ctx context.Context, fullTarget source.ContentSource, repoPrefix string, files iter.Seq[*graph.Node]) (resolver.GoPackageOwnershipLookup, error) {
	if ctx == nil {
		return nil, errors.New("go package ownership needs a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fullTarget == nil || files == nil {
		return nil, nil
	}
	goFiles := make(map[string]string)
	for file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file == nil || file.Kind != graph.KindFile || file.Language != "go" || file.RepoPrefix != repoPrefix {
			continue
		}
		rel, owned := builderRelPath(repoPrefix, file.FilePath)
		if !owned {
			continue
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}
		goFiles[file.FilePath] = dir
	}
	return goPackageOwnershipFromFiles(ctx, fullTarget, repoPrefix, goFiles)
}

// goPackageOwnershipFromFiles consumes only compact Go path metadata. The
// source is borrowed for this call; only immutable strings escape the return.
func goPackageOwnershipFromFiles(ctx context.Context, fullTarget source.ContentSource, repoPrefix string, goFiles map[string]string) (resolver.GoPackageOwnershipLookup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fullTarget == nil || len(goFiles) == 0 {
		return nil, nil
	}
	dirs := make(map[string]struct{})
	for _, dir := range goFiles {
		dirs[dir] = struct{}{}
	}
	type manifestEvidence struct {
		boundary   bool
		modulePath string
	}
	manifests := make(map[string]manifestEvidence)
	readManifest := func(dir string) (manifestEvidence, error) {
		if cached, ok := manifests[dir]; ok {
			return cached, nil
		}
		name := path.Join(dir, "go.mod")
		data, _, err := source.ReadRegularFile(ctx, fullTarget, name, 64<<10)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return manifestEvidence{}, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return manifestEvidence{}, err
		}
		entry := manifestEvidence{boundary: !errors.Is(err, fs.ErrNotExist)}
		if err == nil {
			// Parse the selected module as an owner, not as a dependency whose
			// replace directives may be ignored. Unknown syntax or replacements
			// leave this boundary uncertified instead of inheriting an outer root.
			parsed, parseErr := modfile.Parse(name, data, nil)
			if parseErr == nil && parsed.Module != nil && len(parsed.Replace) == 0 {
				identity := parsed.Module.Mod.Path
				if module.CheckImportPath(identity) == nil && path.Clean(identity) == identity && !strings.Contains(identity, "\\") {
					entry.modulePath = identity
				}
			}
		}
		manifests[dir] = entry
		return entry, nil
	}
	// This implementation does not model workspace membership/replacements.
	// Any present or unreadable go.work in the selected source's ancestor
	// chain makes that directory Unknown. Never inspect a working-tree parent
	// outside the target source to fill a snapshot's missing authority.
	workspaceUnknown := make(map[string]bool)
	workspaceRead := make(map[string]bool)
	readWorkspace := func(dir string) (bool, error) {
		if workspaceRead[dir] {
			return workspaceUnknown[dir], nil
		}
		_, _, err := source.ReadRegularFile(ctx, fullTarget, path.Join(dir, "go.work"), 64<<10)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		unknown := !errors.Is(err, fs.ErrNotExist)
		workspaceRead[dir], workspaceUnknown[dir] = true, unknown
		return unknown, nil
	}
	packages := make(map[string]string, len(dirs))
	for dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vendored := false
		for _, component := range strings.Split(dir, "/") {
			if component == "vendor" {
				vendored = true
				break
			}
		}
		if vendored {
			continue
		}
		unsupportedWorkspace := false
		for ancestor := dir; ; {
			unknown, err := readWorkspace(ancestor)
			if err != nil {
				return nil, err
			}
			if unknown {
				unsupportedWorkspace = true
				break
			}
			if ancestor == "" {
				break
			}
			ancestor = path.Dir(ancestor)
			if ancestor == "." {
				ancestor = ""
			}
		}
		if unsupportedWorkspace {
			continue
		}
		for owner := dir; ; {
			entry, err := readManifest(owner)
			if err != nil {
				return nil, err
			}
			if entry.boundary {
				if entry.modulePath != "" {
					rel := strings.TrimPrefix(strings.TrimPrefix(dir, owner), "/")
					packages[dir] = path.Join(entry.modulePath, rel)
				}
				break
			}
			if owner == "" {
				break
			}
			owner = path.Dir(owner)
			if owner == "." {
				owner = ""
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func(candidate resolver.GoImportCandidate) resolver.GoPackageOwnershipResult {
		if candidate.ImporterRepoPrefix != repoPrefix || candidate.CandidateRepoPrefix != repoPrefix {
			return resolver.GoPackageOwnershipUnknown
		}
		if module.CheckImportPath(candidate.ImportPath) != nil {
			return resolver.GoPackageOwnershipUnknown
		}
		// The importing module can remap an import to another local module.
		// Candidate-only certification is therefore insufficient: require the
		// actual importer file's own clean module/workspace authority too.
		importerDir, importerKnown := goFiles[candidate.ImporterFilePath]
		if !importerKnown {
			return resolver.GoPackageOwnershipUnknown
		}
		if _, importerCertified := packages[importerDir]; !importerCertified {
			return resolver.GoPackageOwnershipUnknown
		}
		// Contract/virtual nodes and mixed-language rich candidates are not
		// certified merely because their directory contains Go source.
		if node := candidate.CandidateNode; node != nil && (node.Language != "go" || node.Kind == graph.KindContract) {
			return resolver.GoPackageOwnershipUnknown
		}
		dir, isGoFile := goFiles[candidate.CandidateFilePath]
		if !isGoFile {
			return resolver.GoPackageOwnershipUnknown
		}
		identity, known := packages[dir]
		if !known {
			return resolver.GoPackageOwnershipUnknown
		}
		if identity == candidate.ImportPath {
			return resolver.GoPackageOwnershipExact
		}
		return resolver.GoPackageOwnershipDifferent
	}, nil
}
