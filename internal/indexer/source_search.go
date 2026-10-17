package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/pathguard"
)

var ErrSourceSearchBudget = errors.New("source search resource budget exceeded")

// DecodeSourceSearchText uses the indexer's built-in encoding policy while
// preserving binary assets. Text search reads accepted source bytes, so it
// does not run configured commands or other parser-specific transforms.
func (idx *Indexer) DecodeSourceSearchText(path string, src []byte) []byte {
	if idx.transforms != nil {
		for _, transform := range idx.transforms.transforms {
			if decoder, ok := transform.(utf16DecodeTransform); ok && decoder.matches(path) {
				return decodeUTF16Source(src)
			}
		}
	}
	return src
}

// CurrentSourceSearchFiles enumerates admitted current sources without a graph
// route. Config/content/size exclusions are the indexer's, while ignore files
// are read from this checkout rather than cached from its canonical sibling.
// Scope is applied before reading sources and before any result limit.
func (idx *Indexer) CurrentSourceSearchFiles(ctx context.Context, root string, allow, allowDir func(string) bool, maxFiles int, patterns []string) ([]string, error) {
	ignores := excludes.NewHierarchical(root, dirIgnoreFiles...)
	matcher := idx.excludeMatcher()
	if patterns != nil {
		matcher = excludes.New(effectiveExcludePatterns(patterns))
	}
	content := idx.newContentAdmissionGate()
	untracked := idx.newUntrackedAssetGate(ctx, root)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && allowDir != nil && !allowDir(filepath.ToSlash(rel)) {
			return filepath.SkipDir
		}
		excluded := sourceSearchExcluded(matcher, ignores, root, path, entry.IsDir())
		if entry.IsDir() {
			if excluded && !ignores.HasNegatedDescendant(path) {
				m := matcher
				if m == nil || !m.HasNegatedDescendant(filepath.ToSlash(rel)) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if excluded || (allow != nil && !allow(filepath.ToSlash(rel))) {
			return nil
		}
		if pathguard.SymlinkEscapes(path, root) {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		adm := idx.admitUnexcludedWalkFile(path, info.Size())
		if !adm.admit {
			return nil
		}
		if _, skip := content.skip(adm.lang, info.Size()); skip {
			return nil
		}
		if _, skip := untracked.skip(adm.lang, path); skip {
			return nil
		}
		if maxFiles > 0 && len(files) >= maxFiles {
			return fmt.Errorf("%w: %d admitted files", ErrSourceSearchBudget, maxFiles)
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

func sourceSearchExcluded(matcher *excludes.Matcher, ignores *excludes.Hierarchical, root, path string, isDir bool) bool {
	if rel, err := filepath.Rel(root, path); err == nil && excludes.InAgentConfigDir(rel) {
		return !isDir && !excludes.IsMCPConfigFile(rel)
	}
	if matcher != nil && matcher.MatchAbsDir(path, root, isDir) {
		return true
	}
	return ignores.Match(path, isDir)
}

// AdmitSourceSearchBuffer applies the same corpus policy to overlay-only files.
func (idx *Indexer) AdmitSourceSearchBuffer(ctx context.Context, root, path string, content []byte, patterns []string) bool {
	matcher := idx.excludeMatcher()
	if patterns != nil {
		matcher = excludes.New(effectiveExcludePatterns(patterns))
	}
	if sourceSearchExcluded(matcher, excludes.NewHierarchical(root, dirIgnoreFiles...), root, path, false) {
		return false
	}
	lang, ok := idx.effectiveLanguage(path, content)
	if !ok || (idx.config.MaxFileSize > 0 && int64(len(content)) > idx.config.MaxFileSize) {
		return false
	}
	if _, skip := idx.newContentAdmissionGate().skip(lang, int64(len(content))); skip {
		return false
	}
	if _, skip := idx.newUntrackedAssetGate(ctx, root).skip(lang, path); skip {
		return false
	}
	return true
}
