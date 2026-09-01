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
)

// payloadSeal is the write-admission flag for one derived payload generation.
// Every handle over that generation shares the one instance held in the core's
// payloadSeals map, so publishing flips them all at once.
//
// The flag is resolved lazily: a fresh handle starts unknown and pays a single
// catalog point read on its first write, after which the write gate costs one
// atomic load. A generation with no catalog row at all is open — such rows are
// written by callers that manage generations themselves, and the lifecycle
// here does not claim authority over them.
type payloadSeal struct {
	state atomic.Int32
}

const (
	payloadSealUnknown int32 = iota
	payloadSealOpen
	payloadSealSealed
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
	case payloadSealSealed:
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationSealed, s.viewGen)
	}
	return s.resolvePayloadSeal(seal)
}

// resolvePayloadSeal reads the generation's catalog state once and caches the
// verdict. It runs before any write transaction is opened, so it never queries
// through a connection it is itself holding.
func (s *Store) resolvePayloadSeal(seal *payloadSeal) error {
	var state string
	err := s.db.QueryRow(
		`SELECT state FROM view_generations WHERE generation_id = ?`, s.viewGen).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.openPayloadSeal(seal)
	case err != nil:
		return err
	case ViewGenerationState(state) == ViewGenerationBuilding:
		return s.openPayloadSeal(seal)
	}
	seal.state.CompareAndSwap(payloadSealUnknown, payloadSealSealed)
	return fmt.Errorf("%w: generation %d is %s", ErrPayloadGenerationSealed, s.viewGen, state)
}

// openPayloadSeal caches the open verdict the catalog read produced. A publish
// or a retire may have sealed the generation while that read was in flight, in
// which case the flag they stored is the newer verdict and the write is
// refused on it rather than on the state the read saw.
func (s *Store) openPayloadSeal(seal *payloadSeal) error {
	if seal.state.CompareAndSwap(payloadSealUnknown, payloadSealOpen) {
		return nil
	}
	if seal.state.Load() == payloadSealSealed {
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationSealed, s.viewGen)
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
	if s == nil || s.storeCore == nil {
		return 0, nil, false, fmt.Errorf("%w: payload generation needs an open store", ErrCatalogInvalidValue)
	}
	s.payloadLifecycleMu.Lock()
	defer s.payloadLifecycleMu.Unlock()
	return s.beginPayloadGenerationWithStatus(ctx, req)
}

// beginPayloadGenerationWithStatus runs with payloadLifecycleMu held. The
// lifecycle mutex precedes the catalog write gate acquired by adoption.
func (s *Store) beginPayloadGenerationWithStatus(
	ctx context.Context,
	req PayloadGenerationRequest,
) (generationID int64, handle *Store, adopted bool, err error) {
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
		State:                ViewGenerationBuilding,
		CreatedAt:            req.CreatedAt,
	})
	if err != nil {
		return 0, nil, false, err
	}
	s.setPayloadSeal(generationID, payloadSealOpen)
	return generationID, s.AtGeneration(generationID), adopted, nil
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
func (s *Store) PublishPayloadGeneration(ctx context.Context, generationID, publishedAt int64) error {
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

	s.setPayloadSeal(generationID, payloadSealSealed)
	if err := s.drainPayloadWriters(ctx); err != nil {
		s.setPayloadSeal(generationID, payloadSealUnknown)
		return err
	}
	if err := s.publishSealedGeneration(ctx, catalog, generationID, publishedAt); err != nil {
		s.setPayloadSeal(generationID, payloadSealUnknown)
		return err
	}
	viewmetrics.Count(viewmetrics.GenerationPublishedTotal, generationOwner(row.OwnerKind))
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
	if err := s.PublishPayloadGeneration(ctx, generationID, publishedAt); err != nil {
		return err
	}
	return s.Catalog().FlipCheckoutRouteSlot(ctx, FlipCheckoutRouteSlotRequest{
		CheckoutID:         checkoutID,
		Slot:               slot,
		GenerationID:       generationID,
		ExpectedRouteEpoch: expectRouteEpoch,
		State:              RouteActive,
	})
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
// inUse is the lease hook: a graph-view lease manager passes a predicate that
// reports whether any reader still holds the generation, and retirement is
// refused while it does. A nil predicate means nothing leases generations.
//
// The order is: refuse while referenced or leased, mark the catalog row
// retiring, seal the generation and drain the writers already past the gate,
// delete the payload in bounded chunks, then delete the catalog row. Every
// delete is keyed by generation and idempotent, so a retire killed part way
// leaves a retiring row whose next run simply continues.
func (s *Store) RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error {
	return s.retirePayloadGeneration(ctx, generationID, inUse, nil)
}

func (s *Store) retirePayloadGeneration(
	ctx context.Context,
	generationID int64,
	inUse func(int64) bool,
	beforeRetirementClaim func(),
) error {
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
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedMissing)
		return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
	}
	owner := generationOwner(row.OwnerKind)
	refs, err := catalog.ViewGenerationReferences(ctx, generationID)
	if err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return err
	}
	if refs.Any() {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, refusalReason(refs))
		return fmt.Errorf("%w: generation %d", ErrCatalogGenerationReferenced, generationID)
	}
	claimErr := func() error {
		// Lock order is payloadLifecycleMu then the catalog write gate acquired
		// by beginPayloadGenerationRetirement. A recovery join uses the same
		// lifecycle mutex, so exactly one side can cross its durable boundary.
		s.payloadLifecycleMu.Lock()
		defer s.payloadLifecycleMu.Unlock()
		if s.payloadBuildFlightActiveLocked(generationID) {
			return fmt.Errorf("%w: generation %d", ErrPayloadGenerationInUse, generationID)
		}
		if inUse != nil && inUse(generationID) {
			return fmt.Errorf("%w: generation %d", ErrPayloadGenerationInUse, generationID)
		}
		if beforeRetirementClaim != nil {
			beforeRetirementClaim()
		}
		return catalog.beginPayloadGenerationRetirement(ctx, generationID)
	}()
	if claimErr != nil {
		if errors.Is(claimErr, ErrPayloadGenerationInUse) {
			viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedLeased)
		} else {
			viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		}
		return claimErr
	}
	s.setPayloadSeal(generationID, payloadSealSealed)
	// A write admitted before the seal closed would otherwise commit rows into
	// a generation the sweep has already walked past.
	if err := s.drainPayloadWriters(ctx); err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return err
	}

	if err := s.sweepPayloadGeneration(ctx, generationID); err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return err
	}
	if err := catalog.DeleteViewGeneration(ctx, generationID); err != nil {
		viewmetrics.Count(viewmetrics.GenerationRetireRefusedTotal, viewmetrics.RefusedError)
		return err
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
	case refs.Routed, refs.CheckoutCached, refs.RefViewed, refs.GraphActive:
		return viewmetrics.RefusedRouted
	case refs.Based:
		return viewmetrics.RefusedBased
	default:
		return viewmetrics.LabelOther
	}
}

// sweepPayloadGeneration deletes every payload row a generation owns, in
// bounded chunks. The FTS documents go first, each chunk taking the map rows
// that addressed it along: a chunk that left them standing would strand the
// documents past it, because the map is the only handle on an FTS row. The
// registry sweep visits those two maps again and finds them empty, which is
// what keeps it derived from the registry rather than from a hand-kept list.
// The core tables go last, so a sweep interrupted half way never leaves a
// sidecar row pointing at a node that is already gone.
func (s *Store) sweepPayloadGeneration(ctx context.Context, generationID int64) error {
	base := s.atBase()
	for _, docidMap := range generationFTSDocidMaps {
		if err := base.deletePayloadChunks(ctx, generationID, deleteFTSDocidChunk(docidMap, generationID)); err != nil {
			return err
		}
	}
	for _, table := range payloadSweepTables() {
		query, err := base.payloadSweepDeleteSQL(table)
		if err != nil {
			return fmt.Errorf("payload generation gc: %s: %w", table, err)
		}
		if err := base.deletePayloadChunks(ctx, generationID, deleteGenerationRowsChunk(query, generationID)); err != nil {
			return err
		}
	}
	for _, query := range []string{deleteGenerationEdgesSQL, deleteGenerationNodesSQL} {
		if err := base.deletePayloadChunks(ctx, generationID, deleteGenerationRowsChunk(query, generationID)); err != nil {
			return err
		}
	}
	return nil
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

// deletePayloadChunks runs one chunk until it removes nothing more. Each chunk
// is its own transaction under the mutation gate, and rechecks that the
// generation is still retiring — a route flip that adopted the generation
// again must stop the sweep rather than delete rows out from under a reader.
func (s *Store) deletePayloadChunks(ctx context.Context, generationID int64, chunk payloadSweepChunk) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		removed, retiring, err := s.deletePayloadChunk(ctx, generationID, chunk)
		if err != nil {
			return fmt.Errorf("payload generation gc: generation %d: %w", generationID, err)
		}
		if !retiring {
			return fmt.Errorf("payload generation gc: generation %d left the retiring state", generationID)
		}
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
