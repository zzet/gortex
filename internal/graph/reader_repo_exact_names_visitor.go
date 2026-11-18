package graph

import "context"

type repoExactNamesContextVisitor interface {
	VisitNodesByNamesInRepoContext(context.Context, []string, string, func(*Node) bool) error
}

// VisitNodesByNamesInRepoContext visits exact-name rows owned by repo. An
// optional checked visitor may apply ownership before decoding rows. Other
// readers retain their existing generation, overlay and error semantics;
// their visible rows are filtered without unwrapping the reader.
func VisitNodesByNamesInRepoContext(ctx context.Context, reader Reader, names []string, repo string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || yield == nil || len(names) == 0 {
		return nil
	}
	owned := func(node *Node) bool {
		if node == nil || node.RepoPrefix != repo {
			return true
		}
		return yield(node)
	}
	if visitor, ok := reader.(repoExactNamesContextVisitor); ok {
		if err := visitor.VisitNodesByNamesInRepoContext(ctx, names, repo, owned); err != nil {
			return err
		}
		return ctx.Err()
	}
	return VisitNodesByNamesContext(ctx, reader, names, owned)
}
