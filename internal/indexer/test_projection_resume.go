package indexer

import (
	"context"
	"errors"
	"maps"

	"github.com/zzet/gortex/internal/graph"
)

// PendingTestProjectionMeta is a file-node debt marker for an orderly canceled
// resolve/projection, not a crash-atomic record of every resolver write.
const PendingTestProjectionMeta = "pending_test_projection"

func writeTestProjectionBatch(g graph.Store, nodes []*graph.Node, edges []*graph.Edge) error {
	if checked, ok := g.(interface {
		AddBatchChecked([]*graph.Node, []*graph.Edge) error
	}); ok {
		return checked.AddBatchChecked(nodes, edges)
	}
	g.AddBatch(nodes, edges)
	return nil
}

// retainTestProjectionDebt is small final ownership work even when ctx has
// canceled: persist only the actual retargeted file frontier before joining.
func retainTestProjectionDebt(g graph.Store, files []string) error {
	if len(files) == 0 {
		return nil
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	var nodes []*graph.Node
	if finder, ok := g.(graph.NodesInFilesByKindFinder); ok {
		nodes = finder.NodesInFilesByKind(files, []graph.NodeKind{graph.KindFile})
	} else {
		for _, fileNodes := range g.GetFileNodesByPaths(files) {
			for _, node := range fileNodes {
				if node != nil && node.Kind == graph.KindFile {
					nodes = append(nodes, node)
				}
			}
		}
	}
	retained := make([]*graph.Node, 0, len(nodes))
	found := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		found[node.FilePath] = true
		copyNode := *node
		copyNode.Meta = maps.Clone(node.Meta)
		if copyNode.Meta == nil {
			copyNode.Meta = make(map[string]any)
		}
		copyNode.Meta[PendingTestProjectionMeta] = true
		retained = append(retained, &copyNode)
	}
	writeErr := writeTestProjectionBatch(g, retained, nil)
	for _, file := range files {
		if file != "" && !found[file] {
			return errors.Join(writeErr, errors.New("retargeted test projection has no file node for durable repair"))
		}
	}
	return writeErr
}

// publishTestProjectionLocked removes debt in the same checked publication as
// the replacement edges. Failed writes retain the durable marker. Copies keep
// borrowed in-memory nodes' marker intact until the publication succeeds.
func publishTestProjectionLocked(g graph.Store, changed []*graph.Node, edges []*graph.Edge, local []*graph.Node) error {
	var discharged []*graph.Node
	for _, node := range local {
		if node.Kind != graph.KindFile || node.Meta[PendingTestProjectionMeta] != true {
			continue
		}
		copyNode := *node
		copyNode.Meta = maps.Clone(node.Meta)
		delete(copyNode.Meta, PendingTestProjectionMeta)
		discharged = append(discharged, &copyNode)
	}
	if len(discharged) == 0 {
		// Keep the ordinary unmarked projection's existing write behavior.
		g.AddBatch(changed, edges)
		return nil
	}
	// A file whose classification also changed appears in changed already.
	// Publish one final copy per ID, independent of a backend's duplicate order.
	dischargedIDs := make(map[string]bool, len(discharged))
	for _, node := range discharged {
		dischargedIDs[node.ID] = true
	}
	publication := make([]*graph.Node, 0, len(changed)+len(discharged))
	for _, node := range changed {
		if !dischargedIDs[node.ID] {
			publication = append(publication, node)
		}
	}
	return writeTestProjectionBatch(g, append(publication, discharged...), edges)
}

// ResumePendingTestProjectionsContext is the startup-only repair of debt an
// orderly canceled resolve left in the persisted file metadata. The metadata
// reader filters candidate file rows, not a key-indexed SQL lookup; warmup logs
// its actual census/repair time. Clean restarts never run a test projection.
func (mi *MultiIndexer) ResumePendingTestProjectionsContext(ctx context.Context) error {
	var files []string
	for prefix := range mi.AllMetadata() {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, node := range graph.ReadRepoNodesByKindsWithMetaKey(mi.graph, prefix, "", []graph.NodeKind{graph.KindFile}, PendingTestProjectionMeta) {
			if node.Meta[PendingTestProjectionMeta] == true {
				files = append(files, node.FilePath)
			}
		}
	}
	files = appendUniqueSorted(nil, files...)
	_, _, err := markTestSymbolsAndEmitEdgesForFilesContext(ctx, mi.graph, files)
	return err
}

func (mi *MultiIndexer) preserveCanceledTestProjection(ctx context.Context, files []string, projectionErr error) error {
	if ctx.Err() == nil && projectionErr == nil {
		return nil
	}
	return errors.Join(ctx.Err(), projectionErr, retainTestProjectionDebt(mi.graph, files))
}
