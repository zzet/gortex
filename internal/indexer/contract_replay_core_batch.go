package indexer

import (
	"context"
	"reflect"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// A batch exists only between one revalidation's R0 and R1. It never replaces
// the installed ReadCoreFile callback, nor survives into an actor checkpoint.
// Declaration/enrichment reads do not set the extraction context marker.
type contractReplayCoreBatchKey struct{}
type contractReplayCoreBatch struct {
	mu     sync.Mutex
	files  []ContractFollowupFile
	tried  map[ContractFollowupFile]bool
	rows   map[ContractFollowupFile]ContractFollowupCoreFile
	closed bool
}

func (b *contractReplayCoreBatch) add(file ContractFollowupFile) {
	for _, existing := range b.files {
		if existing == file {
			return
		}
	}
	b.files = append(b.files, file)
}

func (b *contractReplayCoreBatch) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.files, b.rows, b.tried = nil, nil, nil
}

func (b *contractReplayCoreBatch) read(ctx context.Context, reader graph.Reader, file ContractFollowupFile) (ContractFollowupCoreFile, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || ctx.Err() != nil {
		return ContractFollowupCoreFile{}, false
	}
	if rows, ok := b.rows[file]; ok {
		return cloneContractReplayCoreFile(rows), true
	}
	if b.tried[file] {
		return ContractFollowupCoreFile{}, false
	}
	var files []ContractFollowupFile
	found := false
	for _, candidate := range b.files {
		if candidate == file {
			found = true
			break
		}
	}
	if !found {
		return ContractFollowupCoreFile{}, false
	}
	// Include the requested file first. The existing name-import frontier bound
	// limits transient grouping, not the accepted census or per-file proof.
	files = append(files, file)
	for _, candidate := range b.files {
		if len(files) == 128 {
			break
		}
		if candidate != file && candidate.RepoPrefix == file.RepoPrefix && !b.tried[candidate] {
			files = append(files, candidate)
		}
	}
	if b.tried == nil {
		b.tried = make(map[ContractFollowupFile]bool)
	}
	paths := make([]string, 0, len(files))
	for _, candidate := range files {
		b.tried[candidate] = true
		paths = append(paths, candidate.Path)
	}
	nodes, err := graph.ContractFileNodesContext(ctx, reader, file.RepoPrefix, paths)
	if err != nil {
		return ContractFollowupCoreFile{}, false
	}
	var ids []string
	for _, candidate := range files {
		for _, node := range nodes[candidate.Path] {
			if node == nil {
				return ContractFollowupCoreFile{}, false
			}
			ids = append(ids, node.ID)
		}
	}
	edges, truncated, err := graph.ContractExtractionEdgesContext(ctx, reader, ids, graph.ContractProjectionRowLimit)
	if err != nil || truncated {
		return ContractFollowupCoreFile{}, false
	}
	pending := make(map[ContractFollowupFile]ContractFollowupCoreFile, len(files))
	var allNodes []*graph.Node
	var allEdges []*graph.Edge
	for _, candidate := range files {
		rows := ContractFollowupCoreFile{Nodes: nodes[candidate.Path]}
		for _, node := range rows.Nodes {
			rows.Edges = append(rows.Edges, edges[node.ID]...)
		}
		pending[candidate] = rows
		allNodes = append(allNodes, rows.Nodes...)
		allEdges = append(allEdges, rows.Edges...)
	}
	// An incomplete, oversized or unencodable batch is never evidence of
	// absence. Discard it and let every installed callback take its original
	// checked per-file path, with the original per-file caps/errors.
	if len(allNodes)+len(allEdges) > graph.ContractProjectionRowLimit || contractEvidenceRowsWithinLimit(allNodes, allEdges) != nil || ctx.Err() != nil {
		return ContractFollowupCoreFile{}, false
	}
	// Keep one bounded chunk, not one chunk per census page. Wrappers receive
	// detached full raw frames; mutation of one answer cannot change another.
	b.rows = pending
	return cloneContractReplayCoreFile(pending[file]), true
}

func cloneContractReplayCoreFile(rows ContractFollowupCoreFile) ContractFollowupCoreFile {
	out := ContractFollowupCoreFile{}
	for _, node := range rows.Nodes {
		if node == nil {
			out.Nodes = append(out.Nodes, nil)
			continue
		}
		copyNode := *node
		if node.Meta != nil {
			copyNode.Meta = contractFollowupCloneValue(reflect.ValueOf(node.Meta)).Interface().(map[string]any)
		}
		out.Nodes = append(out.Nodes, &copyNode)
	}
	for _, edge := range rows.Edges {
		if edge == nil {
			out.Edges = append(out.Edges, nil)
			continue
		}
		copyEdge := *edge
		if edge.Meta != nil {
			copyEdge.Meta = contractFollowupCloneValue(reflect.ValueOf(edge.Meta)).Interface().(map[string]any)
		}
		out.Edges = append(out.Edges, &copyEdge)
	}
	return out
}
