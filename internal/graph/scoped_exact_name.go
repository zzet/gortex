package graph

import "context"

// FindNodesByNameScopedContext reuses the selected reader's indexed exact-name
// scope projection. Readers without it retain their own name lookup and the
// same ownership predicate; overlays are never bypassed for a physical store.
func FindNodesByNameScopedContext(ctx context.Context, reader Reader, name string, scope NodeScope) ([]*Node, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	if scope.WorkspaceID == "" && len(scope.RepoAllow) == 0 {
		return FindNodesByNameContext(ctx, reader, name)
	}
	if scoped, ok := BindReadContext(reader, ctx).(ScopedOutlineNodeReader); ok {
		nodes, err := scoped.OutlineNodesByName(scope, name)
		if err != nil {
			return nil, err
		}
		return nodes, ctx.Err()
	}
	nodes, err := FindNodesByNameContext(ctx, reader, name)
	if err != nil {
		return nil, err
	}
	var out []*Node
	for _, node := range nodes {
		if node != nil && scope.Allows(node) {
			out = append(out, node)
		}
	}
	return out, ctx.Err()
}
