package graph

import "context"

type exactNameDeclarationsContextVisitor interface {
	VisitNodeDeclarationsByNamesContext(context.Context, []string, func(*Node) bool) error
}

// VisitNodeDeclarationsByNamesContext visits the same exact-name rows as the
// full visitor, but only ID, Name, Kind, FilePath and RepoPrefix are guaranteed.
// These read-only facts must never be written back as complete graph nodes.
// Every kind is visited: callers retain their original completeness/row caps.
// Unknown readers use their checked full visitor, without unwrapping guards.
func VisitNodeDeclarationsByNamesContext(ctx context.Context, reader Reader, names []string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || yield == nil || len(names) == 0 {
		return nil
	}
	if visitor, ok := reader.(exactNameDeclarationsContextVisitor); ok {
		if err := visitor.VisitNodeDeclarationsByNamesContext(ctx, names, yield); err != nil {
			return err
		}
		return ctx.Err()
	}
	return VisitNodesByNamesContext(ctx, reader, names, yield)
}

func (v *OverlaidView) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil || len(names) == 0 {
		return nil
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
		if visitor, ok := v.layer.(exactNameDeclarationsContextVisitor); ok {
			if err := visitor.VisitNodeDeclarationsByNamesContext(ctx, names, forward); err != nil {
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
	return VisitNodeDeclarationsByNamesContext(ctx, v.base, names, func(node *Node) bool {
		if !v.baseNodeVisible(node) {
			return true
		}
		return forward(node)
	})
}

// VisitNodeDeclarationsByNamesInRepoContext preserves a reader's repository-
// scoped checked door. A wrapper with only the full repo capability retains
// that authority rather than inheriting an unscoped optimized read.
func VisitNodeDeclarationsByNamesInRepoContext(ctx context.Context, reader Reader, names []string, repo string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || yield == nil || len(names) == 0 {
		return nil
	}
	owned := func(n *Node) bool {
		if n == nil || n.RepoPrefix != repo {
			return true
		}
		return yield(n)
	}
	if visitor, ok := reader.(interface {
		VisitNodeDeclarationsByNamesInRepoContext(context.Context, []string, string, func(*Node) bool) error
	}); ok {
		if err := visitor.VisitNodeDeclarationsByNamesInRepoContext(ctx, names, repo, owned); err != nil {
			return err
		}
		return ctx.Err()
	}
	if _, ok := reader.(repoExactNamesContextVisitor); ok {
		return VisitNodesByNamesInRepoContext(ctx, reader, names, repo, yield)
	}
	return VisitNodeDeclarationsByNamesContext(ctx, reader, names, owned)
}

// VisitNodeDeclarationsByNamesInRepoContext keeps repository ownership at each
// checked physical reader rather than decoding foreign rows in every layer.
// File and identity masks still apply to the selected base rows. Unknown
// layers keep their checked declaration/full fallback, including refusals.
func (v *OverlaidView) VisitNodeDeclarationsByNamesInRepoContext(ctx context.Context, names []string, repo string, yield func(*Node) bool) error {
	ctx = normalizeNameLookupContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil || len(names) == 0 {
		return nil
	}
	stopped := false
	forward := func(node *Node) bool {
		if ctx.Err() != nil {
			stopped = true
			return false
		}
		if node == nil || node.RepoPrefix != repo {
			return true
		}
		if !yield(node) {
			stopped = true
			return false
		}
		return true
	}
	if v.layer != nil {
		var err error
		if visitor, ok := v.layer.(interface {
			VisitNodeDeclarationsByNamesInRepoContext(context.Context, []string, string, func(*Node) bool) error
		}); ok {
			err = visitor.VisitNodeDeclarationsByNamesInRepoContext(ctx, names, repo, forward)
		} else if visitor, ok := v.layer.(repoExactNamesContextVisitor); ok {
			// A checked repository wrapper takes precedence over a broad projection.
			err = visitor.VisitNodesByNamesInRepoContext(ctx, names, repo, forward)
		} else {
			err = (&OverlaidView{layer: v.layer}).VisitNodeDeclarationsByNamesContext(ctx, names, forward)
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
	return VisitNodeDeclarationsByNamesInRepoContext(ctx, v.base, names, repo, func(node *Node) bool {
		if !v.baseNodeVisible(node) {
			return true
		}
		return forward(node)
	})
}
