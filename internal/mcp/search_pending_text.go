package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

type pendingTextFile struct {
	abs, path, relative string
	indexer             *indexer.Indexer
}

// Reuse the physical pending-source registry for refused projections, and the
// immutable projection snapshot for admitted ones. Neither receipt-ledger
// retention nor a second publisher/cache determines search completeness.
func (s *Server) pendingTextFiles(ctx context.Context, resolved ResolvedScope, pathFilters []string) ([]pendingTextFile, error) {
	if requestReadsCommittedTree(ctx) || requestViewFromContext(ctx).routed() {
		return nil, nil
	}
	if err := validateRequestProjections(ctx); err != nil {
		return nil, err
	}
	entries := pendingFileProjections.snap.Load().entries
	registry := &s.pendingSourceFiles
	registry.mu.Lock()
	records := make([]*mutationCommitRecord, 0, len(registry.versions))
	for _, record := range registry.versions {
		records = append(records, record)
	}
	registry.mu.Unlock()
	if len(entries) == 0 && len(records) == 0 {
		return nil, nil
	}
	paths := make(map[string]struct{}, len(entries)+len(records))
	for _, entry := range entries {
		owner := s.pickIndexerForPath(entry.absPath)
		if owner == nil || owner.RepoPrefix() != entry.repoPrefix {
			continue
		}
		if pathkey.MutationKey(entry.absPath) != entry.key {
			return nil, fmt.Errorf("pending text physical identity changed")
		}
		paths[entry.absPath] = struct{}{}
	}
	for _, record := range records {
		snap := record.snapshot()
		if record.owner == s && snap.DiskStatus == mutationDiskCommitted && snap.GraphStatus != mutationGraphFresh {
			paths[record.absPath] = struct{}{}
		}
	}
	var prefixes []string
	if s.multiIndexer != nil {
		prefixes = s.multiIndexer.RepoPrefixes()
	}
	filters := expandPathPrefixesWithRepos(normalizePathPrefixes(pathFilters), prefixes)
	files := make([]pendingTextFile, 0, len(paths))
	admission := make(map[*indexer.Indexer]func(string) bool)
	for abs := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owner := s.pickIndexerForPath(abs)
		if owner == nil {
			continue
		}
		relative, inside := relativeWithinRoot(owner.RootPath(), abs)
		if !inside || pathkey.MutationKey(filepath.Join(owner.RootPath(), relative)) != pathkey.MutationKey(abs) {
			continue
		}
		prefix := owner.RepoPrefix()
		path := filepath.ToSlash(relative)
		if prefix != "" {
			path = prefix + "/" + path
		}
		if resolved.RepoAllow != nil && !resolved.RepoAllow[prefix] || resolved.WorkspaceID != "" && resolved.WorkspaceID != owner.WorkspaceID() || resolved.ProjectID != "" && resolved.ProjectID != owner.ProjectID() || len(filters) > 0 && !pathMatchesAnyPrefix(path, filters) {
			continue
		}
		allow := admission[owner]
		if allow == nil {
			var patterns []string
			if s.configManager != nil {
				patterns = s.configManager.EffectiveExcludeForRoot(prefix, owner.RootPath())
			}
			allow = owner.PendingSourceSearchPathFilter(owner.RootPath(), patterns)
			admission[owner] = allow
		}
		if !allow(abs) {
			continue
		}
		files = append(files, pendingTextFile{abs: abs, path: path, relative: filepath.ToSlash(relative), indexer: owner})
		if len(files) > sourceSearchMaxFiles {
			return nil, fmt.Errorf("%w: pending file bound", indexer.ErrSourceSearchBudget)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func (s *Server) scanPendingTextFiles(ctx context.Context, files []pendingTextFile, query string, expression *regexp.Regexp, limit int) ([]enrichedTextMatch, map[string]map[string]struct{}, error) {
	matches := make([]enrichedTextMatch, 0)
	excluded := make(map[string]map[string]struct{})
	total := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		prefix := file.indexer.RepoPrefix()
		if excluded[prefix] == nil {
			excluded[prefix] = make(map[string]struct{})
		}
		excluded[prefix][file.relative] = struct{}{}
		budget := min(int64(64<<20), int64(sourceSearchMaxBytes-total))
		if budget <= 0 {
			return nil, nil, fmt.Errorf("%w: pending byte bound", indexer.ErrSourceSearchBudget)
		}
		raw, evidence, err := readPhysicalFileEvidenceBounded(file.abs, budget)
		if os.IsNotExist(err) {
			continue
		} // actual absence masks old postings too
		if err != nil {
			return nil, nil, fmt.Errorf("current pending text unavailable: %w", err)
		}
		if !(viewPathRoot{root: file.indexer.RootPath()}).contains(evidence.resolvedPath) {
			return nil, nil, fmt.Errorf("current pending text escapes owning repository")
		}
		total += len(raw)
		// Verify every owned file even after the output page fills. A later file's
		// read refusal must not silently turn into a complete old-corpus answer.
		if len(matches) < limit {
			current, err := s.scanSourceSearchText(ctx, file.indexer, file.abs, file.path, raw, query, expression, limit-len(matches))
			if err != nil {
				return nil, nil, err
			}
			matches = append(matches, current...)
		}
	}
	return matches, excluded, nil
}
