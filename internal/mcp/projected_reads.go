package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
)

const projectedReadBudget = 150 * time.Millisecond

// Each request retains only the immutable file layers it actually checked in
// its selected checkout. Editor buffers compose above these disk projections.
type requestProjectionKey struct{}

func requestProjectionLayers(ctx context.Context) []graph.OverlayLayerReader {
	layers, _ := ctx.Value(requestProjectionKey{}).([]graph.OverlayLayerReader)
	return layers
}

// ensureProjected reads current tracked bytes without publishing graph/store
// state. Failure is explicit: an old graph must never masquerade as a current
// answer after the checked file could not be projected.
func (s *Server) ensureProjected(ctx context.Context, paths []string) (context.Context, error) {
	if requestReadsCommittedTree(ctx) || !requestViewFromContext(ctx).acceptsBufferOverlay() {
		return ctx, nil
	}
	buildCtx, cancel := context.WithTimeout(ctx, projectedReadBudget)
	defer cancel()
	layers := append([]graph.OverlayLayerReader(nil), requestProjectionLayers(ctx)...)
	for _, graphPath := range paths {
		if err := buildCtx.Err(); err != nil {
			return ctx, err
		}
		idx, rel := s.indexerForRel(graphPath)
		if idx == nil || !idx.IsTracked(rel) {
			continue // untracked-file tools retain their raw-source guidance
		}
		abs, err := s.resolveGraphPath(ctx, graphPath)
		if err != nil {
			return ctx, err
		}
		entry, err := s.projectCurrentTrackedFile(buildCtx, ctx, abs, graphPath, rel)
		if err != nil {
			return ctx, fmt.Errorf("file graph pending for %s: %w", graphPath, err)
		}
		if entry != nil {
			layers = append(layers, entry.layer)
		}
	}
	if len(layers) == 0 {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, requestProjectionKey{}, layers)
	if editor := OverlayViewFromContext(ctx); editor != nil {
		// Reuse the request's pinned buffer layer; do not parse it again or
		// change its session/snapshot authority.
		ctx = WithOverlayView(ctx, graph.NewOverlaidViewWithLayer(s.requestBaseReader(ctx), editor.LayerReader()))
	}
	return ctx, nil
}

func (s *Server) projectCurrentTrackedFile(buildCtx, requestCtx context.Context, abs, graphPath, rel string) (*projectionEntry, error) {
	idx, _ := s.indexerForRel(graphPath)
	before, err := statDiskStamp(abs)
	if err != nil {
		return nil, err
	}
	if before.info == nil {
		entry := newProjectionTombstone(abs, graphPath, idx.RepoPrefix(), 0)
		if err := pendingFileProjections.publish(entry); err != nil {
			return nil, err
		}
		return entry, nil
	}
	// Reuse the confined, bounded, nonblocking regular-file reader. A FIFO,
	// symlink or unsupported platform is a refusal, never an unsafe Open.
	files, err := source.NewFilesystemSource(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	defer files.Close()
	raw, _, err := files.ReadRegularFile(buildCtx, filepath.Base(abs), 1<<20)
	if err != nil {
		return nil, err
	}
	after, err := statDiskStamp(abs)
	if err != nil || !before.matches(after) {
		return nil, errors.Join(err, errProjectionExternalWrite)
	}
	if !requestViewFromContext(requestCtx).routed() && idx.IndexedContentMatches(rel, raw) {
		return nil, nil
	}
	entry, lookupErr := pendingFileProjections.lookup(abs)
	if lookupErr != nil && !errors.Is(lookupErr, errProjectionExternalWrite) {
		return nil, lookupErr
	}
	if entry != nil {
		if !entry.tombstone && entry.rawHash == blake3.Sum256(raw) {
			return entry, nil
		}
		pendingFileProjections.drop(entry)
	}
	// Use the registered parser identity while retaining the physical
	// checkout path as the registry key and disk identity.
	projection, err := idx.ProjectSource(buildCtx, s.overlayOwnerAbsPath(requestCtx, abs), raw)
	if err != nil {
		return nil, err
	}
	entry = newProjectionEntry(abs, idx.RepoPrefix(), projection, raw, 0, after)
	latest, err := statDiskStamp(abs)
	if err != nil || !after.matches(latest) {
		return nil, errors.Join(err, errProjectionExternalWrite)
	}
	if err := pendingFileProjections.publish(entry); err != nil {
		return nil, err
	}
	return entry, nil
}
