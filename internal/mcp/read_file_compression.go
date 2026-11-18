package mcp

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
)

// A clean indexed file needs no pending projection. Bind its keep ranges to
// accepted source metadata and complete nodes from the SAME selected reader.
// Unknown evidence retains the safe uncompressed response; no base fallback.
func (s *Server) readFileCompressionSymbols(ctx context.Context, absPath, graphPath string, content []byte) ([]*graph.Node, bool) {
	idx, _ := s.indexerForRel(graphPath)
	if idx == nil {
		return nil, false
	}
	key := pathkey.MutationKey(absPath)
	owners := pendingFileProjections.snap.Load()
	if owners.entries[key] != nil {
		return nil, false // An unborrowed pending owner cannot be bypassed.
	}
	reader := s.requestSelectedBaseReader(ctx)
	checked, ok := reader.(graph.ConstantValueProjectionReader)
	if !ok {
		return nil, false
	}
	var sources []graphview.GenerationSource
	if view := requestViewFromContext(ctx); view.routed() && view.materialized != nil {
		sources = view.materialized.GenerationSources()
	}
	baseRevision := func() (uint64, bool) {
		if revision, ok := reader.(interface{ MutationRevision() uint64 }); ok {
			return revision.MutationRevision(), true
		}
		if revision, ok := reader.(interface{ SearchMutationRevision() (uint64, bool) }); ok {
			return revision.SearchMutationRevision()
		}
		return 0, false
	}
	before, known := baseRevision()
	if !known {
		// GenerationSources excludes the mutable shared gen0 beneath the
		// composed layers. Its SAME-reader base witness is required too.
		return nil, false
	}
	budget, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	entry := &projectionEntry{repoPrefix: idx.RepoPrefix(), graphPath: graphPath}
	proof := &selectedProjectionStore{ctx: budget, reader: reader, checked: checked, sources: sources, entry: entry}
	rows, err := proof.FileMetasByPaths(entry.repoPrefix, []string{graphPath})
	if err != nil {
		return nil, false
	}
	row, present := rows[graphPath]
	hash := blake3.Sum256(content)
	if !present || row.Size != len(content) || row.ContentHash != hex.EncodeToString(hash[:]) {
		return nil, false
	}
	nodes := proof.GetFileNodes(graphPath)
	if proof.err != nil || proof.validateProjectionRetirement() != nil || budget.Err() != nil {
		return nil, false
	}
	after, stillKnown := baseRevision()
	if !stillKnown || after != before || pendingFileProjections.snap.Load() != owners {
		return nil, false
	}
	return nodes, true
}
