package graph

import "context"

// NodeKindMembershipReader is an optional checked positive-only projection.
// Omitted IDs include both missing nodes and nodes of other kinds. A reader
// must apply its own selected generation and visibility before returning IDs.
type NodeKindMembershipReader interface {
	GetNodeIDsByKindsContext(context.Context, []string, []NodeKind) (map[string]struct{}, error)
}

// GetNodeIDsByKindsContext preserves the exact reader's checked semantics.
// In particular, never unwrap a selected/scoped reader to reach an accelerator:
// its existing kind projection owns replacement, deletion and namespace masks.
func GetNodeIDsByKindsContext(ctx context.Context, reader Reader, ids []string, kinds []NodeKind) (map[string]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids = uniqueNonEmpty(ids)
	if checked, ok := reader.(NodeKindMembershipReader); ok {
		rows, err := checked.GetNodeIDsByKindsContext(ctx, ids, kinds)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return rows, nil
	}
	rows, err := GetNodeKindsByIDsContext(ctx, reader, ids)
	if err != nil {
		return nil, err
	}
	allowed := make(map[NodeKind]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	result := make(map[string]struct{})
	for id, row := range rows {
		if allowed[row.Kind] {
			result[id] = struct{}{}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
