package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

const sourceSearchMaxFiles = 20000
const sourceSearchMaxBytes = 128 << 20

var errSourceSnapshotMoved = errors.New("source search scope changed while being sampled")

type sourceSearchFile struct {
	path     string
	abs      string
	content  []byte
	evidence physicalReadEvidence
	overlay  bool
}

func (s *Server) sourceSearchIndexer(view *requestView) *indexer.Indexer {
	if s.multiIndexer != nil {
		return s.multiIndexer.GetIndexer(view.sourceRepoPrefix)
	}
	return s.indexer
}

func (s *Server) validateSourceCheckoutIdentity(ctx context.Context, view *requestView) error {
	if view != nil && view.sourceRootInfo != nil {
		info, err := indexer.SourceRootFileInfo(view.viewRoot)
		resolved, resolveErr := filepath.EvalSymlinks(view.viewRoot)
		if err != nil || resolveErr != nil || !os.SameFile(view.sourceRootInfo, info) || filepath.Clean(resolved) != filepath.Clean(view.sourceResolvedRoot) {
			return graphview.NewViewError(graphview.CodeCheckoutInaccessible, "source checkout physical root changed while reading")
		}
	}
	if view == nil || view.rider == nil || s.materializer == nil || s.materializer.Catalog == nil {
		return nil
	}
	checkout, found, err := s.materializer.Catalog.GetCheckout(ctx, view.rider.CheckoutID)
	if err != nil {
		return err
	}
	if !found || checkout.State != store_sqlite.CheckoutStateReady || checkout.Incarnation != view.sourceCheckoutIncarnation || filepath.Clean(checkout.RootPath) != filepath.Clean(view.viewRoot) {
		return graphview.NewViewError(graphview.CodeCheckoutInaccessible, "source checkout identity changed while reading")
	}
	return s.checkoutInSessionScope(ctx, checkout)
}

// sourceSearchSnapshot proves the complete admitted path scope, including new
// files and overlay additions/tombstones. It never treats old postings or a
// list of returned hits as a completeness proof. A moving scope retries inside
// the original absolute budget; resource/read errors make no complete claim.
func (s *Server) sourceSearchSnapshot(ctx context.Context, view *requestView, pathFilters []string, resolved ResolvedScope) ([]sourceSearchFile, error) {
	idx := s.sourceSearchIndexer(view)
	if idx == nil {
		return nil, fmt.Errorf("source search has no owning indexer")
	}
	if len(resolved.RepoAllow) > 0 && !resolved.RepoAllow[view.sourceRepoPrefix] {
		return nil, fmt.Errorf("source search domain is outside the selected checkout")
	}
	if (resolved.WorkspaceID != "" && resolved.WorkspaceID != idx.WorkspaceID()) || (resolved.ProjectID != "" && resolved.ProjectID != idx.ProjectID()) {
		return nil, fmt.Errorf("source search domain is outside the selected checkout")
	}
	prefixes := expandPathPrefixesWithRepos(normalizePathPrefixes(pathFilters), []string{view.sourceRepoPrefix})
	allow := func(rel string) bool {
		path := rel
		if view.sourceRepoPrefix != "" {
			path = view.sourceRepoPrefix + "/" + rel
		}
		return len(prefixes) == 0 || pathMatchesAnyPrefix(path, prefixes)
	}
	allowDir := func(rel string) bool {
		if len(prefixes) == 0 {
			return true
		}
		path := sourceSearchGraphPath(view, rel)
		if pathMatchesAnyPrefix(path, prefixes) {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(prefix, path+"/") {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	if view.freshness != nil && !view.freshness.deadline.IsZero() {
		deadline = view.freshness.deadline
	}
	scanCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		files, err := s.sampleSourceSearchSnapshot(scanCtx, view, idx, allow, allowDir)
		if !errors.Is(err, errSourceSnapshotMoved) {
			return files, err
		}
		if err := waitFreshnessRetry(scanCtx, deadline); err != nil {
			return nil, err
		}
	}
}

func (s *Server) sourceSearchInventory(ctx context.Context, view *requestView, idx *indexer.Indexer, allow, allowDir func(string) bool) ([]string, map[string]sourceSearchFile, error) {
	var patterns []string
	if s.configManager != nil {
		patterns = s.configManager.EffectiveExcludeForRoot(view.sourceRepoPrefix, view.viewRoot)
	}
	paths, err := idx.CurrentSourceSearchFiles(ctx, view.viewRoot, allow, allowDir, sourceSearchMaxFiles, patterns)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, errSourceSnapshotMoved
		}
		return nil, nil, err
	}
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		set[path] = struct{}{}
	}
	overlays := make(map[string]sourceSearchFile)
	if snapshot, ok := overlayRequestSnapshotFromContext(ctx); ok && snapshot != nil {
		for _, file := range snapshot.files {
			abs, err := s.resolveOverlayRequestAbsPath(ctx, file.Path)
			if err != nil {
				return nil, nil, err
			}
			if !requestViewPathRoot(ctx).contains(abs) {
				continue
			}
			rel, err := filepath.Rel(view.viewRoot, abs)
			if err != nil {
				return nil, nil, err
			}
			rel = filepath.ToSlash(rel)
			if !allow(rel) {
				continue
			}
			delete(set, rel)
			if file.Deleted {
				continue
			}
			content := []byte(file.Content)
			if !idx.AdmitSourceSearchBuffer(ctx, view.viewRoot, abs, content, patterns) {
				continue
			}
			// Overlay BaseSHA must be tied to verified bytes, not a separate stat.
			if expected := normalizeExpectedSHA(file.BaseSHA); expected != "" {
				disk, evidence, err := readPhysicalFileEvidenceBounded(abs, sourceSearchMaxBytes)
				if err != nil {
					return nil, nil, err
				}
				if !requestViewPathRoot(ctx).contains(evidence.resolvedPath) || gitBlobSHA(disk) != expected {
					return nil, nil, fmt.Errorf("overlay drift: %s", rel)
				}
			}
			overlays[rel] = sourceSearchFile{path: rel, abs: abs, content: content, overlay: true}
			set[rel] = struct{}{}
		}
	}
	paths = paths[:0]
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > sourceSearchMaxFiles {
		return nil, nil, fmt.Errorf("%w: admitted file bound", indexer.ErrSourceSearchBudget)
	}
	return paths, overlays, nil
}

func (s *Server) sampleSourceSearchSnapshot(ctx context.Context, view *requestView, idx *indexer.Indexer, allow, allowDir func(string) bool) ([]sourceSearchFile, error) {
	paths, overlays, err := s.sourceSearchInventory(ctx, view, idx, allow, allowDir)
	if err != nil {
		return nil, err
	}
	files := make([]sourceSearchFile, 0, len(paths))
	total := 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, overlay := overlays[path]
		if !overlay {
			if total >= sourceSearchMaxBytes {
				return nil, fmt.Errorf("%w: byte bound", indexer.ErrSourceSearchBudget)
			}
			abs := filepath.Join(view.viewRoot, filepath.FromSlash(path))
			info, err := os.Stat(abs)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, errSourceSnapshotMoved
				}
				return nil, err
			}
			if info.Size() > sourceSearchMaxBytes-int64(total) {
				return nil, fmt.Errorf("%w: byte bound", indexer.ErrSourceSearchBudget)
			}
			content, evidence, err := readPhysicalFileEvidenceBounded(abs, int64(sourceSearchMaxBytes-total))
			if err != nil {
				if errors.Is(err, errPhysicalFileMoved) || os.IsNotExist(err) {
					return nil, errSourceSnapshotMoved
				}
				return nil, fmt.Errorf("source search verification: %w", err)
			}
			if !requestViewPathRoot(ctx).contains(evidence.resolvedPath) {
				return nil, fmt.Errorf("source search resolved path escapes selected checkout")
			}
			file = sourceSearchFile{path: path, abs: abs, content: content, evidence: evidence}
		}
		total += len(file.content)
		if total > sourceSearchMaxBytes {
			return nil, fmt.Errorf("%w: byte bound", indexer.ErrSourceSearchBudget)
		}
		files = append(files, file)
	}
	after, _, err := s.sourceSearchInventory(ctx, view, idx, allow, allowDir)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(paths, after) {
		return nil, errSourceSnapshotMoved
	}
	for _, file := range files {
		if file.overlay {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, evidence, err := readPhysicalFileEvidenceBounded(file.abs, sourceSearchMaxBytes)
		if err != nil {
			if errors.Is(err, errPhysicalFileMoved) || os.IsNotExist(err) {
				return nil, errSourceSnapshotMoved
			}
			return nil, err
		}
		if evidence.contentSHA256 != file.evidence.contentSHA256 || filepath.Clean(evidence.resolvedPath) != filepath.Clean(file.evidence.resolvedPath) {
			return nil, errSourceSnapshotMoved
		}
	}
	if err := s.validateSourceCheckoutIdentity(ctx, view); err != nil {
		return nil, err
	}
	return files, nil
}

func sourceSearchGraphPath(view *requestView, path string) string {
	if view.sourceRepoPrefix == "" {
		return path
	}
	return strings.TrimSuffix(view.sourceRepoPrefix, "/") + "/" + path
}
