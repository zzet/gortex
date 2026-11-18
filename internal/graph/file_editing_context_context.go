package graph

import (
	"context"
	"errors"
)

var ErrFileEditingContextUnsupported = errors.New("cancellable editing context unsupported")

// FileEditingContextRows is the full-field input to the editing-context bundle.
// It deliberately exposes no mutation, scan, transaction or unrelated graph API.
type FileEditingContextRows interface {
	GetFileNodes(string) []*Node
	GetNode(string) *Node
	GetOutEdges(string) []*Edge
	GetNodesByIDs([]string) map[string]*Node
	GetOutEdgesByNodeIDs([]string) map[string][]*Edge
	GetInEdgesByNodeIDs([]string) map[string][]*Edge
}

// FileEditingContextReadScope lends a reader only for the synchronous bundle
// callback. The callback may use only the five reads in FileEditingContextOf
// (and GetNode for overlay endpoint visibility). It must not retain the reader.
// Implementations reject cancellation, partial rows and changed revisions after
// the callback; a callback result is usable only when this method returns nil.
// This narrow scope is not a general Reader transaction API.
type FileEditingContextReadScope interface {
	WithFileEditingContextReader(context.Context, func(context.Context, FileEditingContextRows) error) error
}

type FileEditingContextLayerScope interface {
	WithFileEditingContextLayer(context.Context, func(context.Context, OverlayLayerReader) error) error
}

func WithFileEditingContextReader(ctx context.Context, r Reader, fn func(context.Context, FileEditingContextRows) error) error {
	if ctx == nil {
		return ErrFileEditingContextUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scoped, ok := r.(FileEditingContextReadScope)
	if !ok {
		return ErrFileEditingContextUnsupported
	}
	return scoped.WithFileEditingContextReader(ctx, fn)
}

func (v *OverlaidView) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, FileEditingContextRows) error) error {
	if ctx == nil {
		return ErrFileEditingContextUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	withLayer := func(ctx context.Context, base FileEditingContextRows) error {
		if v.layer == nil {
			return fn(ctx, NewOverlaidViewWithLayer(editingContextReaderOf(base), nil))
		}
		layer, ok := v.layer.(FileEditingContextLayerScope)
		if !ok {
			return ErrFileEditingContextUnsupported
		}
		return layer.WithFileEditingContextLayer(ctx, func(ctx context.Context, l OverlayLayerReader) error {
			return fn(ctx, &editingContextView{NewOverlaidViewWithLayer(editingContextReaderOf(base), l)})
		})
	}
	if v.base == nil {
		return withLayer(ctx, nil)
	}
	return WithFileEditingContextReader(ctx, v.base, withLayer)
}

func (g *Graph) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, FileEditingContextRows) error) error {
	if ctx == nil {
		return ErrFileEditingContextUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	revision := g.MutationRevision()
	if err := fn(ctx, g); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if revision != g.MutationRevision() {
		return ErrContractProjectionStale
	}
	return nil
}
func (l *OverlayLayer) WithFileEditingContextLayer(ctx context.Context, fn func(context.Context, OverlayLayerReader) error) error {
	if ctx == nil {
		return ErrFileEditingContextUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(ctx, l); err != nil {
		return err
	}
	return ctx.Err()
}

// The ephemeral composition only invokes the six bundle reads. A nil embedded
// Reader makes any accidental expansion fail rather than reach a live backend.
type editingContextReader struct {
	Reader
	FileEditingContextRows
}

func editingContextReaderOf(rows FileEditingContextRows) Reader {
	if rows == nil {
		return nil
	}
	return &editingContextReader{FileEditingContextRows: rows}
}
func (r *editingContextReader) GetFileNodes(path string) []*Node {
	return r.FileEditingContextRows.GetFileNodes(path)
}
func (r *editingContextReader) GetNode(id string) *Node { return r.FileEditingContextRows.GetNode(id) }
func (r *editingContextReader) GetOutEdges(id string) []*Edge {
	return r.FileEditingContextRows.GetOutEdges(id)
}
func (r *editingContextReader) GetNodesByIDs(ids []string) map[string]*Node {
	return r.FileEditingContextRows.GetNodesByIDs(ids)
}
func (r *editingContextReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return r.FileEditingContextRows.GetOutEdgesByNodeIDs(ids)
}
func (r *editingContextReader) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	return r.FileEditingContextRows.GetInEdgesByNodeIDs(ids)
}

// Keep durable incoming reads batched without advertising the unrelated
// OverlayLayerProjectionReader scans on the narrow borrowed layer.
type editingContextLayerIncoming interface {
	FileEditingContextInEdgesByNodeIDs([]string) map[string][]*Edge
}
type editingContextView struct{ *OverlaidView }

func (v *editingContextView) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	layer, ok := v.layer.(editingContextLayerIncoming)
	if !ok {
		return v.OverlaidView.GetInEdgesByNodeIDs(ids)
	}
	out := make(map[string][]*Edge, len(ids))
	if v.base != nil {
		for id, edges := range v.base.GetInEdgesByNodeIDs(ids) {
			for _, edge := range edges {
				if v.baseEdgeVisible(edge) {
					out[id] = append(out[id], edge)
				}
			}
		}
	}
	for id, edges := range layer.FileEditingContextInEdgesByNodeIDs(ids) {
		out[id] = append(out[id], edges...)
	}
	return out
}
