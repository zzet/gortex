package graph

import "context"

type exactNamesContextVisitor interface {
	VisitNodesByNamesContext(context.Context, []string, func(*Node) bool) error
}

// VisitNodesByNamesContext visits rows matching any exact name. Returning false
// stops successfully. Batch order is reader-defined; overlay rows precede base
// rows. Readers without this optional capability retain checked single-name
// visits. Callers must discard accumulated results if a visit returns an error.
func VisitNodesByNamesContext(ctx context.Context, reader Reader, names []string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || yield == nil || len(names) == 0 {
		return nil
	}
	if len(names) == 1 {
		return VisitNodesByNameContext(ctx, reader, names[0], yield)
	}
	if visitor, ok := reader.(exactNamesContextVisitor); ok {
		if err := visitor.VisitNodesByNamesContext(ctx, names, yield); err != nil {
			return err
		}
		return ctx.Err()
	}
	stopped := false
	for _, name := range names {
		if err := VisitNodesByNameContext(ctx, reader, name, func(node *Node) bool {
			stopped = !yield(node)
			return !stopped
		}); err != nil {
			return err
		}
		if stopped {
			break
		}
	}
	return ctx.Err()
}

// VisitNodesByNamesContext applies the same ownership and tombstone mask as
// the single-name visitor, including context-aware layer fallback reads.
func (v *OverlaidView) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil || len(names) == 0 {
		return nil
	}
	if len(names) == 1 {
		return v.VisitNodesByNameContext(ctx, names[0], yield)
	}
	stopped := false
	forward := func(node *Node) bool {
		if ctx.Err() != nil || !yield(node) {
			stopped = true
			return false
		}
		return true
	}
	if v.layer != nil {
		if visitor, ok := v.layer.(exactNamesContextVisitor); ok {
			if err := visitor.VisitNodesByNamesContext(ctx, names, forward); err != nil {
				return err
			}
		} else {
			for _, name := range names {
				if err := (&OverlaidView{layer: v.layer}).VisitNodesByNameContext(ctx, name, forward); err != nil {
					return err
				}
				if stopped {
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
	return VisitNodesByNamesContext(ctx, v.base, names, func(node *Node) bool {
		if !v.baseNodeVisible(node) {
			return true
		}
		return forward(node)
	})
}
