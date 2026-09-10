package graph

import (
	"context"
	"sort"
	"sync/atomic"
)

// ScopedIncomingSourceReader applies all higher-layer ownership before its
// distinct-source sentinel. A truncated input cannot subsequently be filtered.
type ScopedIncomingSourceReader interface {
	FindIncomingSourcesScoped(context.Context, []string, EdgeKind, int, IncomingSourceScope, *IncomingSourceBudget) (BoundedIncomingSourceProjection, error)
}

// IncomingSourceBudget belongs to one public query and is shared by its
// physical lower/local reads. It is not captured in reusable scope predicates.
type IncomingSourceBudget struct{ inspected atomic.Int64 }

func (b *IncomingSourceBudget) Charge(rows int) error {
	if rows < 0 || b.inspected.Add(int64(rows)) > int64(MaxIncomingSourceCandidateRows) {
		return &BoundedLocalizationLimitError{Resource: "incoming-source candidate inspections", Limit: MaxIncomingSourceCandidateRows}
	}
	return nil
}

func (b *IncomingSourceBudget) Remaining() int {
	return max(0, MaxIncomingSourceCandidateRows-int(b.inspected.Load()))
}

// IncomingSourceNodeQuery permits checked presence reads on the transaction
// already holding a physical reader's connection. No cursor may remain open
// while the query is used. handled=false means the reader is a different store.
type IncomingSourceNodeQuery interface {
	LookupIncomingSourceNode(context.Context, Reader, string) (exists, handled bool, err error)
}

// IncomingSourceNodeChecker distinguishes absent nodes from failed storage
// reads. Persisted layers must not use nullable NodeByID as a checked fallback.
type IncomingSourceNodeChecker interface {
	IncomingSourceNodeExists(context.Context, string, IncomingSourceNodeQuery) (bool, error)
}

type incomingSourceRule struct {
	view      *OverlaidView
	baseEdges bool
}

// IncomingSourceScope is immutable. Appending a layer copies only rule
// references, never marker IDs, source rows, or a repository corpus.
type IncomingSourceScope struct{ rules []incomingSourceRule }

func (s IncomingSourceScope) withLayer(v *OverlaidView, baseEdges bool) IncomingSourceScope {
	rules := make([]incomingSourceRule, len(s.rules)+1)
	copy(rules, s.rules)
	rules[len(s.rules)] = incomingSourceRule{view: v, baseEdges: baseEdges}
	return IncomingSourceScope{rules: rules}
}

// IncomingSourceFilter owns query-local presence caching. Scope values remain
// safe to reuse in concurrent lower/local projections.
type IncomingSourceFilter struct {
	scope    IncomingSourceScope
	presence map[*OverlaidView]map[string]bool
}

func (s IncomingSourceScope) NewFilter() *IncomingSourceFilter {
	return &IncomingSourceFilter{scope: s}
}

func (f *IncomingSourceFilter) identityVisible(ctx context.Context, view *OverlaidView, id string, query IncomingSourceNodeQuery) (bool, error) {
	if view == nil || view.layer == nil {
		return true, nil
	}
	// This is overlayOwnsIdentity's exact predicate. Test covered-file ownership
	// first: GenerationLayer.OwnsNodeIdentity may otherwise hydrate that row.
	if !view.layer.CoversNodeID(id) && !view.layer.OwnsNodeIdentity(id) {
		return true, nil
	}
	if known := f.presence[view]; known != nil {
		if visible, ok := known[id]; ok {
			return visible, nil
		}
	}
	checker, ok := view.layer.(IncomingSourceNodeChecker)
	if !ok {
		return false, ErrBoundedLocalizationUnavailable
	}
	visible, err := checker.IncomingSourceNodeExists(ctx, id, query)
	if err != nil {
		return false, err
	}
	if f.presence == nil {
		f.presence = make(map[*OverlaidView]map[string]bool)
	}
	if f.presence[view] == nil {
		f.presence[view] = make(map[string]bool)
	}
	f.presence[view][id] = visible
	return visible, nil
}

func (f *IncomingSourceFilter) TargetVisible(ctx context.Context, id string, query IncomingSourceNodeQuery) (bool, error) {
	for _, rule := range f.scope.rules {
		visible, err := f.identityVisible(ctx, rule.view, id, query)
		if err != nil || !visible {
			return visible, err
		}
	}
	return true, nil
}

func (f *IncomingSourceFilter) Allows(ctx context.Context, row IncomingSourceCandidate, query IncomingSourceNodeQuery) (bool, error) {
	if row.From == "" {
		return false, nil
	}
	for _, rule := range f.scope.rules {
		if rule.baseEdges && rule.view.overlayOwnsBaseEdge(row.From, row.FilePath) {
			return false, nil
		}
		visible, err := f.identityVisible(ctx, rule.view, row.From, query)
		if err != nil || !visible {
			return visible, err
		}
	}
	return true, nil
}

// ValidateScopedIncomingSources shares the overlay key/sentinel contract with
// physical implementations. limit<=0 is represented by Truncated for every key.
func ValidateScopedIncomingSources(targetIDs []string, limit int) ([]string, error) {
	ids := boundedIncomingTargetIDs(targetIDs)
	if err := validateIncomingSourceCandidateKeys(ids); err != nil {
		return nil, err
	}
	if limit >= maxBoundedIncomingSourceLimit {
		return nil, &BoundedLocalizationLimitError{Resource: "overlay incoming-source sentinel", Limit: maxBoundedIncomingSourceLimit - 1}
	}
	return ids, nil
}

func emptyIncomingSourceProjection() BoundedIncomingSourceProjection {
	return BoundedIncomingSourceProjection{Sources: make(map[string][]string), Truncated: make(map[string]bool)}
}

func (v *OverlaidView) FindIncomingSourcesScoped(ctx context.Context, targetIDs []string, kind EdgeKind, limit int, scope IncomingSourceScope, budget *IncomingSourceBudget) (BoundedIncomingSourceProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget == nil {
		budget = &IncomingSourceBudget{}
	}
	if err := ctx.Err(); err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	ids, err := ValidateScopedIncomingSources(targetIDs, limit)
	if err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	out := emptyIncomingSourceProjection()
	if limit <= 0 {
		for _, id := range ids {
			out.Truncated[id] = true
		}
		return out, nil
	}
	if v == nil {
		return out, nil
	}
	lowerScope, localScope := scope, scope
	if v.layer != nil {
		lowerScope = scope.withLayer(v, true)
		localScope = scope.withLayer(v, false)
	}
	lower, local := emptyIncomingSourceProjection(), emptyIncomingSourceProjection()
	if v.base != nil {
		reader, ok := v.base.(ScopedIncomingSourceReader)
		if !ok {
			return BoundedIncomingSourceProjection{}, ErrBoundedLocalizationUnavailable
		}
		lower, err = reader.FindIncomingSourcesScoped(ctx, ids, kind, limit, lowerScope, budget)
		if err != nil {
			return BoundedIncomingSourceProjection{}, err
		}
	}
	if v.layer != nil {
		reader, ok := v.layer.(ScopedIncomingSourceReader)
		if !ok {
			return BoundedIncomingSourceProjection{}, ErrBoundedLocalizationUnavailable
		}
		localIDs := make([]string, 0, len(ids))
		for _, id := range ids {
			if !lower.Truncated[id] {
				localIDs = append(localIDs, id)
			}
		}
		local, err = reader.FindIncomingSourcesScoped(ctx, localIDs, kind, limit, localScope, budget)
		if err != nil {
			return BoundedIncomingSourceProjection{}, err
		}
	}
	for _, target := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedIncomingSourceProjection{}, err
		}
		// Both inputs have already applied EVERY higher rule. More than limit
		// surviving sources in either input implies the union is truncated.
		if lower.Truncated[target] || local.Truncated[target] {
			out.Truncated[target] = true
			continue
		}
		seen := make(map[string]struct{})
		for _, source := range lower.Sources[target] {
			seen[source] = struct{}{}
		}
		for _, source := range local.Sources[target] {
			seen[source] = struct{}{}
		}
		if len(seen) > limit {
			out.Truncated[target] = true
			continue
		}
		for source := range seen {
			out.Sources[target] = append(out.Sources[target], source)
		}
		sort.Strings(out.Sources[target])
	}
	return out, nil
}

// The in-memory Graph retains its existing one-lock bounded snapshot copy.
// Only physical target adjacency is copied, never all upper nodes or markers;
// callbacks run after that lock is released. Store uses a keyset early-stop path.
func filterIncomingSourceSnapshot(ctx context.Context, targetIDs []string, kind EdgeKind, limit int, scope IncomingSourceScope, budget *IncomingSourceBudget, reader BoundedIncomingSourceCandidateReader) (BoundedIncomingSourceProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget == nil {
		budget = &IncomingSourceBudget{}
	}
	if err := ctx.Err(); err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	ids, err := ValidateScopedIncomingSources(targetIDs, limit)
	if err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	out := emptyIncomingSourceProjection()
	if limit <= 0 {
		for _, id := range ids {
			out.Truncated[id] = true
		}
		return out, nil
	}
	rows, err := reader.ReadIncomingSourceCandidates(ctx, ids, kind)
	if err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	for _, candidates := range rows {
		if err := budget.Charge(len(candidates)); err != nil {
			return BoundedIncomingSourceProjection{}, err
		}
	}
	filter := scope.NewFilter()
	for _, target := range ids {
		visible, err := filter.TargetVisible(ctx, target, nil)
		if err != nil {
			return BoundedIncomingSourceProjection{}, err
		}
		if !visible {
			continue
		}
		seen := make(map[string]struct{})
		for index, row := range rows[target] {
			if index&127 == 0 {
				if err := ctx.Err(); err != nil {
					return BoundedIncomingSourceProjection{}, err
				}
			}
			allowed, err := filter.Allows(ctx, row, nil)
			if err != nil {
				return BoundedIncomingSourceProjection{}, err
			}
			if !allowed {
				continue
			}
			seen[row.From] = struct{}{}
			if len(seen) > limit {
				out.Truncated[target] = true
				break
			}
		}
		if out.Truncated[target] {
			continue
		}
		for source := range seen {
			out.Sources[target] = append(out.Sources[target], source)
		}
		sort.Strings(out.Sources[target])
	}
	if err := ctx.Err(); err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	return out, nil
}

func (g *Graph) FindIncomingSourcesScoped(ctx context.Context, targetIDs []string, kind EdgeKind, limit int, scope IncomingSourceScope, budget *IncomingSourceBudget) (BoundedIncomingSourceProjection, error) {
	return filterIncomingSourceSnapshot(ctx, targetIDs, kind, limit, scope, budget, g)
}

func (l *OverlayLayer) FindIncomingSourcesScoped(ctx context.Context, targetIDs []string, kind EdgeKind, limit int, scope IncomingSourceScope, budget *IncomingSourceBudget) (BoundedIncomingSourceProjection, error) {
	return filterIncomingSourceSnapshot(ctx, targetIDs, kind, limit, scope, budget, l)
}

func (l *OverlayLayer) IncomingSourceNodeExists(ctx context.Context, id string, _ IncomingSourceNodeQuery) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return l != nil && l.NodeByID(id) != nil, nil
}

var (
	_ ScopedIncomingSourceReader = (*Graph)(nil)
	_ ScopedIncomingSourceReader = (*OverlayLayer)(nil)
	_ ScopedIncomingSourceReader = (*OverlaidView)(nil)
	_ IncomingSourceNodeChecker  = (*OverlayLayer)(nil)
)
