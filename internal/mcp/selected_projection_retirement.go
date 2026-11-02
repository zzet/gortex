package mcp

import (
	"context"
	"maps"
	"reflect"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/pathkey"
)

// Retirement is optional proof about the same selected reader, never a reason
// to consult canonical inventory. Keep its lease until the exact-pointer COW
// drop finishes; no I/O or layer check runs under the registry mutex.
func (s *Server) retireSelectedProjection(ctx, requestCtx context.Context, entry *projectionEntry) bool {
	view := requestViewFromContext(requestCtx)
	if view == nil || view.materialized == nil || view.reader == nil || !requestViewPathRoot(requestCtx).contains(entry.absPath) || viewRepoPrefix(view) != entry.repoPrefix {
		return false
	}
	abs, err := s.resolveGraphPath(requestCtx, entry.graphPath)
	if err != nil || pathkey.MutationKey(abs) != entry.key {
		return false
	}
	handoff := view.materialized.Handoff()
	if handoff == nil {
		return false
	}
	defer handoff.Close()
	if !reflect.ValueOf(view.reader).Comparable() || view.reader != handoff.Reader {
		return false
	}
	checked, ok := handoff.Reader.(graph.ConstantValueProjectionReader)
	if !ok {
		return false
	}
	adapter := &selectedProjectionStore{ctx: ctx, reader: handoff.Reader, checked: checked, sources: handoff.GenerationSources(), entry: entry}
	return pendingFileProjections.retireSelected(adapter, map[string]struct{}{entry.key: {}}) == 1
}

// This private adapter supplies the legacy retirement algorithm only checked
// selected metadata/full-node evidence. Its sticky error closes the errorless
// node-read slot; a failed or partial read cannot later certify an empty file.
type selectedProjectionStore struct {
	ctx       context.Context
	reader    graph.Reader
	checked   graph.ConstantValueProjectionReader
	sources   []graphview.GenerationSource
	entry     *projectionEntry
	revisions []store_sqlite.PayloadInputRevision
	initial   map[string]graph.FileMetaRow
	err       error
}

func (p *selectedProjectionStore) validateLayers() error {
	if p.err != nil {
		return p.err
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	first := p.revisions == nil
	if first {
		p.revisions = make([]store_sqlite.PayloadInputRevision, len(p.sources))
	}
	for i, source := range p.sources {
		if source.Handle == nil || source.Layer == nil {
			return graph.ErrConstantProjectionUnsupported
		}
		before, err := source.Handle.PayloadInputRevisionContext(p.ctx)
		if err != nil {
			return err
		}
		checked, ok := source.Layer.(graph.OverlayLayerConstantValueReader)
		if !ok {
			return graph.ErrConstantProjectionUnsupported
		}
		if _, err := checked.LayerConstantValueProjectionContext(p.ctx, nil, nil); err != nil {
			return err
		}
		after, err := source.Handle.PayloadInputRevisionContext(p.ctx)
		if err != nil {
			return err
		}
		if before != after || !first && p.revisions[i] != after {
			return graph.ErrConstantProjectionStale
		}
		if first {
			p.revisions[i] = after
		}
	}
	return p.ctx.Err()
}

func (p *selectedProjectionStore) selectedDeletion() (bool, error) {
	// Newest ownership wins. Even skipped higher sources have already passed
	// their checked layer/revision validation and are checked again afterward.
	for i := len(p.sources) - 1; i >= 0; i-- {
		source := p.sources[i]
		if !source.Layer.HasFile(p.entry.graphPath) {
			continue
		}
		if !source.Layer.IsTombstone(p.entry.graphPath) {
			return false, nil
		}
		before, err := source.Handle.PayloadInputRevisionContext(p.ctx)
		if err != nil {
			return false, err
		}
		rows, err := source.Handle.FileMasksForContext(p.ctx, p.entry.repoPrefix, []string{p.entry.graphPath})
		if err != nil {
			return false, err
		}
		after, err := source.Handle.PayloadInputRevisionContext(p.ctx)
		if err != nil {
			return false, err
		}
		if before != after || after != p.revisions[i] {
			return false, graph.ErrConstantProjectionStale
		}
		return len(rows) == 1 && rows[0].RepoPrefix == p.entry.repoPrefix && rows[0].FilePath == p.entry.graphPath && rows[0].Mode == store_sqlite.OwnershipDelete, nil
	}
	return false, nil
}

func (p *selectedProjectionStore) FileMetasByPaths(prefix string, paths []string) (rows map[string]graph.FileMetaRow, err error) {
	defer func() {
		if err != nil {
			p.err = err
		}
	}()
	if p.err != nil {
		return nil, p.err
	}
	if prefix != p.entry.repoPrefix || len(paths) != 1 || paths[0] != p.entry.graphPath {
		return nil, graph.ErrConstantProjectionUnsupported
	}
	if err = p.validateLayers(); err != nil {
		return nil, err
	}
	key := graph.ConstantFileKey{RepoPrefix: prefix, FilePath: paths[0]}
	proof, err := p.checked.ReadConstantValueProjectionContext(p.ctx, nil, []graph.ConstantFileKey{key})
	if err != nil {
		return nil, err
	}
	rows = make(map[string]graph.FileMetaRow)
	if row, ok := proof.Files[key]; ok {
		if row.FilePath != paths[0] || row.ContentHash == "" {
			return nil, graph.ErrConstantProjectionIncomplete
		}
		rows[paths[0]] = row
	} else if p.entry.tombstone {
		deleted, deleteErr := p.selectedDeletion()
		if deleteErr != nil {
			return nil, deleteErr
		}
		if !deleted {
			return nil, graph.ErrConstantProjectionIncomplete
		}
	}
	if err = p.validateLayers(); err != nil {
		return nil, err
	}
	if p.initial == nil {
		p.initial = maps.Clone(rows)
	}
	return rows, nil
}

func (p *selectedProjectionStore) GetFileNodes(path string) []*graph.Node {
	if path != p.entry.graphPath {
		p.err = graph.ErrContractProjectionUnsupported
		return nil
	}
	if p.err = p.validateLayers(); p.err != nil {
		return nil
	}
	rows, err := graph.ContractFileNodesContext(p.ctx, p.reader, p.entry.repoPrefix, []string{path})
	if err != nil {
		p.err = err
		return nil
	}
	if p.err = p.validateLayers(); p.err != nil {
		return nil
	}
	return rows[path]
}

// Called after hash/node/hash, before acquiring the existing registry COW
// mutex. Rechecking this selected stack is read proof, not new publication
// authority: the accepted content hash/durable delete mask remains required.
func (p *selectedProjectionStore) validateProjectionRetirement() error {
	rows, err := p.FileMetasByPaths(p.entry.repoPrefix, []string{p.entry.graphPath})
	if err != nil {
		return err
	}
	if !maps.Equal(p.initial, rows) {
		return graph.ErrConstantProjectionStale
	}
	return p.validateLayers()
}
