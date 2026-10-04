package graph

import (
	"context"
	"errors"
)

var ErrConstantProjectionUnsupported = errors.New("checked constant projection unsupported")
var ErrConstantProjectionIncomplete = errors.New("constant replacement lacks accepted file inventory")
var ErrConstantProjectionStale = errors.New("constant layer ownership revision changed")

// ConstantFileKey identifies accepted file inventory without a repository scan.
type ConstantFileKey struct{ RepoPrefix, FilePath string }

// ScopedConstantValueRow retains the file/repository ownership lost by the
// ordinary value-only reader.
type ScopedConstantValueRow struct {
	ConstantValueRow
	RepoPrefix string
}

// ConstantValueProjection is a checked physical/composed sidecar snapshot.
// Nodes contains requested identities and their file paths, not mutable rows.
// Files contains actual accepted inventory; a missing entry is not fabricated.
// Callers still need an ancestor revision witness for publication freshness.
type ConstantValueProjection struct {
	Rows  map[string]ScopedConstantValueRow
	Nodes map[string]string
	Files map[ConstantFileKey]FileMetaRow
}

type ConstantValueContextReader interface {
	ConstantValuesByNodeIDsContext(context.Context, []string) (map[string]string, error)
}
type ConstantValueProjectionReader interface {
	ReadConstantValueProjectionContext(context.Context, []string, []ConstantFileKey) (ConstantValueProjection, error)
}
type OverlayLayerConstantValueReader interface {
	LayerConstantValueProjectionContext(context.Context, []string, []ConstantFileKey) (ConstantValueProjection, error)
}

func newConstantValueProjection() ConstantValueProjection {
	return ConstantValueProjection{Rows: make(map[string]ScopedConstantValueRow), Nodes: make(map[string]string), Files: make(map[ConstantFileKey]FileMetaRow)}
}

// ConstantValuesByNodeIDsContext reaches checked constant reads through the
// thin production wrappers without assuming an errorless node miss is absence.
func ConstantValuesByNodeIDsContext(ctx context.Context, r Reader, ids []string) (map[string]string, error) {
	p, err := readConstantProjection(ctx, r, ids, nil)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(p.Rows))
	for id, row := range p.Rows {
		values[id] = row.Value
	}
	return values, nil
}

func readConstantProjection(ctx context.Context, r Reader, ids []string, files []ConstantFileKey) (ConstantValueProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConstantValueProjection{}, err
	}
	switch v := r.(type) {
	case *DeltaWriter:
		return v.readConstantProjection(ctx, ids, files)
	case *OverlaidView:
		return v.readConstantProjection(ctx, ids, files)
	case Unwrapper:
		if next := v.Unwrap(); next != nil {
			return readConstantProjection(ctx, next, ids, files)
		}
	}
	reader, ok := r.(ConstantValueProjectionReader)
	if !ok {
		return ConstantValueProjection{}, ErrConstantProjectionUnsupported
	}
	return reader.ReadConstantValueProjectionContext(ctx, ids, files)
}

func (v *OverlaidView) readConstantProjection(ctx context.Context, ids []string, files []ConstantFileKey) (ConstantValueProjection, error) {
	if layer, ok := v.layer.(*deltaLayer); ok && layer.owner != nil && layer.owner.view == v {
		return layer.owner.readConstantProjection(ctx, ids, files)
	}
	base := newConstantValueProjection()
	var err error
	if v.base != nil {
		base, err = readConstantProjection(ctx, v.base, ids, files)
		if err != nil {
			return ConstantValueProjection{}, err
		}
	}
	if v.layer == nil {
		return base, nil
	}
	reader, ok := v.layer.(OverlayLayerConstantValueReader)
	if !ok {
		return ConstantValueProjection{}, ErrConstantProjectionUnsupported
	}
	keys := append([]ConstantFileKey(nil), files...)
	for _, row := range base.Rows {
		keys = append(keys, ConstantFileKey{RepoPrefix: row.RepoPrefix, FilePath: row.FilePath})
	}
	own, err := reader.LayerConstantValueProjectionContext(ctx, ids, keys)
	if err != nil {
		return ConstantValueProjection{}, err
	}
	return composeConstantProjection(ctx, v.layer, base, own, nil, true)
}

// composeConstantProjection keeps inheritance for identity-only enrichment.
// File replacement owns an empty sidecar only with actual accepted inventory;
// unknown generic replacement explicitly refuses the checked read.
func composeConstantProjection(ctx context.Context, layer OverlayLayerReader, base, own ConstantValueProjection, authoritative map[ConstantFileKey]bool, requireInventory bool) (ConstantValueProjection, error) {
	out := newConstantValueProjection()
	for id, path := range base.Nodes {
		if !layer.CoversNodeID(id) && !layer.OwnsNodeIdentity(id) {
			out.Nodes[id] = path
		}
	}
	for id, path := range own.Nodes {
		out.Nodes[id] = path
	}
	for key, row := range base.Files {
		if layer.HasFile(key.FilePath) || authoritative[key] {
			continue
		}
		out.Files[key] = row
	}
	for key, row := range own.Files {
		if !layer.IsTombstone(key.FilePath) {
			out.Files[key] = row
		}
	}
	for id, row := range base.Rows {
		if _, visible := out.Nodes[id]; !visible {
			continue
		}
		key := ConstantFileKey{RepoPrefix: row.RepoPrefix, FilePath: row.FilePath}
		if layer.IsTombstone(row.FilePath) || authoritative[key] {
			continue
		}
		if requireInventory && layer.HasFile(row.FilePath) {
			inventory, accepted := own.Files[key]
			if !accepted || inventory.ContentHash == "" {
				return ConstantValueProjection{}, ErrConstantProjectionIncomplete
			}
			continue
		}
		out.Rows[id] = row
	}
	for id, row := range own.Rows {
		if _, visible := out.Nodes[id]; visible && !layer.IsTombstone(row.FilePath) {
			out.Rows[id] = row
		}
	}
	if err := ctx.Err(); err != nil {
		return ConstantValueProjection{}, err
	}
	return out, nil
}

func (v *OverlaidView) ConstantValuesByNodeIDsContext(ctx context.Context, ids []string) (map[string]string, error) {
	return ConstantValuesByNodeIDsContext(ctx, v, ids)
}
func (v *OverlaidView) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []ConstantFileKey) (ConstantValueProjection, error) {
	return readConstantProjection(ctx, v, ids, files)
}
func (dw *DeltaWriter) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []ConstantFileKey) (ConstantValueProjection, error) {
	return readConstantProjection(ctx, dw, ids, files)
}
func (v *OverlaidView) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	return v.ConstantValuesByNodeIDsContext(context.Background(), ids)
}
func (dw *DeltaWriter) ConstantValuesByNodeIDsContext(ctx context.Context, ids []string) (map[string]string, error) {
	return ConstantValuesByNodeIDsContext(ctx, dw, ids)
}
func (dw *DeltaWriter) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	return dw.ConstantValuesByNodeIDsContext(context.Background(), ids)
}

func (g *Graph) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []ConstantFileKey) (ConstantValueProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ConstantValueProjection{}, err
	}
	p := newConstantValueProjection()
	g.constValuesMu.Lock()
	for _, id := range ids {
		if entry, ok := g.constValues[id]; ok {
			row := ScopedConstantValueRow{ConstantValueRow: ConstantValueRow{NodeID: id, FilePath: entry.filePath, Value: entry.value}, RepoPrefix: entry.repoPrefix}
			p.Rows[id] = row
			files = append(files, ConstantFileKey{RepoPrefix: entry.repoPrefix, FilePath: entry.filePath})
		}
	}
	g.constValuesMu.Unlock()
	for _, id := range ids {
		if n := g.GetNode(id); n != nil {
			p.Nodes[id] = n.FilePath
		}
	}
	g.fileMetasMu.Lock()
	for _, key := range files {
		if row, ok := g.fileMetas[key.RepoPrefix][key.FilePath]; ok {
			p.Files[key] = row
		}
	}
	g.fileMetasMu.Unlock()
	if err := ctx.Err(); err != nil {
		return ConstantValueProjection{}, err
	}
	return p, nil
}
func (g *Graph) ConstantValuesByNodeIDsContext(ctx context.Context, ids []string) (map[string]string, error) {
	return ConstantValuesByNodeIDsContext(ctx, g, ids)
}
func (l *OverlayLayer) LayerConstantValueProjectionContext(ctx context.Context, ids []string, _ []ConstantFileKey) (ConstantValueProjection, error) {
	if err := ctx.Err(); err != nil {
		return ConstantValueProjection{}, err
	}
	p := newConstantValueProjection()
	for _, id := range ids {
		if node := l.nodeByID[id]; node != nil {
			p.Nodes[id] = node.FilePath
		}
	}
	return p, nil
}
