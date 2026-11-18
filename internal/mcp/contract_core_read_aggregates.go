package mcp

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.FileImporters = (*contractCoreEdges)(nil)
	_ graph.InEdgeCounter = (*contractCoreEdges)(nil)
)

// FileImporters recomputes the projection over selected, filtered adjacency.
// Forwarding the physical join would resurrect masked or overlaid imports.
func (r *contractCoreEdges) FileImporters(path string) []graph.FileImporterRow {
	rows, err := fileImportersFromReader(r.ctx, r, path)
	recordContractCoreReadError(r.ctx, err)
	return rows
}

// This capability explicitly asks for whole-corpus counts by kind. Restrict
// enumeration to those kinds, through this wrapper's mask. Scoped consumers
// must instead count incoming rows of their bounded candidate identities.
func (r *contractCoreEdges) InEdgeCountsByKind(kinds []graph.EdgeKind) map[string]int {
	return inEdgeCountsFromReader(r, kinds)
}

// inEdgeCountsFromReader counts incoming edges per target for each distinct
// requested kind, through reader's own kind iterators. A wrapper that filters
// adjacency answers the capability with it, so the counts see exactly the
// edges its EdgesByKind admits.
func inEdgeCountsFromReader(reader graph.Reader, kinds []graph.EdgeKind) map[string]int {
	out := make(map[string]int)
	seen := make(map[graph.EdgeKind]bool, len(kinds))
	for _, kind := range kinds {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		for edge := range reader.EdgesByKind(kind) {
			if edge != nil {
				out[edge.To]++
			}
		}
	}
	return out
}

// fileImportersFromReader starts at this file's identities and follows only
// incoming imports. It is also the capability-less overlay fallback; unrelated
// repositories' edges and nodes are never enumerated to answer one file.
func fileImportersFromReader(ctx context.Context, reader graph.Reader, path string) ([]graph.FileImporterRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil || path == "" {
		return nil, nil
	}
	ids := make(map[string]bool)
	for _, node := range reader.GetFileNodes(path) {
		if node != nil && node.FilePath == path {
			ids[node.ID] = true
		}
	}
	// Preserve the legacy OR: a file's own identity also matches even if its
	// FilePath differs from the lookup path.
	if node := reader.GetNode(path); node != nil {
		ids[node.ID] = true
	}
	var sources []string
	for id := range ids {
		rows, err := graph.ReadKindAdjacency(ctx, reader, id, graph.DirectionBackward, []graph.EdgeKind{graph.EdgeImports})
		if err != nil {
			return nil, err
		}
		for _, edge := range rows.Edges {
			sources = append(sources, edge.From)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nodes := reader.GetNodesByIDs(sources)
	var out []graph.FileImporterRow
	for _, id := range sources {
		if n := nodes[id]; n != nil {
			out = append(out, graph.FileImporterRow{FromFile: n.FilePath, FromID: n.ID, FromName: n.Name, FromKind: n.Kind})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FromFile < out[j].FromFile })
	return out, ctx.Err()
}
