package mcp

import (
	"bytes"
	"context"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
)

type canonicalSummarySourceKey struct{}

// canonicalSummarySource holds the confined bytes whose indexed content was
// checked before this summary's store read. It carries no relation authority.
type canonicalSummarySource struct {
	indexer                             *indexer.Indexer
	root, abs, readPath, key, graphPath string
	stamp                               diskStamp
	bytes                               []byte
	registry                            *projectionSnapshot
}

// canonicalSummaryDefinitions supplies only file definitions when a checked
// store read of the file timed out or saw the store revision move. It uses
// the original request deadline and leaves the store failure pending. The
// registry's change log for this file excludes even an owner that came and
// went while SQLite was unavailable; other files' owners do not move it.
func (s *Server) canonicalSummaryDefinitions(ctx context.Context, graphPath string, proof *canonicalSummarySource) *query.SubGraph {
	deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !ok || proof.registry == nil || proof.graphPath != graphPath || requestDeferredProjectionFiles(ctx)[graphPath] != "" {
		return nil
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if ctx.Err() != nil || !pendingFileProjections.ownerAbsentSince(proof.registry, proof.key) {
		return nil
	}
	idx, rel := s.indexerForRel(graphPath)
	if idx != proof.indexer || !idx.IsTracked(rel) {
		return nil
	}
	abs, err := s.resolveGraphPath(ctx, graphPath)
	if err != nil || abs != proof.abs || s.projectionStormActive(abs) {
		return nil
	}
	root, readPath, key, err := selectedProjectionReadPath(ctx, idx.RootPath(), idx.RepoPrefix(), abs)
	if err != nil || root != proof.root || readPath != proof.readPath || key != proof.key {
		return nil
	}
	before, err := statDiskStamp(abs)
	if err != nil || !proof.stamp.matches(before) || ctx.Err() != nil {
		return nil
	}
	projection, err := idx.ProjectSource(ctx, abs, proof.bytes)
	if err != nil || projection.GraphPath != graphPath || !projection.ParseChecked || projection.ParseErrors != 0 || ctx.Err() != nil {
		return nil
	}
	files, err := source.NewFilesystemSource(root)
	if err != nil {
		return nil
	}
	defer files.Close()
	current, _, err := files.ReadRegularFile(ctx, filepath.ToSlash(readPath), 1<<20)
	if err != nil || !bytes.Equal(current, proof.bytes) {
		return nil
	}
	after, err := statDiskStamp(abs)
	if err != nil || !proof.stamp.matches(after) || pathkey.MutationKey(abs) != key ||
		s.projectionStormActive(abs) || !pendingFileProjections.ownerAbsentSince(proof.registry, proof.key) || ctx.Err() != nil {
		return nil
	}
	return &query.SubGraph{Nodes: projection.Nodes}
}
