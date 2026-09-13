package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// The payload-generation lifecycle: begin, publish, route, retire.
//
// A payload generation is created in the catalog's building state and written
// through the ordinary Store surface — AddBatch, the sidecar setters, the
// ownership-mask writers — on the handle BeginPayloadGeneration returns. There
// is no second write path: a derived generation is populated by exactly the
// statements a plain index runs, with the handle's generation bound instead of
// the base one.
//
// Publishing moves the row to ready, which the catalog already treats as
// immutable. The payload must become immutable at the same moment, so a
// published generation's handle stops admitting writes at the write gate — see
// payloadSeal. Retirement is the inverse: the catalog row goes to retiring, the
// generation's payload rows are deleted in bounded chunks, and the row itself
// goes last, so a killed retire can simply run again.

// Payload-generation errors.
var (
	// ErrPayloadGenerationSealed means a write was attempted through a handle
	// whose generation is no longer building.
	ErrPayloadGenerationSealed = errors.New("store_sqlite: payload generation is published and no longer writable")

	// ErrPayloadGenerationInUse means retirement was refused because a lease
	// holder still reads the generation.
	ErrPayloadGenerationInUse = errors.New("store_sqlite: payload generation is leased")

	// ErrPayloadGenerationIncomplete means a publish was refused because a
	// producer has not finished contributing to the generation.
	ErrPayloadGenerationIncomplete = errors.New("store_sqlite: payload generation has a producer still building")

	// ErrPayloadGenerationRetired means a derived-plane write was attempted
	// through a handle whose generation is being retired, or has already been
	// swept. Payload writes never see this — they are refused earlier and more
	// coarsely by ErrPayloadGenerationSealed, which covers every non-building
	// state. The analysis cache needs the finer distinction: it must keep
	// admitting writes through a PUBLISHED generation's handle (that is the
	// routed read path's whole point) while refusing them once the retirement
	// sweep has started, because the sweep snapshots the analysis ids it will
	// delete and a later write would leave rows for a corpus that is gone.
	ErrPayloadGenerationRetired = errors.New("store_sqlite: payload generation is being retired")
)

// payloadSeal is the write-admission flag for one derived payload generation.
// Every handle over that generation shares the one instance held in the core's
// payloadSeals map, so publishing flips them all at once.
//
// The flag is resolved lazily: a fresh handle starts unknown and pays a single
// catalog point read on its first write, after which the write gate costs one
// atomic load. A generation with no catalog row at all is open — such rows are
// written by callers that manage generations themselves, and the lifecycle
// here does not claim authority over them. (The analysis plane's own check,
// refuseRetiredAnalysisWrite, qualifies that further: a missing row for an id
// the catalog has already minted is a retired generation, not an unmanaged
// one.)
type payloadSeal struct {
	state atomic.Int32
	// sweep is the generation's retirement state — how far the last sweep pass
	// got and why it stopped. It lives beside the flag because the seal is the
	// one object every handle on the generation already shares, and because it
	// is dropped at exactly the moment that state stops being true: when the
	// generation is finally retired. See payload_generation_sweep.go.
	sweep payloadSweepState
}

const (
	payloadSealUnknown int32 = iota
	payloadSealOpen
	payloadSealSealed
	// payloadSealRetired is a sealed generation whose rows are being deleted.
	// The payload write gate treats it exactly like payloadSealSealed; only
	// the derived planes that deliberately bypass that gate — the analysis
	// cache — read the two apart. Keeping it a distinct verdict is what lets
	// the fast path stay one atomic load: payloadSealSealed now strictly means
	// "published", so an analysis write on it needs no catalog read.
	payloadSealRetired
)

// payloadSealFor returns the flag shared by every handle on generation g.
func (s *Store) payloadSealFor(g int64) *payloadSeal {
	if s.coreless() || g == baseViewGeneration {
		return nil
	}
	if cached, ok := s.payloadSeals.Load(g); ok {
		return cached.(*payloadSeal)
	}
	shared, _ := s.payloadSeals.LoadOrStore(g, &payloadSeal{})
	return shared.(*payloadSeal)
}

// payloadSealIfPresent is payloadSealFor without the minting half: it answers
// for a generation this process already holds state about, and nil for one it
// does not.
//
// It exists because the seal map is the only per-generation state the store
// keeps in memory, and its size is bounded by the generations this process has
// opened a handle on and not yet retired. A lookup that mints — which is the
// right default for the write gate, where the caller is holding the generation
// — would let anything that merely names a generation id grow that map without
// bound, and the map is ranged on every health census. Every reader that takes
// an id from outside the lifecycle (the storage-failure register) goes through
// here instead.
func (s *Store) payloadSealIfPresent(g int64) *payloadSeal {
	if s.coreless() || g == baseViewGeneration {
		return nil
	}
	if cached, ok := s.payloadSeals.Load(g); ok {
		return cached.(*payloadSeal)
	}
	return nil
}

// refuseSealedPayloadWrite is the write gate's generation check. The base
// handle carries no flag and returns immediately; a derived handle costs one
// atomic load once its flag has been resolved.
func (s *Store) refuseSealedPayloadWrite() error {
	seal := s.seal
	if seal == nil {
		return nil
	}
	switch seal.state.Load() {
	case payloadSealOpen:
		return nil
	case payloadSealSealed, payloadSealRetired:
		// Payload writes do not distinguish the two: everything past building
		// is closed to them, and the error they have always reported is the
		// one callers match on.
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationSealed, s.viewGen)
	}
	return s.resolvePayloadSeal(seal)
}

// resolvePayloadSeal reads the generation's catalog state once and caches the
// verdict. It runs before any write transaction is opened, so it never queries
// through a connection it is itself holding.
func (s *Store) resolvePayloadSeal(seal *payloadSeal) error {
	verdict, state, found, err := s.catalogSealVerdict()
	if err != nil {
		return err
	}
	if verdict == payloadSealOpen {
		if !found {
			// A row-less generation is open to payload writes exactly as it has
			// always been, but it must not CACHE that verdict when the reason
			// the row is missing is that retirement deleted it: the analysis
			// plane's own check reads this same flag, and a cached open there
			// would let a write into a swept generation through on the fast
			// path. Leaving the flag unresolved costs a re-read on a handle
			// nothing legitimate holds.
			tombstoned, markErr := s.payloadGenerationTombstoned()
			if markErr != nil {
				return markErr
			}
			if tombstoned {
				// Declining to cache must not also drop a refusal. This is the
				// path openPayloadSeal's post-CAS re-read used to cover: a
				// handle that loaded unknown before RetirePayloadGeneration
				// sealed the shared flag, and whose catalog read completed
				// after the same retire deleted the row, reaches here with the
				// flag already saying retired. Returning nil would ADMIT it to
				// the payload write gate — past the point drainPayloadWriters
				// can wait for it — so the shared flag is honoured here exactly
				// as openPayloadSeal honours it, only without storing a verdict.
				switch seal.state.Load() {
				case payloadSealSealed, payloadSealRetired:
					return fmt.Errorf("%w: generation %d", ErrPayloadGenerationSealed, s.viewGen)
				}
				return nil
			}
		}
		return s.openPayloadSeal(seal)
	}
	seal.state.CompareAndSwap(payloadSealUnknown, verdict)
	return fmt.Errorf("%w: generation %d is %s", ErrPayloadGenerationSealed, s.viewGen, state)
}

// catalogSealVerdict reads this handle's generation state from the catalog
// once and maps it to a seal verdict. A generation with no catalog row is
// open: such rows are written by callers that manage generations themselves,
// and the lifecycle here does not claim authority over them. The third result
// reports whether a row was found, because "open" alone cannot tell an
// unmanaged generation apart from one whose row retirement already deleted —
// see payloadGenerationTombstoned.
//
// The retiring state gets its own verdict so payloadSealSealed strictly means
// "published". That is what keeps the analysis cache's admission check on the
// cheap atomic path in the routed case, which is the common one.
func (s *Store) catalogSealVerdict() (int32, string, bool, error) {
	var state string
	err := s.db.QueryRow(
		`SELECT state FROM view_generations WHERE generation_id = ?`, s.viewGen).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return payloadSealOpen, "", false, nil
	case err != nil:
		return payloadSealUnknown, "", false, err
	case ViewGenerationState(state) == ViewGenerationBuilding:
		return payloadSealOpen, state, true, nil
	case ViewGenerationState(state) == ViewGenerationRetiring:
		return payloadSealRetired, state, true, nil
	}
	return payloadSealSealed, state, true, nil
}

// payloadGenerationTombstoned reports whether this handle's generation once had
// a catalog row that is now gone — the durable tombstone a completed retirement
// leaves behind.
//
// Only the caller that has already established there is no view_generations row
// may ask. Two shapes produce that: a generation the lifecycle never minted (a
// caller managing generations itself, which catalogSealVerdict calls open), and
// one RetirePayloadGeneration swept and deleted. The catalog itself separates
// them: view_generations.generation_id is INTEGER PRIMARY KEY AUTOINCREMENT
// (schema.go), so SQLite keeps the highest id ever minted in sqlite_sequence and
// never lowers it when rows are deleted — DELETE FROM, with or without a WHERE
// clause, leaves the sequence standing. An id at or below that high-water mark
// with no row was therefore allocated and then deleted, and the only production
// caller of Catalog.DeleteViewGeneration is RetirePayloadGeneration itself.
//
// A missing sqlite_sequence row means no generation was ever minted, which
// reads as a high-water mark of 0 and tombstones nothing (generation ids start
// at 1).
//
// The read is not atomic with the caller's own row lookup: a generation being
// minted concurrently could raise the mark between the two reads and be
// reported tombstoned. That refuses a write on a handle whose generation
// another writer is creating right now — conservative, and not a shape any
// caller produces, since a handle is derived from the id its own begin
// returned.
func (s *Store) payloadGenerationTombstoned() (bool, error) {
	var mark int64
	err := s.db.QueryRow(
		`SELECT seq FROM sqlite_sequence WHERE name = 'view_generations'`).Scan(&mark)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.viewGen <= mark, nil
}

// openPayloadSeal caches the open verdict the catalog read produced. A publish
// or a retire may have sealed the generation while that read was in flight, in
// which case the flag they stored is the newer verdict and the write is
// refused on it rather than on the state the read saw.
func (s *Store) openPayloadSeal(seal *payloadSeal) error {
	if seal.state.CompareAndSwap(payloadSealUnknown, payloadSealOpen) {
		return nil
	}
	switch seal.state.Load() {
	case payloadSealSealed, payloadSealRetired:
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationSealed, s.viewGen)
	}
	return nil
}

// refuseRetiredAnalysisWrite is the analysis cache's admission check.
//
// The cache's transactions deliberately run on the base handle
// (beginAnalysisWrite), which is what lets a PUBLISHED generation still cache
// an analysis. That bypass also skips the one thing that used to refuse a
// write into a generation being deleted, so the analysis plane has to make the
// finer decision itself: published is admitted, retiring is not. Without this,
// a BeginAnalysisGeneration interleaving the retirement sweep — which releases
// writeMu between chunks and only ever re-checks the PAYLOAD generation's
// state — leaves manifest, child and pointer rows for a corpus that is gone,
// and generation ids are never reissued, so nothing ever collects them.
//
// The in-memory seal is not enough on its own. RetirePayloadGeneration ends by
// dropping the shared flag (payloadSeals.Delete), so a handle derived at that
// id afterwards starts unknown, finds no catalog row, and would read as open.
// The catalog's own allocation high-water mark is consulted for exactly that
// case, which is what makes the refusal survive the seal's disposal and a
// process restart.
func (s *Store) refuseRetiredAnalysisWrite() error {
	seal := s.seal
	if seal == nil {
		// The base corpus. RetirePayloadGeneration refuses generation ids at
		// or below baseViewGeneration, so it never retires.
		return nil
	}
	switch seal.state.Load() {
	case payloadSealOpen, payloadSealSealed:
		return nil
	case payloadSealRetired:
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationRetired, s.viewGen)
	}
	// Unknown: this handle has never resolved its generation, which is the
	// ordinary case for one that only ever writes the derived plane. One
	// catalog point read, cached like every other seal verdict.
	verdict, _, found, err := s.catalogSealVerdict()
	if err != nil {
		return err
	}
	if !found {
		// No row. Either a generation the lifecycle never minted — open, as it
		// has always been — or one retirement swept and deleted.
		tombstoned, markErr := s.payloadGenerationTombstoned()
		if markErr != nil {
			return markErr
		}
		if tombstoned {
			// Deliberately not cached on the shared seal: a retired-and-deleted
			// id is unreachable for any legitimate caller, and storing a verdict
			// here would change what the PAYLOAD write gate reports for the same
			// handle, which this check must leave byte-identical.
			return fmt.Errorf("%w: generation %d", ErrPayloadGenerationRetired, s.viewGen)
		}
	}
	seal.state.CompareAndSwap(payloadSealUnknown, verdict)
	// Re-read: a publish or a retire may have stored its own verdict while the
	// catalog read was in flight, and that verdict is the newer one.
	if seal.state.Load() == payloadSealRetired {
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationRetired, s.viewGen)
	}
	return nil
}

// setPayloadSeal forces the cached verdict for a generation, so a publish does
// not have to wait for a handle to re-read the catalog.
func (s *Store) setPayloadSeal(generationID int64, state int32) {
	if seal := s.payloadSealFor(generationID); seal != nil {
		seal.state.Store(state)
	}
}

// PayloadGenerationRequest describes the generation to begin: who owns it,
// which layer it is built over, and the fingerprint of the inputs that
// produced it. The fingerprint fields are what make two begins for the same
// layer the same build.
type PayloadGenerationRequest struct {
	OwnerKind      string
	GraphID        string
	LayerID        string
	CheckoutID     string
	GenerationKind string

	// BaseGenerationID is the layer beneath this one, 0 for the base corpus.
	BaseGenerationID int64

	LowerViewFingerprint string
	TreeOID              string
	ProvenanceCommitOID  string
	ConfigHash           string
	ExtractorVersions    string
	ResolverVersion      string
	DependencyRevision   string

	CreatedAt int64 // unix seconds
}

// BeginPayloadGeneration creates a building generation and returns its id
// together with the handle to write it through. Writers then populate the
// generation with the same calls a plain index makes.
//
// A second begin naming the same layer and the same inputs adopts the
// generation already in flight instead of minting a second one; the returned
// handle addresses that generation. Call BeginPayloadGenerationWithStatus
// when the caller must distinguish those two outcomes.
func (s *Store) BeginPayloadGeneration(ctx context.Context, req PayloadGenerationRequest) (int64, *Store, error) {
	generationID, handle, _, err := s.BeginPayloadGenerationWithStatus(ctx, req)
	return generationID, handle, err
}

// BeginPayloadGenerationWithStatus is BeginPayloadGeneration with the catalog's
// adoption verdict preserved. adopted is true when the returned handle belongs
// to an already-building generation with the same complete input identity.
// Callers that use the verdict must still serialize identical builds: adoption
// does not transfer ownership away from a live writer.
func (s *Store) BeginPayloadGenerationWithStatus(
	ctx context.Context,
	req PayloadGenerationRequest,
) (generationID int64, handle *Store, adopted bool, err error) {
	if ctx == nil {
		return 0, nil, false, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	generationID, adopted, err = s.Catalog().AdoptOrCreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:            req.OwnerKind,
		GraphID:              req.GraphID,
		LayerID:              req.LayerID,
		CheckoutID:           req.CheckoutID,
		GenerationKind:       req.GenerationKind,
		BaseGenerationID:     req.BaseGenerationID,
		LowerViewFingerprint: req.LowerViewFingerprint,
		TreeOID:              req.TreeOID,
		ProvenanceCommitOID:  req.ProvenanceCommitOID,
		ConfigHash:           req.ConfigHash,
		ExtractorVersions:    req.ExtractorVersions,
		ResolverVersion:      req.ResolverVersion,
		DependencyRevision:   req.DependencyRevision,
		State:                ViewGenerationBuilding,
		CreatedAt:            req.CreatedAt,
	})
	if err != nil {
		return 0, nil, false, err
	}
	handle, err = s.AtManagedGeneration(generationID)
	if err != nil {
		return 0, nil, false, err
	}
	return generationID, handle, adopted, nil
}

// PublishPayloadGeneration validates a building generation and moves it to
// ready.
//
// The order matters. The generation is sealed first and the mutation gate is
// then taken and released once, which drains the writers that passed the write
// gate before the seal closed. Only against a payload nothing can add to any
// more are the masks checked, the producers checked for one still running, and
// the rollup measured and stored; a writer admitted after those probes ran
// would land rows inside the published payload that were never validated nor
// counted — a ready generation whose delete mask contradicts its own rows, or
// whose covered_files does not match them.
//
// Sealing before the checks rather than after is what makes that window empty.
// The catalog writes the checks feed run through the base handle, which the
// payload seal does not cover, so the seal costs the transition nothing.
//
// A failure anywhere after the seal closes returns the flag to unknown rather
// than forcing it open: the generation may have left building underneath us —
// a racing publisher that won the transition, a racing retire — and re-reading
// the catalog on the next write is the only verdict that stays right in every
// case. A generation that really is still building resolves back to open on
// that read, so the caller can fix what was refused and retry.
//
// This is the daemon's PHYSICAL publish — the one the generation builder calls
// as its last step (internal/indexer/builder_generation.go, the
// SparseGenerationBuilder tail) — so it is where the planner-statistics refresh
// a publish owes is asked for. A published generation adds a whole checkout's
// payload in one step, and issue #651 is what a store planning against
// statistics that describe a fraction of itself does: a zero sqlite_stat1 row
// flips the rebind plan onto the wrong outer loop. The refresh is SCHEDULED
// onto the maintenance lane (store_compact.go), never run here: sqlite_stat1 is
// one table for one database file, no generation owns it, and no generation's
// publish may be charged for it. The lane runs it once the publish drains and
// payload builds in flight have finished, and coalesces a burst of publishes
// into a single pass.
//
// How close that gets to "after the route flip", stated exactly, because the
// store cannot see a flip it does not perform: the builder holds its payload
// build flight across this call (internal/indexer/builder_generation.go joins
// it before the build and completes it in a defer that runs after this
// returns), so a pass scheduled here cannot start before the builder has
// returned to its caller — the checkout coordinator, whose very next act is the
// flip. What is left is that last hop, and it costs at most latency: the
// refresh never QUEUES on the write gate (planner_stats_freshness.go try-locks
// per index and gives up on a busy gate), so a flip arriving mid-pass waits for
// one index's ANALYZE at worst, bounded by plannerStatsIndexTimeout, and never
// for a whole pass. A caller that publishes and flips through this package —
// PublishAndRoute — keeps the stronger property by asking below its own flip.
func (s *Store) PublishPayloadGeneration(ctx context.Context, generationID, publishedAt int64) error {
	return s.publishPayloadGeneration(ctx, generationID, publishedAt, true)
}

// publishPayloadGeneration is the publish half both entry points share.
//
// scheduleMaintenance is false for exactly one caller: PublishAndRoute, which
// owns a WIDER window than the publish — it publishes and then flips a route,
// and between the two the generation is ready but unrouted. That caller asks
// the lane itself, after its flip, so one publish window still produces exactly
// one lane request rather than two.
func (s *Store) publishPayloadGeneration(ctx context.Context, generationID, publishedAt int64, scheduleMaintenance bool) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if generationID <= baseViewGeneration {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	catalog := s.Catalog()
	row, found, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		return err
	} else if !found {
		return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
	}

	// The publish window is the closure below, and nothing but the transition
	// happens inside it. Whole-database maintenance (ANALYZE / VACUUM /
	// TRUNCATE checkpoint) reads publishDrains and waits the window out: a
	// publish must pay no maintenance inside its own transaction window, and
	// equally no maintenance may rewrite the file underneath a sealed
	// generation whose transition has not committed yet.
	if err := func() error {
		s.publishDrains.Add(1)
		defer s.publishDrains.Add(-1)

		s.setPayloadSeal(generationID, payloadSealSealed)
		if err := s.drainPayloadWriters(ctx); err != nil {
			s.setPayloadSeal(generationID, payloadSealUnknown)
			return err
		}
		if err := s.publishSealedGeneration(ctx, catalog, generationID, publishedAt); err != nil {
			s.setPayloadSeal(generationID, payloadSealUnknown)
			return err
		}
		return nil
	}(); err != nil {
		return err
	}
	viewmetrics.Count(viewmetrics.GenerationPublishedTotal, generationOwner(row.OwnerKind))
	// Asked AFTER the window closed, not inside it: the request itself is two
	// atomics and at most one goroutine start, but a request made inside the
	// window would be a publish doing maintenance bookkeeping in its own
	// transaction span, and the counter it reads would still name itself.
	// A failed publish asks for nothing — it added no payload to notice.
	if scheduleMaintenance {
		s.schedulePublishMaintenance()
	}
	return nil
}

// Owner kinds a payload generation's catalog row can carry. They mirror the
// owner_kind strings the coordinator and the ref-view manager stamp; only the
// metric mapping below reads them, so the writers stay the authority on the
// values themselves.
const (
	checkoutGenerationOwnerKind = "dedicated_graph"
	refViewGenerationOwnerKind  = "ref_view"
)

// generationOwner maps a catalog owner kind onto the bounded metric label. An
// owner this build does not know collapses to the registry's other bucket
// rather than minting a series.
func generationOwner(ownerKind string) string {
	switch ownerKind {
	case checkoutGenerationOwnerKind:
		return viewmetrics.OwnerCheckout
	case refViewGenerationOwnerKind:
		return viewmetrics.OwnerRefView
	default:
		return viewmetrics.LabelOther
	}
}

// drainPayloadWriters waits out every write that passed the write gate before
// the seal closed. The gate check and the commit that follows it happen under
// one hold of the mutation gate, so taking that gate once and releasing it is
// the barrier: once it returns, no admitted write is still in flight.
func (s *Store) drainPayloadWriters(ctx context.Context) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	s.writeMu.Unlock()
	return nil
}

// publishSealedGeneration runs the publish checks against a sealed payload and
// commits the transition. The reads bind the generation; the two catalog
// writes go through the base handle, both guarded on the building state, so a
// generation another publisher already moved fails here instead of being
// published twice.
func (s *Store) publishSealedGeneration(ctx context.Context, catalog *Catalog, generationID, publishedAt int64) error {
	handle := s.AtGeneration(generationID)
	if err := handle.ValidateGenerationMasks(); err != nil {
		return err
	}
	if err := handle.requireProducersSettled(ctx); err != nil {
		return err
	}
	covered, affected, bytes, err := handle.payloadRollup(ctx)
	if err != nil {
		return err
	}
	if err := catalog.UpdateViewGenerationRollup(ctx, generationID, covered, affected, bytes); err != nil {
		return err
	}
	return catalog.PublishViewGeneration(ctx, generationID, publishedAt)
}

// requireProducersSettled refuses a generation a producer has not finished
// contributing to. The probe is a leading-key seek on the completeness table's
// primary key, so it costs one index range regardless of graph size.
func (s *Store) requireProducersSettled(ctx context.Context) error {
	var producer string
	err := s.db.QueryRowContext(ctx, `
SELECT producer FROM generation_producer_completeness
 WHERE view_gen = ? AND state = ? ORDER BY producer LIMIT 1`,
		s.viewGen, string(ProducerStateBuilding)).Scan(&producer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: generation %d: producer %q", ErrPayloadGenerationIncomplete, s.viewGen, producer)
}

// payloadRollup measures what the generation carries: the files it holds, the
// files it makes an ownership claim about, and the source bytes behind them.
//
// storage_bytes is the summed size of those files, not a database page count:
// pages are not partitioned by generation, so no per-generation page figure
// exists to report. All three probes are leading-key seeks on the generation.
func (s *Store) payloadRollup(ctx context.Context) (covered, affected, bytes int64, err error) {
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files WHERE view_gen = ?`,
		s.viewGen).Scan(&covered, &bytes); err != nil {
		return 0, 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM generation_file_masks WHERE view_gen = ?`,
		s.viewGen).Scan(&affected); err != nil {
		return 0, 0, 0, err
	}
	return covered, affected, bytes, nil
}

// PublishAndRoute publishes a generation and then points one slot of a
// checkout's route at it.
//
// The two steps are not one transaction and deliberately so: publishing is
// about the generation, flipping is about the checkout, and the flip's
// compare-and-set can lose to a concurrent reconciler long after the publish
// has committed. A lost flip leaves the generation ready but unrouted, which is
// a legal resting state — whether to supersede it is the caller's call, and
// MarkPayloadGenerationSuperseded is how it says so.
func (s *Store) PublishAndRoute(ctx context.Context, generationID int64, checkoutID string, expectRouteEpoch int64, slot RouteSlot) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	publishedAt, err := s.publishTimestamp(ctx)
	if err != nil {
		return err
	}
	// The publish half deliberately does NOT schedule: this API's window is the
	// wider one (publish, then flip), and the single request it owes is made at
	// its tail, below the flip.
	if err := s.publishPayloadGeneration(ctx, generationID, publishedAt, false); err != nil {
		return err
	}
	if err := s.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         checkoutID,
		Slot:               slot,
		GenerationID:       generationID,
		ExpectedRouteEpoch: expectRouteEpoch,
		State:              RouteActive,
	}); err != nil {
		return err
	}
	// The single lane request this window owes, made HERE rather than at the
	// publish tail: between the publish and the flip the generation is ready
	// but unrouted, and maintenance taking the write gate inside that window
	// would widen a documented transient state and stall a caller waiting
	// behind it. Asking after the flip means the request cannot be served
	// before the route is live.
	//
	// SCHEDULED, not run — see PublishPayloadGeneration for why sqlite_stat1
	// belongs to the file rather than to any generation, and why dropping the
	// ask (rather than moving it) would be the issue-#651 regression.
	//
	// Reach, stated so it is not over-read: PublishAndRoute has no non-test
	// caller in this tree. The daemon's physical publish is
	// PublishPayloadGeneration, which schedules for itself; the checkout
	// coordinator flips separately through FlipCheckoutRouteSlot. What this
	// call keeps is the boundary for a DIRECT caller of this API — which is
	// also the only shape in which the store can observe a route flip at all.
	s.schedulePublishMaintenance()
	return nil
}

// publishTimestamp is the published_at PublishAndRoute stamps. It reads
// SQLite's clock rather than the process clock so a generation's publish time
// is on the same timeline as every other timestamp in the database.
func (s *Store) publishTimestamp(ctx context.Context) (int64, error) {
	var now int64
	err := s.db.QueryRowContext(ctx, `SELECT unixepoch()`).Scan(&now)
	return now, err
}

// MarkPayloadGenerationSuperseded records that a newer generation has replaced
// a ready one. The payload stays until retirement collects it, because readers
// holding the generation must keep seeing what they started reading.
func (s *Store) MarkPayloadGenerationSuperseded(ctx context.Context, generationID int64) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	catalog := s.Catalog()
	if err := catalog.SetViewGenerationState(ctx, generationID, ViewGenerationSuperseded, ViewGenerationReady); err != nil {
		return err
	}
	// The owner is read after the transition, not before: a supersede that
	// lost its guard is not a supersession and must not be counted as one.
	if row, found, err := catalog.GetViewGeneration(ctx, generationID); err == nil && found {
		viewmetrics.Count(viewmetrics.GenerationSupersededTotal, generationOwner(row.OwnerKind))
	}
	return nil
}

// payloadGenerationSweepBatch bounds the rows one delete statement removes, so
// no chunk holds the mutation gate for an unbounded time.
const payloadGenerationSweepBatch = 1000

// Chunked deletes for the two core tables. The generation predicate is
// restated as a literal `view_gen > 0` alongside the bound equality because
// SQLite only uses a partial index when the query's WHERE matches the index's
// — see nodesByGenerationIndexDDL.
const (
	deleteGenerationNodesSQL = `DELETE FROM nodes WHERE view_gen = ? AND id IN (
    SELECT id FROM nodes WHERE view_gen > 0 AND view_gen = ? LIMIT ?)`
	deleteGenerationEdgesSQL = `DELETE FROM edges WHERE view_gen = ? AND id IN (
    SELECT id FROM edges WHERE view_gen > 0 AND view_gen = ? LIMIT ?)`
)

// ftsDocidMap pairs an FTS5 virtual table with the sidecar that maps a
// generation's rows to its docids. The virtual tables carry no generation
// column, so the map is the only address a sweep has for their rows.
type ftsDocidMap struct {
	fts string
	ids string
}

var generationFTSDocidMaps = []ftsDocidMap{
	{fts: "symbol_fts", ids: "symbol_fts_rowid"},
	{fts: "content_fts", ids: "content_fts_rowid"},
}

// RetirePayloadGeneration deletes a generation and everything it carries.
//
// inUse is the external reader lease hook: a graph-view lease manager passes a
// predicate reporting whether readers still hold the generation. A nil predicate
// omits that external reader check, never the Store's physical-flight check.
//
// The order is: refuse while referenced or owned, atomically fence the catalog
// row as retiring while rechecking references, then recheck reader and physical
// ownership after that transaction commits. Only then seal the generation and
// drain writers already past the gate, delete payload in bounded chunks, and
// delete the catalog row. Every delete is keyed and idempotent; a partial retire
// leaves its fence in place so the next run can safely continue.
//
// The sweep runs under a budget (payload_generation_sweep.go). A pass that
// spends it stops on a committed chunk boundary and returns
// ErrPayloadSweepBudgetExhausted with the fence still in place; the next pass
// resumes where this one stopped. The default budget is a runaway ceiling
// rather than a fair-share slice precisely so that yield is not an ordinary
// outcome — one caller (repository_cleanup.go) cannot resume a refused
// retirement, and the constant block states that dependency in full.
//
// A pass the storage layer refuses — a full volume, a failing disk — returns a
// *StorageError and records a bounded reason against the generation, readable
// through StorageFailures. Every attempt retracts the previous attempt's
// reason first, so the register states the present rather than a history.
func (s *Store) RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error {
	return s.retirePayloadGeneration(ctx, generationID, inUse, defaultPayloadSweepBudget())
}

// retirePayloadGeneration is RetirePayloadGeneration with the sweep budget
// named. Production has exactly one budget — the default above — and the
// parameter exists so the yield-and-resume behaviour can be driven at a scale
// a test can assert on, rather than by writing millions of rows.
func (s *Store) retirePayloadGeneration(
	ctx context.Context, generationID int64, inUse func(int64) bool, budget payloadSweepBudget,
) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if generationID <= baseViewGeneration {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	// The register states the outcome of the attempt that just ran, not a
	// history, so every attempt retracts the last one's reason before it can
	// produce its own. Retracting here rather than inside the sweep is what
	// makes that true of the refusals too: a generation that hit a full volume
	// and is then held by a lease is refused, not failing, and must stop
	// claiming the volume is the reason it is still there. A retraction that
	// turns out to be premature is re-derived by this same attempt.
	s.ClearStorageFailure(generationID)
	catalog := s.Catalog()
	row, found, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		return err
	} else if !found {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedMissing)
		return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
	}
	owner := generationOwner(row.OwnerKind)
	// Preserve cheap refusals and their existing metric labels. These observations
	// are not the retirement authority: the catalog rechecks references atomically.
	refs, err := catalog.ViewGenerationReferences(ctx, generationID)
	if err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return err
	}
	if refs.Any() {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, refusalReason(refs))
		return fmt.Errorf("%w: generation %d", ErrCatalogGenerationReferenced, generationID)
	}
	inUseNow := func() bool {
		return s.PayloadBuildFlightActive(generationID) || (inUse != nil && inUse(generationID))
	}
	if inUseNow() {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedLeased)
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationInUse, generationID)
	}
	if err := catalog.BeginViewGenerationRetirement(ctx, generationID); err != nil {
		reason := viewmetrics.RefusedError
		if errors.Is(err, ErrCatalogGenerationReferenced) {
			// The transaction reports a reference added after the fast check,
			// but not its kind. Keep its cause without a second diagnostic query.
			reason = viewmetrics.LabelOther
		}
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, reason)
		return err
	}
	// A reader or physical leader may have acquired ownership after the fast
	// refusal. The fence is committed and its transaction ended before this
	// decisive check. Keep it in place while owners drain; never reopen admissions.
	if inUseNow() {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedLeased)
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationInUse, generationID)
	}
	// payloadSealRetired, not payloadSealSealed: the payload write gate reads
	// them identically, but the analysis cache — whose transactions run on the
	// base handle and so never reach that gate — has to keep admitting writes
	// through a published generation while refusing them here.
	s.setPayloadSeal(generationID, payloadSealRetired)
	// A write admitted before the seal closed would otherwise commit rows into
	// a generation the sweep has already walked past.
	//
	// This arm is not classified. drainPayloadWriters takes the mutation gate
	// and releases it, so the only error it can produce is the caller's own
	// context expiring while it waits — which is never a storage failure, and
	// running it through the classifier would leave an arm no failure can ever
	// reach.
	if err := s.drainPayloadWriters(ctx); err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return err
	}

	// Past this point the generation is fenced, sealed and drained, and every
	// remaining step is idempotent. A pass that yields on its budget or is
	// refused by the storage layer therefore stops where it is and returns:
	// the catalog row stays retiring, which is the state a crash here would
	// leave and the state the next pass resumes from. Nothing between here and
	// DeleteViewGeneration makes a half-swept generation visible as anything
	// other than retiring.
	if err := s.sweepPayloadGeneration(ctx, generationID, budget.begin()); err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return s.noteRetirementFailure(generationID, err)
	}
	if err := catalog.DeleteViewGeneration(ctx, generationID); err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return s.noteRetirementFailure(generationID, err)
	}
	s.payloadSeals.Delete(generationID)
	// Generation ids are never reused, so a handle still holding the lane
	// keeps a mutex nothing new can join rather than sharing one with a later
	// generation.
	s.resolveLanes.Delete(generationID)
	viewmetrics.Count(viewmetrics.GenerationRetiredTotal, owner)
	return nil
}

// refusalReason names the holder a refused retirement lost to. A generation
// can be held by more than one pointer at once; the order here is the order
// the guard itself checks in, so the reason a reader sees is the first thing
// that would have to be released.
func refusalReason(refs ViewGenerationReferences) string {
	switch {
	case refs.Routed, refs.RefViewed, refs.GraphActive:
		return viewmetrics.RefusedRouted
	case refs.Based:
		return viewmetrics.RefusedBased
	default:
		return viewmetrics.LabelOther
	}
}

// payloadSweepTables is every generation-keyed table the sweep walks, taken
// from the two registries that declare them, so a sidecar or mask added later
// is collected without being named here as well.
func payloadSweepTables() []string {
	tables := make([]string, 0, len(viewGenSidecars)+len(generationMaskTables))
	for _, sidecar := range viewGenSidecars {
		tables = append(tables, sidecar.table)
	}
	for _, mask := range generationMaskTables {
		tables = append(tables, mask.table)
	}
	return tables
}

// payloadSweepDeleteSQL builds one table's chunked delete from its primary
// key, which the table itself reports. Every generation-keyed table's rows are
// addressed by that key, so the subselect bounds the chunk and the outer delete
// removes exactly the rows it named.
func (s *Store) payloadSweepDeleteSQL(table string) (string, error) {
	keys, err := s.primaryKeyColumns(table)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("table has no primary key")
	}
	list := strings.Join(keys, ", ")
	target := list
	if len(keys) > 1 {
		target = "(" + list + ")"
	}
	return `DELETE FROM ` + table + ` WHERE view_gen = ? AND ` + target +
		` IN (SELECT ` + list + ` FROM ` + table + ` WHERE view_gen = ? LIMIT ?)`, nil
}

// primaryKeyColumns returns a table's primary-key columns in key order.
func (s *Store) primaryKeyColumns(table string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		keys = append(keys, name)
	}
	return keys, rows.Err()
}

// payloadSweepChunk removes one bounded batch of a generation's rows inside
// the sweep's transaction and reports how many it took. Every chunk must
// shrink its own source, so a batch that removes nothing means the table is
// clear rather than that the chunk re-read rows it had already collected.
type payloadSweepChunk func(ctx context.Context, tx *sql.Tx) (int64, error)

// deleteGenerationRowsChunk runs one bounded delete statement. The generation
// binds twice: once for the outer delete and once for the bounding subselect,
// which reads the very table the delete shrinks.
func deleteGenerationRowsChunk(query string, generationID int64) payloadSweepChunk {
	return func(ctx context.Context, tx *sql.Tx) (int64, error) {
		result, err := tx.ExecContext(ctx, query, generationID, generationID, payloadGenerationSweepBatch)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
}

// deleteFTSDocidChunk removes one bounded batch of a generation's FTS
// documents together with the map rows that addressed them. The docids are
// read inside the chunk's own transaction and both deletes are bound to that
// exact set, so the map — the subselect's source — shrinks by the same rows
// the virtual table does and the next chunk sees the batch after it.
func deleteFTSDocidChunk(docidMap ftsDocidMap, generationID int64) payloadSweepChunk {
	return func(ctx context.Context, tx *sql.Tx) (int64, error) {
		docids, err := generationFTSDocids(ctx, tx, docidMap.ids, generationID)
		if err != nil || len(docids) == 0 {
			return 0, err
		}
		list := sqlInt64List(docids)
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+docidMap.fts+` WHERE rowid IN (`+list+`)`); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+docidMap.ids+` WHERE view_gen = ? AND fts_rowid IN (`+list+`)`, generationID); err != nil {
			return 0, err
		}
		return int64(len(docids)), nil
	}
}

// generationFTSDocids reads one bounded batch of a generation's FTS docids.
func generationFTSDocids(ctx context.Context, tx *sql.Tx, table string, generationID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT fts_rowid FROM `+table+` WHERE view_gen = ? LIMIT ?`,
		generationID, payloadGenerationSweepBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docids := make([]int64, 0, payloadGenerationSweepBatch)
	for rows.Next() {
		var docid int64
		if err := rows.Scan(&docid); err != nil {
			return nil, err
		}
		docids = append(docids, docid)
	}
	return docids, rows.Err()
}

// sqlInt64List renders a docid batch as a SQL value list. The values come
// straight back out of an INTEGER column, so they go in as literals rather
// than host parameters and a chunk is bounded only by its own batch size.
func sqlInt64List(values []int64) string {
	var b strings.Builder
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(value, 10))
	}
	return b.String()
}

// deletePayloadChunks runs one chunk until it removes nothing more, or until
// the pass has spent its budget. Each chunk is its own transaction under the
// mutation gate, and rechecks that the generation is still retiring — a route
// flip that adopted the generation again must stop the sweep rather than
// delete rows out from under a reader.
//
// The budget is checked before a chunk rather than after one, so a yield never
// splits a transaction and a pass with any budget left always makes at least
// one chunk of progress. That is what bounds the number of passes: every pass
// either finishes the step or removes a chunk's worth of it.
func (s *Store) deletePayloadChunks(
	ctx context.Context, generationID int64, chunk payloadSweepChunk, pass *payloadSweepPass,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pass.spent() {
			return fmt.Errorf("%w: generation %d", ErrPayloadSweepBudgetExhausted, generationID)
		}
		removed, retiring, err := s.deletePayloadChunk(ctx, generationID, chunk)
		if err != nil {
			return fmt.Errorf("payload generation gc: generation %d: %w", generationID, err)
		}
		if !retiring {
			return fmt.Errorf("payload generation gc: generation %d left the retiring state", generationID)
		}
		pass.spend(removed)
		if removed == 0 {
			return nil
		}
	}
}

func (s *Store) deletePayloadChunk(ctx context.Context, generationID int64, chunk payloadSweepChunk) (int64, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	retiring, err := payloadGenerationRetiringTx(ctx, tx, generationID)
	if err != nil || !retiring {
		return 0, retiring, err
	}
	removed, err := chunk(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	committed = true
	return removed, true, nil
}

func payloadGenerationRetiringTx(ctx context.Context, tx *sql.Tx, generationID int64) (bool, error) {
	var state string
	err := tx.QueryRowContext(ctx,
		`SELECT state FROM view_generations WHERE generation_id = ?`, generationID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ViewGenerationState(state) == ViewGenerationRetiring, nil
}
