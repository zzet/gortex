package indexer

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"

	"go.uber.org/zap"
	"golang.org/x/mod/modfile"

	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/resolver"
)

func (mi *MultiIndexer) trackedGoModuleReplacements() map[string][]resolver.GoModuleReplacement {
	mi.goModuleSnapshot()
	mi.goModuleMu.Lock()
	defer mi.goModuleMu.Unlock()
	return mi.goModuleReplacements // immutable until the next snapshot replaces it
}

func (mi *MultiIndexer) readGoModuleReplacements(roots map[string]string) map[string][]resolver.GoModuleReplacement {
	replacements := make(map[string][]resolver.GoModuleReplacement)
	for repo, root := range roots {
		filename := filepath.Join(root, "go.mod")
		data, err := os.ReadFile(filename)
		if os.IsNotExist(err) {
			continue
		}
		var file *modfile.File
		if err == nil {
			file, err = modfile.Parse(filename, data, nil)
		}
		if err != nil {
			mi.logger.Warn("Go module replacements unavailable; refusing module binding for caller", zap.String("repo", repo), zap.Error(err))
			replacements[repo] = []resolver.GoModuleReplacement{{}}
			continue
		}
		for _, replacement := range file.Replace {
			if replacement.New.Version != "" {
				if replacement.New.Path != replacement.Old.Path {
					replacements[repo] = append(replacements[repo], resolver.GoModuleReplacement{Module: replacement.Old.Path})
				}
				continue
			}
			target := replacement.New.Path
			if !filepath.IsAbs(target) {
				target = filepath.Join(root, target)
			}
			target = pathkey.CanonicalExistingRoot(target)
			if pathkey.HasPathPrefix(target, pathkey.CanonicalExistingRoot(root)) {
				rel, err := filepath.Rel(pathkey.CanonicalExistingRoot(root), target)
				if err == nil {
					replacements[repo] = append(replacements[repo], resolver.GoModuleReplacement{
						Module: replacement.Old.Path, Directory: filepath.ToSlash(filepath.Join(repo, rel)),
					})
					continue
				}
			}
			tracked := false
			for _, provider := range roots {
				if pathkey.SamePathIdentity(target, pathkey.CanonicalExistingRoot(provider)) {
					tracked = true
					break
				}
			}
			if !tracked {
				replacements[repo] = append(replacements[repo], resolver.GoModuleReplacement{Module: replacement.Old.Path})
			}
		}
		slices.SortFunc(replacements[repo], func(a, b resolver.GoModuleReplacement) int {
			if order := cmp.Compare(a.Module, b.Module); order != 0 {
				return order
			}
			return cmp.Compare(a.Directory, b.Directory)
		})
	}
	return replacements
}
