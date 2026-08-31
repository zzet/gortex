package graphview

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Materializer turns a checkout's route into a readable view.
//
// It owns none of the three things it needs and holds them by
// reference: the store the payload lives in, the catalog that says which
// generations a checkout is currently routed to, and the lease manager
// that keeps those generations alive while a reader still holds them.
type Materializer struct {
	// Store is any handle on the database. Materialization reads the
	// indexed corpus through it and derives one pinned handle per routed
	// generation, so which generation the handle it was given happens to
	// be on does not matter.
	Store *store_sqlite.Store
	// Catalog answers what the checkout is routed to.
	Catalog *store_sqlite.Catalog
	// Leases pins the generations a materialized view reads. Retirement
	// consults the same manager, so a generation under a live view
	// cannot be swept.
	Leases *LeaseManager
	// Logger records the failures. A materialization that could not be
	// assembled is the one moment the caller sees an error code and
	// nothing else; the log line beside it carries the generations the
	// stack was being built from, which is what makes the code
	// diagnosable. nil silences it.
	Logger *zap.Logger
}

// GenerationSource is one persisted generation of a view's stack seen by
// a caller that has to query the generation's own indexes rather than
// read through the composed graph.
//
// The composed Reader answers node and edge questions for the whole
// stack, but a full-text index is not a read the composition can serve:
// each generation carries its own rows and a handle only ever matches
// the generation it is pinned to. A caller enumerating candidates
// therefore queries every generation itself, and needs both halves of
// this pair — the Handle to ask, and the Layer whose ownership claims
// say what this generation hides from the ones below it.
type GenerationSource struct {
	// Generation is the payload generation this source reads.
	Generation int64
	// Handle is pinned to Generation, so every index it queries answers
	// with that generation's rows alone.
	Handle *store_sqlite.Store
	// Layer is the same ownership contract the composed reader applies
	// for this generation: which paths it claims and which identities it
	// speaks for.
	Layer graph.OverlayLayerReader
}

// RepoView is one materialized repository view: the identity that names
// its content, the reader that serves it, what it can answer, and the
// lease that keeps it readable.
//
// Close is mandatory and idempotent. Until it runs, every generation the
// view reads is pinned and retirement of any of them is refused.
type RepoView struct {
	// ID names the exact content this view reads.
	ID RepoViewID
	// Reader is the composed graph: the indexed corpus with the
	// checkout's routed generations stacked on it.
	Reader graph.Reader
	// Completeness is what this view can currently answer.
	Completeness Completeness

	generations []int64
	sources     []GenerationSource
	lease       *Lease
	closeOnce   sync.Once
}

// Generations lists every payload generation this view holds a lease on,
// bottom first: any dedicated base ancestry, the commit generation, then the
// working-tree one when the route named it.
func (v *RepoView) Generations() []int64 {
	if v == nil {
		return nil
	}
	out := make([]int64, len(v.generations))
	copy(out, v.generations)
	return out
}

// GenerationSources lists the stack's generations bottom first, in the
// order they compose: any dedicated base ancestry, the commit generation,
// then the working-tree one when the route named it. Generation zero is not
// in the list — it is the shared corpus and the caller already reads it
// through its own handle.
//
// The sources are valid for as long as the view is: every handle is
// pinned to a generation the view's lease keeps from retiring.
func (v *RepoView) GenerationSources() []GenerationSource {
	if v == nil {
		return nil
	}
	out := make([]GenerationSource, len(v.sources))
	copy(out, v.sources)
	return out
}

// Close releases the view's lease. Calling it twice, or on a nil view,
// does nothing.
func (v *RepoView) Close() {
	if v == nil {
		return
	}
	v.closeOnce.Do(func() { v.lease.Release() })
}

// MaterializeCheckout builds the view a checkout's queries currently
// land on.
//
// The route carries two generation slots. The commit slot is the
// checkout's committed content and is the base of its stack — a checkout
// with no published commit generation has no view to serve, so that is
// where the view_building error comes from rather than from a thinner
// stack. It is the base in the identity's sense too: the view is named
// by that generation, and the fact that reading it costs one overlay
// level over the indexed corpus is a storage detail, not part of what
// the view is. The dirty slot is the working tree, and it is the one
// layer a materialized checkout view stacks on top; buffer layers are
// not materialized here at all, because they belong to a session rather
// than to a checkout and the MCP overlay composes them on top of this
// reader at request time exactly as it does today.
//
// Every slot the route names must be servable or the whole call fails.
// A stack that quietly dropped a layer whose generation was not ready
// would answer with content from the wrong state of the world and look
// exactly like a successful answer while doing it.
//
// The lease is taken before the generations are inspected, not after.
// Retirement refuses a leased generation, so a generation that is still
// ready once the lease is held stays ready; checking first and leasing
// afterwards would leave a window in which the sweep runs between the
// two.
func (m *Materializer) MaterializeCheckout(ctx context.Context, checkoutID string) (view *RepoView, err error) {
	var (
		graphID     string
		generations []int64
	)
	defer func() {
		m.recordMaterialization(viewmetrics.ViewWorktree, graphID, checkoutID, view, generations, err)
	}()

	if err = m.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a context")
	}
	if checkoutID == "" {
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a checkout id")
	}
	// The route read and generation lease cannot be one SQLite/in-memory
	// transaction. Pin the generations named by one route snapshot, then read
	// the route again while those pins are held. If it moved, none of the old
	// payload is opened and the current route gets another bounded attempt.
	const routeSnapshotAttempts = 3
	for attempt := 0; attempt < routeSnapshotAttempts; attempt++ {
		route, routeErr := m.routeSnapshot(ctx, checkoutID)
		if routeErr != nil {
			return nil, routeErr
		}
		graphID = route.GraphID
		if route.CommitGenerationID <= 0 {
			return nil, NewViewError(CodeViewBuilding,
				fmt.Sprintf("checkout %q has no published commit generation", checkoutID))
		}

		generations = []int64{route.CommitGenerationID}
		if route.DirtyGenerationID > 0 {
			generations = append(generations, route.DirtyGenerationID)
		}
		lease, moved, routeErr := m.pinCheckoutRoute(ctx, checkoutID, route, generations)
		if routeErr != nil {
			return nil, routeErr
		}
		if moved {
			continue
		}

		repoPrefix, prefixErr := m.repoPrefix(ctx, route.GraphID)
		if prefixErr != nil {
			lease.Release()
			return nil, prefixErr
		}
		view, err = m.assemble(ctx, route.GraphID, repoPrefix, generations, lease)
		if err != nil {
			lease.Release()
			return nil, err
		}
		// The checkout root may move while the route is being opened. Recheck
		// after assembly so an exact reader is never handed out across a move
		// journal that began during materialization. Closing releases every
		// generation lease before the building verdict escapes.
		if fenceErr := m.checkoutRootMoveFence(ctx, checkoutID); fenceErr != nil {
			view.Close()
			view = nil
			return nil, fenceErr
		}
		return view, nil
	}
	return nil, NewViewError(CodeViewBuilding,
		fmt.Sprintf("the route of checkout %q kept changing while materializing", checkoutID))
}

// routeSnapshot is the materialization preflight: route identity and the
// root-move fence come from one SQLite statement, avoiding an extra catalog
// round trip on every ready request without weakening the move linearization.
func (m *Materializer) routeSnapshot(
	ctx context.Context, checkoutID string,
) (store_sqlite.CheckoutRoute, error) {
	route, found, moving, err := m.Catalog.GetCheckoutRouteSnapshot(ctx, checkoutID)
	switch {
	case err != nil:
		return route, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read the route of checkout %q", checkoutID), err)
	case moving:
		return route, NewViewError(CodeViewBuilding,
			fmt.Sprintf("checkout %q is completing a root move", checkoutID))
	case !found:
		return route, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is not routed", checkoutID))
	case route.State == store_sqlite.RouteRetired:
		return route, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("the route of checkout %q has retired", checkoutID))
	default:
		return route, nil
	}
}

// checkoutRootMoveFence refuses checkout-specific exactness while its durable
// relocation journal is standing. The route and graph generations may already
// be valid, but runtime shells, watchers, configuration and intent locators do
// not become one coherent working-copy view until that journal is removed.
func (m *Materializer) checkoutRootMoveFence(ctx context.Context, checkoutID string) error {
	_, pending, err := m.Catalog.GetCheckoutRootMove(ctx, checkoutID)
	switch {
	case err != nil:
		return WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read pending root move for checkout %q", checkoutID), err)
	case pending:
		return NewViewError(CodeViewBuilding,
			fmt.Sprintf("checkout %q is completing a root move", checkoutID))
	default:
		return nil
	}
}

// pinCheckoutRoute closes the gap between reading a route and leasing its
// generations. A retirement can only pass its reference check after the route
// moves; if that happened before these pins landed, the second read observes
// the move and the caller retries without opening the old payload. A provisional
// pin protects the routed slots while their catalog ancestry is resolved; the
// returned lease covers the whole base -> commit -> dirty chain.
func (m *Materializer) pinCheckoutRoute(
	ctx context.Context,
	checkoutID string,
	route store_sqlite.CheckoutRoute,
	generations []int64,
) (*Lease, bool, error) {
	provisional := m.Leases.Acquire(generations...)
	current, err := m.route(ctx, checkoutID)
	if err != nil {
		provisional.Release()
		return nil, false, err
	}
	if current != route {
		provisional.Release()
		return nil, true, nil
	}
	ancestry, err := m.generationAncestry(ctx, generations)
	if err != nil {
		provisional.Release()
		return nil, false, err
	}
	lease := m.Leases.Acquire(ancestry...)
	provisional.Release()

	// A route can move while ancestry is being resolved. Re-read it after the
	// complete lease is held so the caller never opens a stale stack.
	current, err = m.route(ctx, checkoutID)
	if err != nil {
		lease.Release()
		return nil, false, err
	}
	if current != route {
		lease.Release()
		return nil, true, nil
	}
	return lease, false, nil
}

// pinGenerationAncestry closes the equivalent inspection gap for a ref view,
// which has no checkout route to revalidate. Its requested generation is
// provisionally pinned while the immutable BaseGenerationID chain is read.
func (m *Materializer) pinGenerationAncestry(ctx context.Context, generations []int64) (*Lease, error) {
	provisional := m.Leases.Acquire(generations...)
	ancestry, err := m.generationAncestry(ctx, generations)
	if err != nil {
		provisional.Release()
		return nil, err
	}
	lease := m.Leases.Acquire(ancestry...)
	provisional.Release()
	return lease, nil
}

// generationAncestry resolves the physical stack beneath the routed
// generations. The first routed generation may sit on a dedicated full base;
// later routed generations must form an exact chain above it.
func (m *Materializer) generationAncestry(ctx context.Context, generations []int64) ([]int64, error) {
	if len(generations) == 0 {
		return nil, NewViewError(CodeViewBuilding, "the view has no generations")
	}
	ancestry := make([]int64, 0, len(generations)+1)
	seen := make(map[int64]struct{}, len(generations)+1)
	for generationID := generations[0]; generationID > 0; {
		if _, duplicate := seen[generationID]; duplicate {
			return nil, NewViewError(CodeViewBuilding,
				fmt.Sprintf("generation ancestry contains a cycle at %d", generationID))
		}
		seen[generationID] = struct{}{}
		row, err := m.servableGeneration(ctx, generationID)
		if err != nil {
			return nil, err
		}
		ancestry = append(ancestry, generationID)
		generationID = row.BaseGenerationID
	}
	for left, right := 0, len(ancestry)-1; left < right; left, right = left+1, right-1 {
		ancestry[left], ancestry[right] = ancestry[right], ancestry[left]
	}

	lower := generations[0]
	for _, generationID := range generations[1:] {
		if _, duplicate := seen[generationID]; duplicate {
			return nil, NewViewError(CodeViewBuilding,
				fmt.Sprintf("generation ancestry contains a cycle at %d", generationID))
		}
		row, err := m.servableGeneration(ctx, generationID)
		if err != nil {
			return nil, err
		}
		if row.BaseGenerationID != lower {
			return nil, NewViewError(CodeViewBuilding, fmt.Sprintf(
				"generation %d sits on %d, want %d", generationID, row.BaseGenerationID, lower))
		}
		seen[generationID] = struct{}{}
		ancestry = append(ancestry, generationID)
		lower = generationID
	}
	return ancestry, nil
}

// MaterializeRefView builds the view one ref-view generation serves.
//
// A ref view names committed state nobody has checked out, so its stack is one
// layer and no more: the graph's indexed corpus with the generation describing
// the selector's tree composed onto it. There is no working-tree slot to add —
// the selector means the committed tree by definition — and no buffer layer,
// which belongs to a session rather than to a view.
//
// Everything else is what MaterializeCheckout does: the lease is taken before
// the generation is inspected, so the sweep cannot run between the check and
// the pin, and completeness comes from the generation's own producer states.
// MaterializeBase builds the sealed generation that backs one dedicated graph.
// It intentionally accepts an explicit generation instead of a checkout route:
// grace fallback must exclude every commit, dirty, filesystem, and buffer layer
// above the family's primary base.
func (m *Materializer) MaterializeBase(
	ctx context.Context, graphID string, generationID int64,
) (view *RepoView, err error) {
	var generations []int64
	defer func() {
		m.recordMaterialization(viewmetrics.ViewBase, graphID, "", view, generations, err)
	}()

	if err = m.validate(); err != nil {
		return nil, err
	}
	switch {
	case ctx == nil:
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a context")
	case graphID == "":
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a graph id")
	case generationID <= 0:
		return nil, NewViewError(CodePrimaryNotReady, "the base graph has no published generation")
	}
	generation, found, readErr := m.Catalog.GetViewGeneration(ctx, generationID)
	switch {
	case readErr != nil:
		return nil, WrapViewError(CodeViewBuilding,
			fmt.Sprintf("read base generation %d", generationID), readErr)
	case !found:
		return nil, NewViewError(CodeViewBuilding,
			fmt.Sprintf("base generation %d is not registered", generationID))
	case generation.GraphID != graphID:
		return nil, NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("generation %d belongs to graph %q, not %q", generationID, generation.GraphID, graphID))
	case generation.OwnerKind != "dedicated_base" ||
		generation.GenerationKind != "dedicated_base" ||
		generation.LayerID != graphID+":base" ||
		generation.BaseGenerationID != 0 ||
		generation.CheckoutID == "" ||
		generation.TreeOID == "":
		return nil, NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("generation %d is not a sealed dedicated base", generationID))
	case generation.State != store_sqlite.ViewGenerationReady:
		return nil, NewViewError(CodeViewBuilding,
			fmt.Sprintf("base generation %d is %s", generationID, generation.State))
	}
	dedicated, found, readErr := m.Catalog.GetDedicatedGraph(ctx, graphID)
	switch {
	case readErr != nil:
		return nil, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read graph %q", graphID), readErr)
	case !found:
		return nil, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("graph %q has no catalog row", graphID))
	case generation.CheckoutID != dedicated.OwnerCheckoutID:
		return nil, NewViewError(CodeInvalidViewSelector,
			fmt.Sprintf("generation %d is owned by checkout %q, not graph owner %q",
				generationID, generation.CheckoutID, dedicated.OwnerCheckoutID))
	}
	repoPrefix := dedicated.RepoPrefix
	generations = []int64{generationID}
	lease, err := m.pinGenerationAncestry(ctx, generations)
	if err != nil {
		return nil, err
	}
	view, err = m.assemble(ctx, graphID, repoPrefix, generations, lease)
	if err != nil {
		lease.Release()
		return nil, err
	}
	return view, nil
}

func (m *Materializer) MaterializeRefView(
	ctx context.Context, graphID string, generationID int64,
) (view *RepoView, err error) {
	var generations []int64
	defer func() {
		m.recordMaterialization(viewmetrics.ViewRef, graphID, "", view, generations, err)
	}()

	if err = m.validate(); err != nil {
		return nil, err
	}
	switch {
	case ctx == nil:
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a context")
	case graphID == "":
		return nil, NewViewError(CodeInvalidViewSelector, "materialization needs a graph id")
	case generationID <= 0:
		return nil, NewViewError(CodeViewBuilding, "the ref view has no published generation")
	}
	repoPrefix, err := m.repoPrefix(ctx, graphID)
	if err != nil {
		return nil, err
	}
	generations = []int64{generationID}
	lease, err := m.pinGenerationAncestry(ctx, generations)
	if err != nil {
		return nil, err
	}
	view, err = m.assemble(ctx, graphID, repoPrefix, generations, lease)
	if err != nil {
		lease.Release()
		return nil, err
	}
	return view, nil
}

// recordMaterialization counts one materialization attempt and, when it
// failed, logs what it was assembling.
//
// The counter is aggregate by construction — kind and outcome, nothing that
// names a checkout — so the log line is the only place the ids live: the
// stack's generation ids and the graph and checkout they were routed for, plus
// the fingerprint of the view when one was composed. That pairing is the whole
// contract: a rise in the error counter says a view stopped materializing, and
// the log says which one and over what.
func (m *Materializer) recordMaterialization(
	kind, graphID, checkoutID string,
	view *RepoView,
	generations []int64,
	err error,
) {
	if err == nil {
		viewmetrics.Count(viewmetrics.MaterializationTotal, kind, viewmetrics.OutcomeOK)
		return
	}
	viewmetrics.Count(viewmetrics.MaterializationTotal, kind, viewmetrics.OutcomeError)
	if m == nil || m.Logger == nil {
		return
	}
	fields := []zap.Field{
		zap.String("view_kind", kind),
		zap.String("code", CodeOf(err)),
		zap.Int64s("generations", generations),
		zap.Error(err),
	}
	if graphID != "" {
		fields = append(fields, zap.String("graph", graphID))
	}
	if checkoutID != "" {
		fields = append(fields, zap.String("checkout", checkoutID))
	}
	if view != nil {
		fields = append(fields, zap.String("view_fingerprint", view.ID.Fingerprint()))
	}
	m.Logger.Warn("graph view: materialization failed", fields...)
}

// validate refuses a Materializer that cannot do its job, so a missing
// dependency reports itself here instead of as a nil dereference inside
// a read.
func (m *Materializer) validate() error {
	switch {
	case m == nil || m.Store == nil:
		return NewViewError(CodeInvalidViewSelector, "materializer needs a store")
	case m.Catalog == nil:
		return NewViewError(CodeInvalidViewSelector, "materializer needs a catalog")
	case m.Leases == nil:
		return NewViewError(CodeInvalidViewSelector, "materializer needs a lease manager")
	default:
		return nil
	}
}

// route reads the checkout's current route. A route that does not exist
// or has retired is a checkout nothing can be read from — the failure is
// about the checkout, not about the selector that named it.
func (m *Materializer) route(ctx context.Context, checkoutID string) (store_sqlite.CheckoutRoute, error) {
	route, found, err := m.Catalog.GetCheckoutRoute(ctx, checkoutID)
	switch {
	case err != nil:
		return route, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read the route of checkout %q", checkoutID), err)
	case !found:
		return route, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("checkout %q is not routed", checkoutID))
	case route.State == store_sqlite.RouteRetired:
		return route, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("the route of checkout %q has retired", checkoutID))
	default:
		return route, nil
	}
}

// repoPrefix resolves the repository the routed graph carries. The
// prefix is part of the view identity, so a graph the catalog has no row
// for cannot be named and cannot be served.
func (m *Materializer) repoPrefix(ctx context.Context, graphID string) (string, error) {
	dedicated, found, err := m.Catalog.GetDedicatedGraph(ctx, graphID)
	switch {
	case err != nil:
		return "", WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read graph %q", graphID), err)
	case !found:
		return "", NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("graph %q has no catalog row", graphID))
	default:
		return dedicated.RepoPrefix, nil
	}
}

// assemble builds the layer stack, the identity and the completeness of
// a leased set of generations. It runs with the lease already held, so
// every generation it reads is safe from the sweep.
//
// generations is the stack bottom first: the generation that names the
// view's committed content, then any layer stacked over it — the
// working-tree generation for a checkout, nothing at all for a ref view.
// The routed commit generation remains the identity base. Its physical
// BaseGenerationID ancestry is a storage concern: the oldest nonzero ancestor
// is a flat dedicated corpus, any descendants compose above it, and all of them
// are exposed and leased as generation sources.
func (m *Materializer) assemble(
	ctx context.Context,
	graphID string,
	repoPrefix string,
	generations []int64,
	lease *Lease,
) (*RepoView, error) {
	ancestry, err := m.generationAncestry(ctx, generations)
	if err != nil {
		return nil, err
	}
	routedStart := len(ancestry) - len(generations)
	if routedStart < 0 {
		return nil, NewViewError(CodeViewBuilding, "generation ancestry is incomplete")
	}

	handles := make([]*store_sqlite.Store, 0, len(ancestry))
	sources := make([]GenerationSource, 0, len(ancestry))
	open := func(generationID int64) (*store_sqlite.Store, *GenerationLayer, store_sqlite.ViewGeneration, error) {
		handle, layer, row, openErr := m.openGeneration(ctx, generationID)
		if openErr == nil {
			handles = append(handles, handle)
			sources = append(sources, GenerationSource{
				Generation: generationID, Handle: handle, Layer: layer,
			})
		}
		return handle, layer, row, openErr
	}

	var base graph.Reader = m.Store.AtGeneration(0)
	firstOverlay := 0
	if routedStart > 0 {
		handle, _, _, err := open(ancestry[0])
		if err != nil {
			return nil, err
		}
		base = handle
		firstOverlay = 1
	}
	for index := firstOverlay; index <= routedStart; index++ {
		_, layer, _, err := open(ancestry[index])
		if err != nil {
			return nil, err
		}
		base = graph.NewOverlaidViewWithLayer(base, layer)
	}

	var (
		layers    []graph.OverlayLayerReader
		layerRefs []LayerRef
	)
	for index := routedStart + 1; index < len(ancestry); index++ {
		generationID := ancestry[index]
		_, layer, row, err := open(generationID)
		if err != nil {
			return nil, err
		}
		ref, err := dirtyLayerRef(row)
		if err != nil {
			return nil, err
		}
		layers = append(layers, layer)
		layerRefs = append(layerRefs, ref)
	}

	id, err := NewRepoViewID(repoPrefix, graphID, generations[0], layerRefs...)
	if err != nil {
		return nil, err
	}
	reader, id, err := ComposeRepoView(base, layers, id)
	if err != nil {
		return nil, err
	}
	completeness, err := m.completeness(handles)
	if err != nil {
		return nil, err
	}
	return &RepoView{
		ID:           id,
		Reader:       reader,
		Completeness: completeness,
		generations:  ancestry,
		sources:      sources,
		lease:        lease,
	}, nil
}

// openGeneration checks that one routed generation can be served and
// returns the handle pinned to it, the layer over that handle, and the
// catalog row the identity is built from.
func (m *Materializer) openGeneration(ctx context.Context, generationID int64) (
	*store_sqlite.Store, *GenerationLayer, store_sqlite.ViewGeneration, error,
) {
	row, err := m.servableGeneration(ctx, generationID)
	if err != nil {
		return nil, nil, row, err
	}
	handle := m.Store.AtGeneration(generationID)
	layer, err := NewGenerationLayer(handle)
	if err != nil {
		return nil, nil, row, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("open generation %d", generationID), err)
	}
	return handle, layer, row, nil
}

// servableGeneration reads one routed generation and reports whether it
// can be served.
//
// ready and superseded both serve: superseded says only that a newer
// generation exists, and the route — not the newness of a generation —
// decides what a checkout reads. building is the retryable case a caller
// polls. Anything else is content that is going away or never arrived,
// and serving from it would mean answering out of a payload the sweep
// may already be deleting.
func (m *Materializer) servableGeneration(ctx context.Context, generationID int64) (store_sqlite.ViewGeneration, error) {
	row, found, err := m.Catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		return row, WrapViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("read generation %d", generationID), err)
	}
	if !found {
		return row, NewViewError(CodeViewBuilding,
			fmt.Sprintf("generation %d is not in the catalog", generationID))
	}
	switch row.State {
	case store_sqlite.ViewGenerationReady, store_sqlite.ViewGenerationSuperseded:
		return row, nil
	case store_sqlite.ViewGenerationBuilding:
		return row, NewViewError(CodeViewBuilding,
			fmt.Sprintf("generation %d is still building", generationID))
	default:
		return row, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("generation %d is %s", generationID, string(row.State)))
	}
}

// dirtyLayerRef names the working-tree layer of a checkout's stack. A
// generation that names no layer cannot be identified, and an identity
// that cannot be built is a view that cannot be cached or compared.
func dirtyLayerRef(row store_sqlite.ViewGeneration) (LayerRef, error) {
	if row.LayerID == "" {
		return LayerRef{}, NewViewError(CodeCheckoutInaccessible,
			fmt.Sprintf("generation %d names no layer", row.GenerationID))
	}
	ref := LayerRef{Kind: LayerDirty, LayerID: row.LayerID, Generation: row.GenerationID}
	if err := ref.Validate(); err != nil {
		return LayerRef{}, err
	}
	return ref, nil
}

// completeness unions the producer states of the whole stack, taking the
// worst state any generation declares for a capability: a generation
// stacked on top answers for the files it built and cannot repair what a
// generation below it left partial, so a stack whose commit generation
// truncated its closure stays incomplete no matter how whole the
// working-tree build on top of it was.
//
// The union starts from the base corpus, which declares nothing and is
// complete for everything. That is not an omission: a producer row is
// written per derived generation, and the corpus underneath them is a
// plain whole index, so its contribution to any capability is complete
// by construction and there is no row to read — SetProducerState refuses
// a base handle for exactly that reason. Only a generation stacked on it
// can narrow a capability, which is the direction the union runs, and a
// generation that leaves a capability out is saying it did not touch it
// rather than that it withdrew it.
//
// A producer that does not name a known capability is skipped rather
// than recorded: producer names are a build-side vocabulary and the ones
// that do not correspond to something a caller can require are build
// stages, not answers a view offers.
func (m *Materializer) completeness(generations []*store_sqlite.Store) (Completeness, error) {
	known := KnownCapabilities()
	out := make(Completeness, len(known))
	for _, id := range known {
		out[id] = StateComplete
	}
	for _, handle := range generations {
		rows, err := handle.ProducerStates()
		if err != nil {
			return nil, WrapViewError(CodeCheckoutInaccessible,
				fmt.Sprintf("read producer states of generation %d", handle.ViewGeneration()), err)
		}
		for _, row := range rows {
			id := CapabilityID(row.Producer)
			state := capabilityStateOf(row.State)
			if !id.Valid() || !state.Valid() {
				continue
			}
			out[id] = out[id].worst(state)
		}
	}
	return out, nil
}

// capabilityStateOf maps a producer's contribution state onto what a
// caller sees. The two vocabularies are one-to-one: a producer is the
// thing that populates a capability, so how far along it is IS how far
// along the capability is.
func capabilityStateOf(state store_sqlite.ProducerState) CapabilityState {
	switch state {
	case store_sqlite.ProducerStateComplete:
		return StateComplete
	case store_sqlite.ProducerStateIncomplete:
		return StateIncomplete
	case store_sqlite.ProducerStateBuilding:
		return StateBuilding
	case store_sqlite.ProducerStateUnavailable:
		return StateUnavailable
	case store_sqlite.ProducerStateDisabledByConfig:
		return StateDisabledByConfig
	default:
		return CapabilityState("")
	}
}
