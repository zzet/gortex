package graph

import "context"

type exactNameContextVisitor interface {
	VisitNodesByNameContext(context.Context, string, func(*Node) bool) error
}

type layerExactNameContextVisitor interface {
	VisitNodesByNameContext(context.Context, string, func(*Node) bool) error
}

// VisitNodesByNameContext visits exact-name rows in the same order as
// FindNodesByNameContext. Returning false stops successfully. Readers without
// the optional visitor capability retain the materialized lookup behavior.
func VisitNodesByNameContext(ctx context.Context, reader Reader, name string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || yield == nil {
		return nil
	}
	if visitor, ok := reader.(exactNameContextVisitor); ok {
		if err := visitor.VisitNodesByNameContext(ctx, name, yield); err != nil {
			return err
		}
		return ctx.Err()
	}
	nodes, err := FindNodesByNameContext(ctx, reader, name)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !yield(node) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// VisitNodesByNameContext visits overlay rows before visible base rows. It
// applies the same ownership and tombstone mask as FindNodesByNameContext.
func (v *OverlaidView) VisitNodesByNameContext(ctx context.Context, name string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil {
		return nil
	}
	stopped := false
	forward := func(node *Node) bool {
		if ctx.Err() != nil {
			stopped = true
			return false
		}
		if !yield(node) {
			stopped = true
			return false
		}
		return true
	}
	if v.layer != nil {
		if visitor, ok := v.layer.(layerExactNameContextVisitor); ok {
			if err := visitor.VisitNodesByNameContext(ctx, name, forward); err != nil {
				return err
			}
		} else {
			var nodes []*Node
			if contextual, ok := v.layer.(layerExactNameContextReader); ok {
				var err error
				nodes, err = contextual.NodesByNameContext(ctx, name)
				if err != nil {
					return err
				}
			} else {
				nodes = v.layer.NodesByName(name)
			}
			for _, node := range nodes {
				if !forward(node) {
					break
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
	if v.base == nil {
		return ctx.Err()
	}
	return VisitNodesByNameContext(ctx, v.base, name, func(node *Node) bool {
		if !v.baseNodeVisible(node) {
			return true
		}
		return forward(node)
	})
}
