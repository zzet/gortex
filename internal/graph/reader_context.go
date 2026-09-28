package graph

import (
	"context"
	"strings"
)

type exactNameContextReader interface {
	FindNodesByNameContext(context.Context, string) ([]*Node, error)
}

type containingNameContextReader interface {
	FindNodesByNameContainingContext(context.Context, string, int) ([]*Node, error)
}

type layerExactNameContextReader interface {
	NodesByNameContext(context.Context, string) ([]*Node, error)
}

type layerFoldedNameContextVisitor interface {
	VisitNodesByNameContainingFoldedContext(context.Context, string, func(*Node) bool) error
}

func normalizeNameLookupContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// FindNodesByNameContext carries request cancellation through readers that
// support it and brackets a legacy contextless read with cancellation checks.
func FindNodesByNameContext(ctx context.Context, reader Reader, name string) ([]*Node, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	if contextual, ok := reader.(exactNameContextReader); ok {
		nodes, err := contextual.FindNodesByNameContext(ctx, name)
		if err != nil {
			return nodes, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	nodes := reader.FindNodesByName(name)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// FindNodesByNameContainingContext carries request cancellation through
// readers that support it and brackets a legacy contextless read with checks.
func FindNodesByNameContainingContext(ctx context.Context, reader Reader, substr string, limit int) ([]*Node, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	if contextual, ok := reader.(containingNameContextReader); ok {
		nodes, err := contextual.FindNodesByNameContainingContext(ctx, substr, limit)
		if err != nil {
			return nodes, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	nodes := reader.FindNodesByNameContaining(substr, limit)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// FindNodesByNameContext is the request-aware sibling of FindNodesByName.
func (v *OverlaidView) FindNodesByNameContext(ctx context.Context, name string) ([]*Node, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []*Node
	if v.layer != nil {
		var nodes []*Node
		if contextual, ok := v.layer.(layerExactNameContextReader); ok {
			var err error
			nodes, err = contextual.NodesByNameContext(ctx, name)
			if err != nil {
				return out, err
			}
		} else {
			nodes = v.layer.NodesByName(name)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			out = append(out, node)
		}
	}
	if v.base == nil {
		return out, ctx.Err()
	}
	baseNodes, err := FindNodesByNameContext(ctx, v.base, name)
	if err != nil {
		return out, err
	}
	for _, node := range baseNodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if v.baseNodeVisible(node) {
			out = append(out, node)
		}
	}
	return out, ctx.Err()
}

// FindNodesByNameContainingContext is the request-aware sibling of
// FindNodesByNameContaining.
func (v *OverlaidView) FindNodesByNameContainingContext(ctx context.Context, substr string, limit int) ([]*Node, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if substr == "" {
		return nil, nil
	}
	needle := strings.ToLower(substr)
	var out []*Node
	if v.layer != nil {
		switch bounded := v.layer.(type) {
		case layerFoldedNameContextVisitor:
			err := bounded.VisitNodesByNameContainingFoldedContext(ctx, substr, func(node *Node) bool {
				if ctx.Err() != nil {
					return false
				}
				if node == nil || node.Name == "" || !strings.Contains(strings.ToLower(node.Name), needle) {
					return true
				}
				out = append(out, node)
				return limit <= 0 || len(out) < limit
			})
			if err != nil {
				return out, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		case interface {
			VisitNodesByNameContainingFolded(string, func(*Node) bool)
		}:
			bounded.VisitNodesByNameContainingFolded(substr, func(node *Node) bool {
				if ctx.Err() != nil {
					return false
				}
				if node == nil || node.Name == "" || !strings.Contains(strings.ToLower(node.Name), needle) {
					return true
				}
				out = append(out, node)
				return limit <= 0 || len(out) < limit
			})
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		default:
			named := v.layer.NamedNodes()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for name, bucket := range named {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if !strings.Contains(strings.ToLower(name), needle) {
					continue
				}
				for _, node := range bucket {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					out = append(out, node)
					if limit > 0 && len(out) >= limit {
						return out[:limit], nil
					}
				}
			}
		}
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
	}
	if v.base == nil {
		return out, ctx.Err()
	}
	if limit <= 0 {
		candidates, err := FindNodesByNameContainingContext(ctx, v.base, substr, 0)
		if err != nil {
			return out, err
		}
		for _, node := range candidates {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if v.baseNodeVisible(node) {
				out = append(out, node)
			}
		}
		return out, ctx.Err()
	}

	overlayLen := len(out)
	remaining := limit - overlayLen
	if remaining <= 0 {
		return out[:limit], nil
	}
	maxInt := int(^uint(0) >> 1)
	fetch := remaining
	if fetch < maxInt/2 {
		fetch *= 2
	} else {
		fetch = maxInt
	}
	for {
		candidates, err := FindNodesByNameContainingContext(ctx, v.base, substr, fetch)
		if err != nil {
			return out, err
		}
		out = out[:overlayLen]
		for _, node := range candidates {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !v.baseNodeVisible(node) {
				continue
			}
			out = append(out, node)
			if len(out) >= limit {
				return out[:limit], nil
			}
		}
		if len(candidates) < fetch || fetch == maxInt {
			return out, ctx.Err()
		}
		if fetch > maxInt/2 {
			fetch = maxInt
		} else {
			fetch *= 2
		}
	}
}
