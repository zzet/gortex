package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Folding a chain in steps.
//
// FlattenGenerationChain composes a chain in one write transaction: about
// 30 µs per row, 13 s for the ~440k rows of hundreds of dirty files, all of it
// holding the write gate an edit waits on. A ChainFold writes the same result
// into the same kind of empty building generation in steps of about
// chainFoldStepTarget of write-gate hold each:
//
//   - The composition is the same, top-down member by member (see
//     FlattenGenerationChain for the layer contract); within a member each
//     table is paged by its key, each FTS docid map by its docids.
//   - A step is one write transaction. What it commits is the progress: the
//     cursor (member, phase, last key) lives in the ChainFold and moves only
//     after the commit.
//   - A step gives way to an edit: when an ordinary write is announced or a writer is
//     parked on the gate, the step in flight is interrupted and rolled back
//     (ErrChainFoldYielded, cursor unchanged) and the caller steps again later.
//   - A step does not start while the log is over its mark
//     (ErrChainFoldWALMark): chainFoldWALMark (1 GiB), edited or not.
//   - Step never sleeps; waiting is the caller's.
//   - Readers see nothing new between steps: `to` is building, which no view
//     serves, and the members are held (retirement refuses them) until
//     Release.
//   - Progress does not survive the process: a restart leaves `to` building
//     and unowned, and the orphaned-building recovery (in every released
//     binary) retires and sweeps it. The caller starts a fresh fold.
//
// The exclusion state (what the members above hide) is kept in memory and
// in connection-local temp tables (fold_hidden_*), refilled whenever a step
// finds them missing or different: the writer connection may be replaced
// between steps, and the one-shot flatten uses its own tables.

var (
	// ErrChainFoldStale refuses a fold whose chain is not what it was: a
	// member missing, not ready, or being retired, or `to` not building.
	ErrChainFoldStale = errors.New("store_sqlite: chain fold: the chain is stale")
	// ErrChainFoldBusy refuses a second fold on the same store.
	ErrChainFoldBusy = errors.New("store_sqlite: chain fold: another fold is running")
	// ErrChainFoldYielded reports a step rolled back for an edit; step again.
	ErrChainFoldYielded = errors.New("store_sqlite: chain fold: the step gave way to an edit")
	// ErrChainFoldWALMark reports a step not started over the log's mark;
	// step again once the log is down.
	ErrChainFoldWALMark = errors.New("store_sqlite: chain fold: the log is over its mark")
)

// Vars, not consts, only so the in-package cases can change them.
var (
	// chainFoldStepTarget is the write-gate hold one step aims at.
	chainFoldStepTarget = 50 * time.Millisecond
	// chainFoldFirstRows sizes the first step, before a rate is known.
	chainFoldFirstRows = 1000
	chainFoldMinRows   = 64
	chainFoldMaxRows   = 20000
	// chainFoldYieldPoll is how often a step in flight checks for an edit.
	chainFoldYieldPoll = time.Millisecond
	// chainFoldWALMark: no step starts with more log than this since its
	// last reset, edited or not. Provisional: it stays at 1 GiB unless a
	// measurement of what a log of that size costs a read and an edit says
	// otherwise. It is the fold's own and not the pressure mark, although
	// both are 1 GiB today. (It was 256 MiB while a checkout was edited: in
	// a burst the edits' own writes kept the log over that, and a fold was
	// refused for minutes while the resets it waited for were impossible.)
	chainFoldWALMark int64 = 1 << 30
	// chainFoldInjectFailure, when a test sets it, can fail a step before
	// its n-th phase (a failed commit, a lost connection). nil in production.
	chainFoldInjectFailure func(phase int) error
)

// ChainFoldRequest names the chain (oldest first) and the empty building
// generation the fold writes; Owner identifies the caller in logs.
type ChainFoldRequest struct {
	Chain []int64
	To    int64
	Owner string
	// StepTarget may request smaller transactions for a fold sharing the writer
	// with frequent interactive edits. Zero retains the default target.
	StepTarget time.Duration
	// BuildFlight optionally borrows the caller's live destination leader.
	// The caller retains it across Begin retries and through publication/landing;
	// neither a refused Begin nor Release completes borrowed ownership.
	BuildFlight *PayloadBuildFlight
}

// chainFoldPhase is where in a member the fold is.
type chainFoldPhase int

const (
	foldPhasePrepare chainFoldPhase = iota // masks, exclusion, scratch
	foldPhaseRows                          // tables[table], paged by key
	foldPhaseFTS                           // docid maps[table], paged by docid
	foldPhaseMasks                         // masks, then the member is done
	foldPhaseSettle                        // after the last member
	foldPhaseDone
)

// ChainFold is one fold in progress. Its methods are not safe for concurrent
// use; one caller drives it.
type ChainFold struct {
	s              *Store
	chain          []int64
	to             int64
	owner          string
	flight         *PayloadBuildFlight
	borrowedFlight bool
	shapes         []payloadTableShape
	keys           [][]string // per shape: the page key columns

	member int // index into chain, from the top down
	phase  chainFoldPhase
	table  int
	after  []any   // last key of the current rows phase
	docids []int64 // the current FTS phase's docids, read at its start
	docPos int

	hiddenPaths, hiddenIDs, hiddenSources map[string]struct{}
	speakers                              []flattenSpeaker
	masks                                 generationMaskSet
	memberIDs                             map[string]struct{}
	exclude                               *flattenExclusion

	usPerRow   float64
	counts     GenerationCopyCounts
	steps      int
	yields     int
	released   bool
	stepTarget time.Duration
	// interruptedPageRows bounds only retries of a page rolled back for a
	// queued writer. A committed step returns to measured time-based sizing.
	interruptedPageRows int
	interruptedPages    int
	pageBackoffs        int
}

// chainFoldHeld is the store's one fold and the members it holds.
type chainFoldHeld struct {
	active  atomic.Bool
	mu      sync.Mutex
	members map[int64]struct{}
}

func (s *Store) chainFoldHolds(generationID int64) bool {
	if s.coreless() || !s.chainFold.active.Load() {
		return false
	}
	s.chainFold.mu.Lock()
	defer s.chainFold.mu.Unlock()
	_, ok := s.chainFold.members[generationID]
	return ok
}

// BeginChainFold validates the chain, takes the store's fold lease, holds the
// members against retirement and becomes the writer of `to` (a payload build
// flight, so the orphaned-building recovery leaves it alone).
func (s *Store) BeginChainFold(ctx context.Context, req ChainFoldRequest) (*ChainFold, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if s.coreless() {
		return nil, fmt.Errorf("%w: a chain fold needs an open store", ErrCatalogInvalidValue)
	}
	if len(req.Chain) == 0 {
		return nil, fmt.Errorf("%w: an empty chain has nothing to fold", ErrCatalogInvalidValue)
	}
	for _, id := range req.Chain {
		if id <= baseViewGeneration || id == req.To {
			return nil, fmt.Errorf("%w: chain member %d cannot be folded into %d", ErrCatalogInvalidValue, id, req.To)
		}
	}
	if req.To <= baseViewGeneration {
		return nil, fmt.Errorf("%w: a fold may not write the base corpus", ErrCatalogInvalidValue)
	}
	if !s.chainFold.active.CompareAndSwap(false, true) {
		return nil, ErrChainFoldBusy
	}
	fail := func(err error) (*ChainFold, error) {
		s.chainFold.mu.Lock()
		s.chainFold.members = nil
		s.chainFold.mu.Unlock()
		s.chainFold.active.Store(false)
		return nil, err
	}
	// Hold the members before checking them, so a retirement that begins
	// after the check sees them held (RetirePayloadGeneration rechecks after
	// its fence).
	s.chainFold.mu.Lock()
	s.chainFold.members = make(map[int64]struct{}, len(req.Chain))
	for _, id := range req.Chain {
		s.chainFold.members[id] = struct{}{}
	}
	s.chainFold.mu.Unlock()
	catalog := s.Catalog()
	for _, id := range req.Chain {
		row, found, err := catalog.GetViewGeneration(ctx, id)
		if err != nil {
			return fail(err)
		}
		if !found || (row.State != ViewGenerationReady && row.State != ViewGenerationSuperseded) {
			return fail(fmt.Errorf("%w: member %d is not ready", ErrChainFoldStale, id))
		}
	}
	flight := req.BuildFlight
	borrowed := flight != nil
	if borrowed {
		if err := s.validatePayloadBuildFlight(flight, req.To); err != nil {
			return fail(err)
		}
	} else {
		var leader bool
		var err error
		flight, leader, _, err = s.JoinPayloadBuildFlight(ctx, req.To, false)
		if err != nil {
			return fail(fmt.Errorf("%w: %v", ErrChainFoldStale, err))
		}
		if !leader {
			return fail(fmt.Errorf("%w: generation %d has another writer", ErrChainFoldBusy, req.To))
		}
	}
	complete := func(err error) {
		if !borrowed {
			flight.Complete(err)
		}
	}
	f := &ChainFold{
		s: s, chain: append([]int64(nil), req.Chain...), to: req.To, owner: req.Owner, flight: flight, borrowedFlight: borrowed, stepTarget: req.StepTarget,
		member: len(req.Chain) - 1, phase: foldPhasePrepare,
		hiddenPaths: map[string]struct{}{}, hiddenIDs: map[string]struct{}{}, hiddenSources: map[string]struct{}{},
	}

	// Prepare the immutable table shapes and key layout without holding the
	// writer. A slow catalog query must not repeatedly lose its entire work to
	// foreground writes before the first fold step can begin.
	metadata, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		complete(err)
		return fail(err)
	}
	prepare := func() error {
		defer metadata.Rollback() //nolint:errcheck // read-only metadata snapshot; explicit close below
		if chainFoldMetadataHook != nil {
			if err := chainFoldMetadataHook(ctx, false); err != nil {
				return err
			}
		}
		var schema int
		if err := metadata.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schema); err != nil {
			return err
		}
		shapes, err := payloadRowTables(ctx, metadata)
		if err != nil {
			return err
		}
		f.shapes = shapes
		for _, shape := range shapes {
			keys, err := chainFoldKeys(ctx, metadata, shape.table)
			if err != nil {
				return err
			}
			f.keys = append(f.keys, keys)
		}
		if err := metadata.Rollback(); err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			return err
		}
		if chainFoldMetadataHook != nil {
			if err := chainFoldMetadataHook(ctx, true); err != nil {
				return err
			}
		}
		// Recheck the shape snapshot and destination in the same transaction that
		// protects empty admission. Schema changes retry within the existing bounds.
		return s.withFoldTx(ctx, req.To, func(ctx context.Context, tx *sql.Tx) error {
			var current int
			if err := tx.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&current); err != nil {
				return err
			}
			if current != schema {
				return fmt.Errorf("%w: payload schema changed during preparation", ErrChainFoldYielded)
			}
			empty, err := generationPayloadEmptyTx(ctx, tx, req.To)
			if err != nil {
				return err
			}
			if !empty {
				return fmt.Errorf("%w: generation %d", ErrGenerationBulkLoadPopulated, req.To)
			}
			return nil
		})
	}
	if err := prepare(); err != nil {
		complete(err)
		return fail(err)
	}
	return f, nil
}

// chainFoldKeys is the column list a table's rows are paged by within one
// generation: its primary key without the generation column, or the table's
// id where the key is the row's physical id.
func chainFoldKeys(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	switch table {
	case "nodes", "edges":
		return []string{"id"}, nil // nodes_by_generation / edges_by_generation
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		pk = append(pk, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var keys []string
	for _, column := range pk {
		if column != viewGenColumnName {
			keys = append(keys, column)
		}
	}
	if len(keys) == 0 {
		return []string{"rowid"}, nil
	}
	return keys, nil
}

// foldWriteWanted keeps actual writer admission and ordinary mutation priority.
// A checkout ticket alone must not prevent its own fold from making progress.
func (s *Store) foldWriteWanted() bool {
	return s.writeMu.waiting() > 0 || s.foldWriteIntents.Load() > 0
}

// withFoldTx runs fn in one write transaction of the destination handle,
// under the write gate. An ordinary write announced or parked on the gate meanwhile
// interrupts the transaction (ErrChainFoldYielded) and rolls it back.
func (s *Store) withFoldTx(ctx context.Context, to int64, fn func(ctx context.Context, tx *sql.Tx) error) error {
	destination, err := s.AtManagedGeneration(to)
	if err != nil {
		return err
	}
	if err := destination.refuseSealedPayloadWrite(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.foldWriteWanted() {
		return ErrChainFoldYielded
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.foldWriteWanted() {
		return ErrChainFoldYielded
	}
	if err := destination.refuseSealedPayloadWrite(); err != nil {
		return err
	}
	stepCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(chainFoldYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-stepCtx.Done():
				return
			case <-ticker.C:
				if s.foldWriteWanted() {
					cancel(ErrChainFoldYielded)
					return
				}
			}
		}
	}()
	defer close(watchDone)
	// Own the transaction's lifetime under writeMu. database/sql's automatic
	// rollback marks a canceled Tx done before SQLite has rolled it back, so a
	// second Rollback may return ErrTxDone and release the gate too early. A bulk
	// window pins the connection: the next writer could then begin inside this
	// fold's transaction. Keep cancellation on admission and every statement,
	// and finish rollback synchronously before handing the writer to anyone else.
	conn, release, err := s.activeWriteConnLocked(stepCtx)
	if err != nil {
		return foldCause(stepCtx, err)
	}
	defer release()
	tx, err := destination.beginWriteOnWithLifetimeContext(stepCtx, context.WithoutCancel(stepCtx), conn)
	if err != nil {
		return foldCause(stepCtx, err)
	}
	defer func() { _ = tx.Rollback() }()
	// The statements run on the step's own context: cancelling it
	// interrupts the one in flight (a rollback alone would wait for it).
	if err := fn(stepCtx, tx); err != nil {
		return foldCause(stepCtx, err)
	}
	if err := stepCtx.Err(); err != nil {
		return foldCause(stepCtx, err)
	}
	if err := tx.Commit(); err != nil {
		return foldCause(stepCtx, err)
	}
	destination.constantInputCounter(destination.viewGen).Add(1)
	return nil
}

func foldCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrChainFoldYielded) {
		return ErrChainFoldYielded
	}
	return err
}

// walOverFoldMark reports whether a step must not start now.
func (s *Store) walOverFoldMark(now time.Time) bool {
	mark := s.WALWriteMark()
	if !mark.Valid {
		return false
	}
	_ = now
	return int64(mark.MxFrame)*(int64(mark.PageSize)+walFrameHeaderBytes) > chainFoldWALMark
}

func (f *ChainFold) targetDuration() time.Duration {
	if f.stepTarget > 0 && f.stepTarget < chainFoldStepTarget {
		return f.stepTarget
	}
	return chainFoldStepTarget
}

// stepRows is the row budget of the next step.
func (f *ChainFold) stepRows() int {
	rows := chainFoldFirstRows
	if f.usPerRow <= 0 {
		if f.interruptedPageRows > 0 {
			return min(rows, f.interruptedPageRows)
		}
		return rows
	}
	rows = min(max(int(float64(f.targetDuration().Microseconds())/f.usPerRow), chainFoldMinRows), chainFoldMaxRows)
	if f.interruptedPageRows > 0 {
		rows = min(rows, f.interruptedPageRows)
	}
	return rows
}

// Step runs one step: at most one write transaction of about
// chainFoldStepTarget. done reports that `to` holds the whole fold (it stays
// building; the caller verifies and publishes). ErrChainFoldYielded and
// ErrChainFoldWALMark leave the fold where it was: step again later.
func (f *ChainFold) Step(ctx context.Context) (done bool, err error) {
	if f.released {
		return false, fmt.Errorf("%w: the fold was released", ErrCatalogInvalidValue)
	}
	if f.phase == foldPhaseDone {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s := f.s
	if s.walOverFoldMark(time.Now()) {
		s.RequestWALReclaim()
		return false, ErrChainFoldWALMark
	}
	if s.foldWriteWanted() {
		f.yields++
		return false, ErrChainFoldYielded
	}
	budget := f.stepRows()
	started := time.Now()
	var moved int64
	var deferred func() // mask progress and member exclusion: committed only
	snapshot := f.cursor()
	transactionStarted := false
	err = s.withFoldTx(ctx, f.to, func(ctx context.Context, tx *sql.Tx) error {
		transactionStarted = true
		// Phases run back to back in the one transaction until the row
		// budget or the target hold is spent: an empty table costs a
		// statement, not a step. Each mask operation/page closes the step,
		// even when empty; earlier committed mask work is not replayed.
		for phases := 0; ; phases++ {
			if inject := chainFoldInjectFailure; inject != nil {
				if err := inject(phases); err != nil {
					return err
				}
			}
			ending := f.phase == foldPhaseMasks
			n, next, err := f.stepTx(ctx, tx, max(budget-int(moved), chainFoldMinRows))
			if err != nil {
				return err
			}
			moved += n
			if ending {
				deferred = next
				return nil
			}
			next()
			if f.phase == foldPhaseDone || int(moved) >= budget || time.Since(started) >= f.targetDuration() {
				return nil
			}
		}
	})
	if err != nil {
		f.restore(snapshot)
		if errors.Is(err, ErrChainFoldYielded) {
			f.yields++
			if transactionStarted {
				// Replaying the same oversized page requires the same quiet
				// window. Shorten only an interrupted transaction's retry;
				// refusal before admission gives no sizing evidence.
				f.interruptedPageRows = max(budget/2, chainFoldMinRows)
				f.interruptedPages++
				if f.interruptedPageRows < budget {
					f.pageBackoffs++
				}
			}
		}
		return false, err
	}
	f.interruptedPageRows = 0
	if deferred != nil {
		deferred()
	}
	f.steps++
	if moved >= int64(chainFoldMinRows) {
		us := float64(time.Since(started).Microseconds()) / float64(moved)
		if f.usPerRow > 0 {
			us = (us + f.usPerRow) / 2
		}
		f.usPerRow = us
	}
	return f.phase == foldPhaseDone, nil
}

// foldCursor is the part of a fold a rolled-back step must put back.
type foldCursor struct {
	member  int
	phase   chainFoldPhase
	table   int
	after   []any
	docids  []int64
	docPos  int
	masks   generationMaskSet
	exclude *flattenExclusion
	counts  GenerationCopyCounts
}

func (f *ChainFold) cursor() foldCursor {
	return foldCursor{f.member, f.phase, f.table, f.after, f.docids, f.docPos, f.masks, f.exclude, f.counts}
}

func (f *ChainFold) restore(c foldCursor) {
	f.member, f.phase, f.table, f.after, f.docids, f.docPos, f.masks, f.exclude, f.counts =
		c.member, c.phase, c.table, c.after, c.docids, c.docPos, c.masks, c.exclude, c.counts
}

// stepTx does one phase's work inside tx and returns the cursor move for it.
// Every move but a member's end (foldPhaseMasks) touches only the cursor, so
// Step applies it at once and puts the cursor back if the transaction fails.
func (f *ChainFold) stepTx(ctx context.Context, tx *sql.Tx, budget int) (int64, func(), error) {
	if f.phase == foldPhaseSettle {
		// An identity replacement whose row a member above hid replaces
		// nothing any more (FlattenGenerationChain's last statement).
		if _, err := tx.ExecContext(ctx, `
DELETE FROM generation_node_tombstones
 WHERE view_gen = ?1 AND claim_kind = 'identity_replace'
   AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.view_gen = ?1 AND n.id = node_id)`, f.to); err != nil {
			return 0, nil, fmt.Errorf("store_sqlite: settle folded identity masks: %w", err)
		}
		return 0, func() { f.phase = foldPhaseDone }, nil
	}
	member := f.chain[f.member]
	switch f.phase {
	case foldPhasePrepare:
		masks, err := readGenerationMasksTx(ctx, tx, member)
		if err != nil {
			return 0, nil, err
		}
		endpoints, err := flattenHiddenEndpointsTx(ctx, tx, member, f.speakers)
		if err != nil {
			return 0, nil, err
		}
		exclude := &flattenExclusion{
			paths: f.hiddenPaths, context: masks.context, ids: f.hiddenIDs,
			sources: f.hiddenSources, endpoints: endpoints, scratch: "fold",
		}
		if err := flattenScratch(ctx, tx, exclude); err != nil {
			return 0, nil, err
		}
		return 0, func() {
			f.masks, f.exclude = masks, exclude
			f.phase, f.table, f.after = foldPhaseRows, 0, nil
		}, nil
	case foldPhaseRows:
		if err := f.ensureScratch(ctx, tx); err != nil {
			return 0, nil, err
		}
		shape := f.shapes[f.table]
		moved, last, finished, err := insertGenerationRowsPageTx(ctx, tx, shape, f.keys[f.table], member, f.to, f.exclude, f.after, budget)
		if err != nil {
			return 0, nil, fmt.Errorf("store_sqlite: fold %s of generation %d: %w", shape.table, member, err)
		}
		return moved, func() {
			f.counts.Rows += moved
			switch shape.table {
			case "nodes":
				f.counts.Nodes += moved
			case "edges":
				f.counts.Edges += moved
			}
			if finished {
				f.table++
				f.after = nil
				if f.table >= len(f.shapes) {
					f.phase, f.table, f.docids, f.docPos = foldPhaseFTS, 0, nil, 0
				}
				return
			}
			f.after = last
		}, nil
	case foldPhaseFTS:
		if err := f.ensureScratch(ctx, tx); err != nil {
			return 0, nil, err
		}
		docidMap := generationFTSDocidMaps[f.table]
		docids := f.docids
		if docids == nil {
			filter, args := flattenFTSFilter(docidMap, member, f.to, f.exclude)
			read, err := readDocidsTx(ctx, tx,
				`SELECT fts_rowid FROM `+docidMap.ids+` WHERE `+viewGenColumnName+` = ?`+filter+` ORDER BY fts_rowid`,
				append([]any{member}, args...)...)
			if err != nil {
				return 0, nil, err
			}
			docids = read
		}
		end := min(f.docPos+budget, len(docids))
		moved, err := copyFTSDocidsTx(ctx, tx, docidMap, f.to, docids[f.docPos:end])
		if err != nil {
			return 0, nil, err
		}
		return moved, func() {
			f.counts.Rows += moved
			f.docids, f.docPos = docids, end
			if f.docPos >= len(f.docids) {
				f.table++
				f.docids, f.docPos = nil, 0
				if f.table >= len(generationFTSDocidMaps) {
					f.phase, f.table = foldPhaseMasks, 0
				}
			}
		}, nil
	case foldPhaseMasks:
		if f.table < len(foldMaskOperations) {
			op := foldMaskOperations[f.table]
			last, finished, err := foldMaskPageTx(ctx, tx, op, member, f.to, f.after, budget)
			if err != nil {
				return 0, nil, err
			}
			if hook := chainFoldMaskStepHook; hook != nil {
				if err := hook(ctx, op.table); err != nil {
					return 0, nil, err
				}
			}
			return 0, func() {
				f.after = last
				if finished {
					f.table++
					f.after = nil
				}
			}, nil
		}
		ids, last, finished, err := foldMemberIDsPageTx(ctx, tx, member, f.after, f.masks.context, budget)
		if err != nil {
			return 0, nil, err
		}
		if hook := chainFoldMaskStepHook; hook != nil {
			if err := hook(ctx, "member_ids"); err != nil {
				return 0, nil, err
			}
		}
		return 0, func() {
			if f.memberIDs == nil {
				f.memberIDs = map[string]struct{}{}
			}
			for id := range ids {
				f.memberIDs[id] = struct{}{}
			}
			f.after = last
			if !finished {
				return
			}
			for p := range f.masks.covered {
				f.hiddenPaths[p] = struct{}{}
			}
			for id := range f.masks.identity {
				f.hiddenIDs[id] = struct{}{}
			}
			for id := range f.memberIDs {
				f.hiddenIDs[id] = struct{}{}
			}
			f.speakers = append(f.speakers, flattenSpeaker{covered: f.masks.covered, identity: f.masks.identity, carried: f.memberIDs})
			for id := range f.masks.sources {
				if !idInPaths(id, f.masks.covered) {
					f.hiddenSources[id] = struct{}{}
				}
			}
			for id, kind := range f.masks.identity {
				if kind == string(NodeIdentityMaskLegacy) && !idInPaths(id, f.masks.covered) {
					f.hiddenSources[id] = struct{}{}
				}
			}
			f.exclude, f.memberIDs = nil, nil
			f.member--
			f.phase, f.table, f.after = foldPhasePrepare, 0, nil
			if f.member < 0 {
				f.phase = foldPhaseSettle
			}
		}, nil
	}
	return 0, nil, fmt.Errorf("store_sqlite: chain fold in an unknown phase %d", f.phase)
}

// ensureScratch refills the fold's temp tables when a step finds them missing
// or holding other than the current member's exclusion (a replaced writer
// connection).
func (f *ChainFold) ensureScratch(ctx context.Context, tx *sql.Tx) error {
	var paths, ids, sources, endpoints int
	err := tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM temp.fold_hidden_paths), (SELECT count(*) FROM temp.fold_hidden_ids),
 (SELECT count(*) FROM temp.fold_hidden_sources), (SELECT count(*) FROM temp.fold_hidden_endpoints)`).Scan(&paths, &ids, &sources, &endpoints)
	if err == nil && paths == len(unionKeys(f.exclude.paths, f.exclude.context)) && ids == len(f.exclude.ids) &&
		sources == len(f.exclude.sources) && endpoints == len(f.exclude.endpoints) {
		return nil
	}
	return flattenScratch(ctx, tx, f.exclude)
}

func unionKeys(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// Counts reports what the fold wrote so far, and its steps and yields.
func (f *ChainFold) Counts() (counts GenerationCopyCounts, steps, yields int) {
	return f.counts, f.steps, f.yields
}

// InterruptionCounts distinguishes rolled-back transactions from admission
// refusals, and reports how many of those interruptions shortened the retry.
func (f *ChainFold) InterruptionCounts() (transactions, budgetReductions int) {
	return f.interruptedPages, f.pageBackoffs
}

// Release ends the fold: the members are no longer held and its own writer
// completes (the caller publishes it first, or abandons it). A borrowed writer
// remains the caller's until the result is installed or abandoned.
func (f *ChainFold) Release(ctx context.Context) error {
	if f.released {
		return nil
	}
	f.released = true
	if !f.borrowedFlight {
		f.flight.Complete(nil)
	}
	s := f.s
	s.chainFold.mu.Lock()
	s.chainFold.members = nil
	s.chainFold.mu.Unlock()
	s.chainFold.active.Store(false)
	return nil
}

// Abandon gives the fold up: `to` is marked failed, so the sweep deletes what
// the steps wrote, and the fold is released.
func (f *ChainFold) Abandon(ctx context.Context) error {
	if f.released {
		return nil
	}
	err := f.s.Catalog().SetViewGenerationState(ctx, f.to, ViewGenerationFailed, ViewGenerationBuilding)
	if relErr := f.Release(ctx); err == nil {
		err = relErr
	}
	return err
}

// insertGenerationRowsPageTx is insertGenerationRowsTx over one page of the
// member's rows: those after `after` in key order, up to `limit` of them. It
// returns the page's last key and whether the member's rows are done.
func insertGenerationRowsPageTx(
	ctx context.Context, tx *sql.Tx, shape payloadTableShape, keys []string, from, to int64,
	exclude *flattenExclusion, after []any, limit int,
) (int64, []any, bool, error) {
	keyList := "t." + strings.Join(keys, ", t.")
	tuple := "(" + keyList + ")"
	marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(keys)), ", ") + ")"
	where := "t." + viewGenColumnName + " = ?"
	args := []any{from}
	if after != nil {
		where += " AND " + tuple + " > " + marks
		args = append(args, after...)
	}
	// The page's upper key: the limit-th row after `after` (none: the rest).
	upper := make([]any, len(keys))
	targets := make([]any, len(keys))
	for i := range upper {
		targets[i] = &upper[i]
	}
	err := tx.QueryRowContext(ctx, `SELECT `+keyList+` FROM `+shape.table+` t WHERE `+where+
		` ORDER BY `+keyList+` LIMIT 1 OFFSET ?`, append(args, limit-1)...).Scan(targets...)
	finished := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		finished, upper = true, nil
	case err != nil:
		return 0, nil, false, err
	}
	extra := []string{}
	extraArgs := []any{}
	if after != nil {
		extra = append(extra, tuple+" > "+marks)
		extraArgs = append(extraArgs, after...)
	}
	if upper != nil {
		extra = append(extra, tuple+" <= "+marks)
		extraArgs = append(extraArgs, upper...)
	}
	moved, err := insertGenerationRowsBoundedTx(ctx, tx, shape, from, to, exclude, true, extra, extraArgs)
	if err != nil {
		return 0, nil, false, err
	}
	return moved, upper, finished, nil
}

// flattenFTSFilter is flattenFTSTx's per-map predicate over the source map.
func flattenFTSFilter(docidMap ftsDocidMap, member, to int64, exclude *flattenExclusion) (string, []any) {
	scratch := exclude.scratchName()
	switch docidMap.ids {
	case "symbol_fts_rowid":
		return ` AND node_id NOT IN (SELECT id FROM ` + scratch + `_hidden_ids)
 AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.view_gen = ? AND n.id = node_id
   AND n.file_path IN (SELECT p FROM ` + scratch + `_hidden_paths))
 AND NOT EXISTS (SELECT 1 FROM symbol_fts_rowid d WHERE d.view_gen = ? AND d.node_id = symbol_fts_rowid.node_id)`, []any{member, to}
	case "content_fts_rowid":
		return ` AND file_path NOT IN (SELECT p FROM ` + scratch + `_hidden_paths)`, nil
	}
	return "", nil
}

// copyFTSDocidsTx copies the listed documents of one docid map into `to`
// under freshly minted docids.
func copyFTSDocidsTx(ctx context.Context, tx *sql.Tx, docidMap ftsDocidMap, to int64, docids []int64) (int64, error) {
	if len(docids) == 0 {
		return 0, nil
	}
	mapColumns, err := generationCopyColumns(ctx, tx, docidMap.ids)
	if err != nil {
		return 0, err
	}
	carried := make([]string, 0, len(mapColumns))
	for _, column := range mapColumns {
		if column == viewGenColumnName || column == "fts_rowid" {
			continue
		}
		carried = append(carried, column)
	}
	ftsColumns, err := generationCopyColumns(ctx, tx, docidMap.fts)
	if err != nil {
		return 0, err
	}
	next, err := nextFTSRowIDTx(tx, docidMap.fts)
	if err != nil {
		return 0, err
	}
	var moved int64
	for start := 0; start < len(docids); start += generationCopyFTSChunk {
		end := min(start+generationCopyFTSChunk, len(docids))
		chunk, err := generationCopyFTSChunkRows(ctx, tx, docidMap, carried, ftsColumns, docids[start:end])
		if err != nil {
			return moved, err
		}
		if len(chunk) == 0 {
			continue
		}
		written, err := generationCopyWriteFTSChunk(ctx, tx, docidMap, carried, ftsColumns, chunk, to, &next)
		if err != nil {
			return moved, err
		}
		moved += written
	}
	return moved, nil
}

// chainFoldMetadataHook is a serialized test seam: false before metadata reads,
// true after the read transaction closes and before writer admission.
// Production leaves it nil.
var chainFoldMetadataHook func(context.Context, bool) error
