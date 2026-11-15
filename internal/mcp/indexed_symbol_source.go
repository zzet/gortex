package mcp

import (
	"bytes"
	"context"
	"encoding/hex"
	"time"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
)

// Bound only after the source handler has normalized the actual requested ID.
// This is not permission to reuse any other symbol or canonical graph.
type indexedSymbolSourceTargetKey struct{}

// readIndexedSymbolSource binds current bytes to one checked declaration from
// the SAME selected reader. Unlike body compression, source for one symbol
// needs no unrelated file-node hydration. Unknown evidence keeps the ordinary
// physical projection/extraction path and its unchanged request allowance.
func (s *Server) readIndexedSymbolSource(ctx context.Context, absPath, graphPath string, content []byte, id string, expected *graph.Node) (*graph.Node, bool) {
	if !requestViewFromContext(ctx).routed() || !requestViewPathRoot(ctx).contains(absPath) || id == "" || graph.IDFile(id) != graphPath {
		return nil, false
	}
	idx, _ := s.indexerForRel(graphPath)
	if idx == nil {
		return nil, false
	}
	key := pathkey.MutationKey(absPath)
	owners := pendingFileProjections.snap.Load()
	if owners.entries[key] != nil {
		return nil, false
	}
	reader := s.requestSelectedBaseReader(ctx)
	checked, ok := reader.(graph.ConstantValueProjectionReader)
	if !ok {
		return nil, false
	}
	var sources []graphview.GenerationSource
	if view := requestViewFromContext(ctx); view.materialized != nil {
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
	if proof.validateLayers() != nil {
		return nil, false
	}
	targets, err := graph.ContractSourceNodesContext(budget, reader, []string{id})
	if err != nil || proof.validateLayers() != nil {
		return nil, false
	}
	node := targets[id]
	if node == nil || node.ID != id || node.RepoPrefix != entry.repoPrefix || node.FilePath != graphPath || node.Kind == "" || node.StartLine < 1 || node.EndLine < node.StartLine || node.EndLine > bytes.Count(content, []byte{'\n'})+1 {
		return nil, false
	}
	if expected != nil && (node.ID != expected.ID || node.RepoPrefix != expected.RepoPrefix || node.FilePath != expected.FilePath || node.Kind != expected.Kind || node.StartLine != expected.StartLine || node.EndLine != expected.EndLine) {
		return nil, false
	}
	if proof.validateProjectionRetirement() != nil || budget.Err() != nil {
		return nil, false
	}
	after, stillKnown := baseRevision()
	if !stillKnown || after != before || pendingFileProjections.snap.Load() != owners {
		return nil, false
	}
	return node, true
}
