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

// IncomingSourceAdmission is the completeness fact one bounded incoming
// admission leaves behind. It is a fact, not a metric: a caller that cannot
// tell a refused admission from a complete one cannot tell a bounded
// incremental pass from one that silently stopped admitting work, and the two
// leave different graphs behind. It mirrors the shape the bounded affected-by
// fan-out publishes (internal/indexer/affected_by.go affectedByTruncation).
//
// Inspected counts every physical row THIS admission read, refused or not; a
// budget shared with other admissions holds more. Limit is the ceiling that
// applied, i.e. MaxIncomingSourceCandidateRows.
type IncomingSourceAdmission struct {
	// Keys is the deduplicated key count the admission read for.
	Keys int
	// Inspected is the physical row count this admission read and charged.
	Inspected int
	// Limit is the shared ceiling that applied.
	Limit int
	// Refused is the fact itself: the ceiling fired and NOTHING was admitted.
	Refused bool
	// Dropped is the row count the refusal left unadmitted — the size of the
	// hole a consumer's pass has. Because a refusal is whole-batch it equals
	// Inspected when Refused and is zero otherwise; it is carried explicitly so
	// a consumer reporting "how much did this pass not see" never has to infer
	// it from a counter that a shared budget also feeds.
	Dropped int
}

// AdmitIncomingRowsBounded performs ONE incoming-adjacency read for the whole
// deduplicated key set and charges every returned row against ONE shared
// IncomingSourceBudget: the same object, the same row units and the same
// MaxIncomingSourceCandidateRows ceiling filterIncomingSourceSnapshot charges
// for FindIncomingSourcesScoped. Rows are charged as read, before any
// caller-side filtering, exactly as the scoped projection charges raw
// candidates before ownership filtering.
//
// The single read is load-bearing, not incidental. The incremental frontier
// documents — and internal/resolver/batch_hotpaths_test.go guards — a constant
// number of logical store calls per pass: one file-node read, one
// outgoing-adjacency read, one incoming-stub read. Splitting the incoming read
// into key chunks would turn that constant into ceil(keys/chunk) statements on
// the per-save hot path, so this admission keeps the caller's batch shape and
// bounds what is ADMITTED — every row it returns is an edge the caller may
// rebind and durably reindex. Peak read memory is therefore whatever the
// caller's own unbounded read already cost; the ceiling governs write
// amplification, not the size of the physical batch.
//
// Exceeding the ceiling refuses the WHOLE admission: a nil map, the
// completeness fact with Refused and Dropped set, and the typed
// *BoundedLocalizationLimitError — never a partial batch. A partial incoming
// admission is indistinguishable, to every later pass and to the durable edges
// it writes back, from a complete one over a smaller corpus; the refusal keeps
// the unadmitted edges parked exactly as they were instead. Charging the whole
// batch in one call is also what lets Dropped report the true size of the hole:
// a chunked early stop can only report the rows it happened to reach.
//
// A nil budget gets a fresh one per admission, mirroring
// FindIncomingSourcesScoped's nil-budget handling. Passing one in is how two
// admissions share a ceiling.
func AdmitIncomingRowsBounded[T any](
	ctx context.Context,
	read func([]string) map[string][]T,
	keys []string,
	budget *IncomingSourceBudget,
) (map[string][]T, IncomingSourceAdmission, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget == nil {
		budget = &IncomingSourceBudget{}
	}
	fact := IncomingSourceAdmission{Limit: MaxIncomingSourceCandidateRows}
	if err := ctx.Err(); err != nil {
		return nil, fact, err
	}
	ids := boundedIncomingTargetIDs(keys)
	fact.Keys = len(ids)
	if len(ids) == 0 || read == nil {
		return make(map[string][]T), fact, nil
	}
	rows := read(ids)
	if err := ctx.Err(); err != nil {
		return nil, fact, err
	}
	for _, key := range ids {
		fact.Inspected += len(rows[key])
	}
	// A zero-row admission never charges: an empty reverse frontier must not
	// read as a refusal just because an earlier admission spent the budget.
	if fact.Inspected > 0 {
		if err := budget.Charge(fact.Inspected); err != nil {
			fact.Refused = true
			fact.Dropped = fact.Inspected
			return nil, fact, err
		}
	}
	out := make(map[string][]T, len(ids))
	for _, key := range ids {
		if batch := rows[key]; len(batch) > 0 {
			out[key] = batch
		}
	}
	return out, fact, nil
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
