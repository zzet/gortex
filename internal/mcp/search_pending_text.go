package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathguard"
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
	if err := s.validatePendingTextKeys(ctx, entries); err != nil {
		return nil, err
	}
	paths := make(map[string]struct{}, len(entries)+len(records))
	for _, entry := range entries {
		owner := s.pickIndexerForPath(entry.absPath)
		if owner == nil || owner.RepoPrefix() != entry.repoPrefix {
			continue
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
		if !inside {
			continue
		}
		// Identical cleaned inputs cannot have different MutationKeys. Keep the
		// real alias check for distinct spellings and the entry-key check above.
		joined := filepath.Join(owner.RootPath(), relative)
		if filepath.Clean(joined) != filepath.Clean(abs) && pathkey.MutationKey(joined) != pathkey.MutationKey(abs) {
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

// Validate the same immutable entry keys before selection, amortizing only
// the absolute repository prefix. At most one grouped root/ancestor stack is
// open, and no key is accepted until its complete directory/root fence closes.
// The later physical-byte reader and its mutation token remain independent.
func (s *Server) validatePendingTextKeys(ctx context.Context, entries map[string]*projectionEntry) (err error) {
	type selected struct {
		entry *projectionEntry
		owner *indexer.Indexer
	}
	ordered := make([]selected, 0, len(entries))
	for _, entry := range entries {
		owner := s.pickIndexerForPath(entry.absPath)
		if owner != nil && owner.RepoPrefix() == entry.repoPrefix {
			ordered = append(ordered, selected{entry, owner})
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.owner.RootPath() != b.owner.RootPath() {
			return a.owner.RootPath() < b.owner.RootPath()
		}
		return a.entry.absPath < b.entry.absPath
	})
	var active *pendingTextRoot
	var activeOwner *indexer.Indexer
	defer func() {
		if active != nil {
			err = errors.Join(err, active.close(ctx))
		}
	}()
	for _, item := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, owner := item.entry, item.owner
		relative, inside := relativeWithinRoot(owner.RootPath(), entry.absPath)
		var key string
		if !inside {
			// Keep the old physical spelling check for non-lexical aliases; path
			// selection below still applies its existing owning-root admission.
			key = pathkey.MutationKey(entry.absPath)
		} else {
			if owner != activeOwner {
				if active != nil {
					closed := active.close(ctx)
					active = nil
					if closed != nil {
						return closed
					}
				}
				active, err = openPendingTextRootForScan(owner.RootPath())
				if err != nil {
					return err
				}
				activeOwner = owner
			}
			key, err = active.mutationKey(ctx, relative)
			if err != nil {
				return err
			}
		}
		if key != entry.key {
			return fmt.Errorf("pending text physical identity changed")
		}
	}
	return ctx.Err()
}

func (s *Server) scanPendingTextFiles(ctx context.Context, files []pendingTextFile, query string, expression *regexp.Regexp, limit int) (matches []enrichedTextMatch, excluded map[string]map[string]struct{}, err error) {
	matches = make([]enrichedTextMatch, 0)
	excluded = make(map[string]map[string]struct{})
	var active *pendingTextRoot
	var activeOwner *indexer.Indexer
	defer func() {
		if active != nil {
			if closeErr := active.close(ctx); closeErr != nil {
				matches, excluded, err = nil, nil, errors.Join(err, closeErr)
			}
		}
	}()
	total := 0
	verificationBuffer := make([]byte, 32<<10)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if activeOwner != file.indexer {
			if active != nil {
				closeErr := active.close(ctx)
				active = nil
				if closeErr != nil {
					return nil, nil, closeErr
				}
			}
			active, err = openPendingTextRootForScan(file.indexer.RootPath())
			if err != nil {
				return nil, nil, err
			}
			activeOwner = file.indexer
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
		raw, evidence, err := active.read(ctx, file.relative, nil, budget, verificationBuffer)
		if errors.Is(err, os.ErrNotExist) && pendingTextPathAbsent(file.abs, file.indexer.RootPath()) {
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

// Missing bytes mask old postings only when the missing path is still owned.
// A broken final link or a missing target behind an escaping parent is a read
// refusal, not evidence that the repository's file has been deleted.
func pendingTextPathAbsent(abs, root string) bool {
	for current := abs; ; current = filepath.Dir(current) {
		_, err := os.Lstat(current)
		if err == nil {
			if current == abs {
				return false
			}
			resolved, resolveErr := filepath.EvalSymlinks(current)
			return resolveErr == nil && pathguard.WithinRoot(resolved, pathguard.ResolveRoot(root))
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(current) == current {
			return false
		}
	}
}
