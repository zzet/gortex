package mcp

import (
	"context"
	"path/filepath"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/pathkey"
)

// A callee's own file does not enumerate its pending callers. Select the
// existing pending files of this physical checkout before either direction of
// the relation walk, then use the ordinary checked projection/carry boundary.
// No search names, editor lifetime, or background publication is inferred.
func (s *Server) bindPendingRelationReads(ctx context.Context) (context.Context, error) {
	if requestReadsCommittedTree(ctx) || !requestViewFromContext(ctx).acceptsBufferOverlay() {
		return ctx, nil
	}
	if err := validateRequestProjections(ctx); err != nil {
		return ctx, err
	}
	snapshot := pendingFileProjections.snap.Load()
	if len(snapshot.entries) == 0 {
		return ctx, ctx.Err()
	}
	deadline := time.Now().Add(projectedReadBudget)
	if original, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && original.Before(deadline) {
		deadline = original
	}
	ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
	view := requestViewFromContext(ctx)
	paths := make([]string, 0, len(snapshot.entries))
	for _, entry := range snapshot.entries {
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		idx, rel := s.indexerForRel(entry.graphPath)
		if idx == nil || idx.RepoPrefix() != entry.repoPrefix {
			continue
		}
		root := idx.RootPath()
		if view.routed() {
			root = view.viewRoot
		}
		if root == "" || pathkey.MutationKey(filepath.Join(root, filepath.FromSlash(rel))) != entry.key {
			continue
		}
		paths = append(paths, entry.graphPath)
	}
	sort.Strings(paths)
	return s.ensureProjected(ctx, paths)
}
