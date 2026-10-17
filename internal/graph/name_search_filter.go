package graph

import "context"

// NameSearchFilter restricts supplementary name candidates before their limit.
// Accept sees only ID, Name, Kind, FilePath, RepoPrefix, WorkspaceID and
// ProjectID. It must be a pure predicate: do not inspect payload metadata,
// re-enter the reader, or perform blocking work. Repository-less synthetic
// nodes remain eligible under RepoAllow; Accept owns workspace and path rules.
type NameSearchFilter struct {
	RepoAllow map[string]bool
	Accept    func(*Node) bool
}

func (f NameSearchFilter) Allows(n *Node) bool {
	return n != nil && (len(f.RepoAllow) == 0 || n.RepoPrefix == "" || f.RepoAllow[n.RepoPrefix]) &&
		(f.Accept == nil || f.Accept(n))
}

type FilteredContainingNameReader interface {
	FindNodesByNameContainingFilteredContext(context.Context, string, int, NameSearchFilter) ([]*Node, error)
}

// FindNodesByNameContainingFilteredContext retains compatibility with readers
// lacking the compact capability, without applying their cap before filtering.
func FindNodesByNameContainingFilteredContext(ctx context.Context, reader Reader, substr string, limit int, filter NameSearchFilter) ([]*Node, error) {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	if filtered, ok := reader.(FilteredContainingNameReader); ok {
		nodes, err := filtered.FindNodesByNameContainingFilteredContext(ctx, substr, limit, filter)
		if err != nil {
			return nodes, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	if filter.Accept == nil && len(filter.RepoAllow) == 0 {
		return FindNodesByNameContainingContext(ctx, reader, substr, limit)
	}
	nodes, err := FindNodesByNameContainingContext(ctx, reader, substr, 0)
	if err != nil && len(nodes) == 0 {
		return nil, err
	}
	kept := nodes[:0]
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if filter.Allows(n) {
			kept = append(kept, n)
			if limit > 0 && len(kept) >= limit {
				break
			}
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return kept, err
}
