package store_sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The retirement sweep under a budget and under a full volume.
//
// Both cases are the same question asked twice: a sweep that cannot finish
// right now must stop in a state that is indistinguishable from a crash at the
// same point — the catalog row still retiring, the seal still closed, the rows
// it did remove gone and the rows it did not still addressable by the same
// statement. What must never happen is a generation that is half retired and
// says otherwise.

// retiringState reports one generation's catalog row, failing the test if the
// row cannot be read.
func generationState(t *testing.T, store *Store, generationID int64) (ViewGenerationState, bool) {
	t.Helper()
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration(%d): %v", generationID, err)
	}
	if !found {
		return "", false
	}
	return row.State, true
}

// TestASweepYieldsOnItsBudgetAndFinishesOnResume pins the sweep's budget.
//
// The sweep used to drain every table until a chunk removed nothing, with no
// total row, chunk or elapsed bound: one retirement held the janitor for as
// long as the generation was large. A budget only helps if yielding is safe
// and resuming is cheap, so this asserts both halves — the yielded generation
// is still exactly a retiring generation, and repeated passes under the same
// tiny budget finish the job in a bounded number of them.
//
// Revert-red: drop the `pass.spent()` check from deletePayloadChunks and the
// first retire returns nil with everything already collected, so the
// ErrPayloadSweepBudgetExhausted assertion fails.
func TestASweepYieldsOnItsBudgetAndFinishesOnResume(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	// One chunk per pass: the smallest budget that still makes progress.
	oneChunk := payloadSweepBudget{maxChunks: 1}
	if err := store.retirePayloadGeneration(ctx, generationID, nil, oneChunk); !errors.Is(err, ErrPayloadSweepBudgetExhausted) {
		t.Fatalf("first budgeted retire = %v, want %v", err, ErrPayloadSweepBudgetExhausted)
	}

	// No partial retirement is visible. The row is still there and still
	// retiring, which is the one state that means "this generation is being
	// collected and nothing may take a new reference to it".
	state, found := generationState(t, store, generationID)
	if !found || state != ViewGenerationRetiring {
		t.Fatalf("after a yielded sweep the catalog says found=%v state=%q, want a retiring row", found, state)
	}
	if got := store.payloadSealFor(generationID).state.Load(); got != payloadSealRetired {
		t.Fatalf("seal after a yielded sweep = %d, want payloadSealRetired (%d)", got, payloadSealRetired)
	}
	// A yield is not a failure: nothing is reported as a storage problem.
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a budget yield reported storage failures: %+v", failures)
	}
	// And the next pass does not start over.
	if cursor := store.payloadSweepStateFor(generationID).cursor(); cursor == 0 {
		t.Fatal("a yielded sweep left no resume cursor, so every pass re-walks the tables it already emptied")
	}

	// Resume under the same budget until it finishes. The bound is the real
	// assertion and it is deliberately loose — the number of passes a
	// one-chunk budget needs is the number of tables the sweep registries
	// declare, which grows with the schema. What is being pinned is that the
	// number is finite: a sweep that yielded without progress never gets here.
	const maxPasses = 512
	passes := 0
	for {
		err := store.retirePayloadGeneration(ctx, generationID, nil, oneChunk)
		passes++
		if err == nil {
			break
		}
		if !errors.Is(err, ErrPayloadSweepBudgetExhausted) {
			t.Fatalf("resumed retire pass %d = %v", passes, err)
		}
		if passes >= maxPasses {
			t.Fatalf("a one-chunk sweep made no progress: still yielding after %d passes", passes)
		}
	}
	t.Logf("one-chunk budget finished generation %d in %d resumed passes", generationID, passes+1)

	if _, found := generationState(t, store, generationID); found {
		t.Fatal("the catalog row survived the completed sweep")
	}
	for _, table := range payloadGenerationTables() {
		if got := countAtGeneration(t, store, table, generationID); got != 0 {
			t.Fatalf("%s still holds %d rows at generation %d after the resumed sweep", table, got, generationID)
		}
	}
	// The base corpus is untouched: a budgeted sweep collects one generation,
	// not whatever it happened to reach when it ran out.
	if got := countAtGeneration(t, store, "nodes", baseViewGeneration); got != 3 {
		t.Fatalf("base nodes after the resumed sweep = %d, want the 3 the base carries", got)
	}
}

// TestTheDefaultSweepBudgetIsACeilingNotASlice pins the budget production
// actually runs under, which is the only one no test can inject.
//
// Two things are asserted and they pull in opposite directions, which is the
// point. Every axis must be bounded: an unbounded default puts the sweep back
// where this item found it, draining each table until a chunk removes nothing
// with no total row, chunk or elapsed bound. And every axis must be bounded far
// above what a real generation costs, because a yield is only free for a caller
// that offers the generation again — internal/indexer/repository_cleanup.go:264
// propagates every retirement error but ErrCatalogNotFound, and an untrack
// reports a retryable "pending" only for ErrRepositoryCleanupPending, so a
// default small enough to fire on an ordinary retirement turns untracking a
// large repository from slow into failed.
//
// Lowering these into a scheduler slice therefore has a prerequisite, and this
// test is where it is written down: teach repository_cleanup.go to treat
// ErrPayloadSweepBudgetExhausted as pending first.
//
// Revert-red: replace defaultPayloadSweepBudget's body with
// payloadSweepBudget{} — the mutation that makes production unbounded — and the
// first assertion fails.
func TestTheDefaultSweepBudgetIsACeilingNotASlice(t *testing.T) {
	budget := defaultPayloadSweepBudget()
	if budget.maxRows <= 0 || budget.maxChunks <= 0 || budget.maxElapsed <= 0 {
		t.Fatalf("production retirement runs unbounded on at least one axis: %+v", budget)
	}

	// A whole large repository's graph — nodes, edges, every sidecar and every
	// FTS map — is a few million rows. The ceiling has to sit well clear of
	// that on both row-shaped axes and on the clock.
	const wellClearOfARealGeneration = 16_000_000
	if budget.maxRows < wellClearOfARealGeneration {
		t.Fatalf("row ceiling %d can fire on a real generation; untrack would report failure "+
			"instead of pending (repository_cleanup.go:264)", budget.maxRows)
	}
	if rows := budget.maxChunks * payloadGenerationSweepBatch; rows < wellClearOfARealGeneration {
		t.Fatalf("chunk ceiling %d is %d rows, which can fire on a real generation; untrack would "+
			"report failure instead of pending (repository_cleanup.go:264)", budget.maxChunks, rows)
	}
	if budget.maxElapsed < time.Minute {
		t.Fatalf("elapsed ceiling %v is a scheduler slice, not a runaway ceiling; teach "+
			"repository_cleanup.go to treat ErrPayloadSweepBudgetExhausted as pending first",
			budget.maxElapsed)
	}
	if budget.now != nil {
		t.Fatal("production reads an injected clock")
	}
}

// TestASweepYieldsOnItsRowBudget exercises the row axis end to end.
//
// The chunk axis is the one the other budget tests drive, and the two are not
// the same check: a sweep across many already-empty tables spends chunks
// without removing rows, and a sweep of one enormous table spends rows without
// spending many chunks. This is the second shape, so the budget is stated in
// rows only — every other axis unbounded — and the yield can therefore come
// from nowhere else.
//
// Revert-red: drop the maxRows arm from payloadSweepPass.spent() and the first
// retire runs to completion instead of yielding.
func TestASweepYieldsOnItsRowBudget(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	oneRow := payloadSweepBudget{maxRows: 1}
	if err := store.retirePayloadGeneration(ctx, generationID, nil, oneRow); !errors.Is(err, ErrPayloadSweepBudgetExhausted) {
		t.Fatalf("first row-budgeted retire = %v, want %v", err, ErrPayloadSweepBudgetExhausted)
	}
	state, found := generationState(t, store, generationID)
	if !found || state != ViewGenerationRetiring {
		t.Fatalf("after a row-budget yield the catalog says found=%v state=%q, want a retiring row", found, state)
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a row-budget yield reported storage failures: %+v", failures)
	}

	passes := 0
	for {
		err := store.retirePayloadGeneration(ctx, generationID, nil, oneRow)
		passes++
		if err == nil {
			break
		}
		if !errors.Is(err, ErrPayloadSweepBudgetExhausted) {
			t.Fatalf("resumed row-budgeted retire pass %d = %v", passes, err)
		}
		if passes >= 512 {
			t.Fatalf("a one-row budget made no progress: still yielding after %d passes", passes)
		}
	}
	if _, found := generationState(t, store, generationID); found {
		t.Fatal("the catalog row survived the completed sweep")
	}
	for _, table := range payloadGenerationTables() {
		if got := countAtGeneration(t, store, table, generationID); got != 0 {
			t.Fatalf("%s still holds %d rows at generation %d after the resumed sweep", table, got, generationID)
		}
	}
}

// TestASweepPassAlwaysRemovesAChunk is the termination half stated on its own.
//
// An elapsed budget is the axis that can be smaller than one chunk on a slow
// disk, and a pass that yields before doing anything turns the retry loop into
// a spin. The clock is injected rather than slept through: the budget is a
// yield policy and never a fence, so a test that proves the axis works must
// move time itself.
//
// Revert-red: remove the `p.chunks == 0` early return from spent() and the
// first pass yields having removed nothing, which the progress assertion
// catches.
func TestASweepPassAlwaysRemovesAChunk(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	// A clock that is already past its own budget every time it is read.
	ticks := 0
	expired := payloadSweepBudget{
		maxElapsed: time.Nanosecond,
		now: func() time.Time {
			ticks++
			return time.Unix(0, 0).Add(time.Duration(ticks) * time.Hour)
		},
	}

	before := countAtGeneration(t, store, "symbol_fts_rowid", generationID)
	if before == 0 {
		t.Fatal("fixture wrote no FTS rows, so a pass that removed nothing would look like progress")
	}
	if err := store.retirePayloadGeneration(ctx, generationID, nil, expired); !errors.Is(err, ErrPayloadSweepBudgetExhausted) {
		t.Fatalf("retire under an expired clock = %v, want %v", err, ErrPayloadSweepBudgetExhausted)
	}

	passes := 0
	for {
		err := store.retirePayloadGeneration(ctx, generationID, nil, expired)
		passes++
		if err == nil {
			break
		}
		if !errors.Is(err, ErrPayloadSweepBudgetExhausted) {
			t.Fatalf("resumed retire under an expired clock, pass %d = %v", passes, err)
		}
		if passes >= 512 {
			t.Fatalf("a sweep under an expired clock never finishes: %d passes and still yielding", passes)
		}
	}
	if _, found := generationState(t, store, generationID); found {
		t.Fatal("the catalog row survived the completed sweep")
	}
}

// TestPrivateRetirementSweepStopsCleanlyOnAFullVolume is the storage-refusal
// classification's disk-full arm and acceptance gate 9's.
//
// The failure is real, not a stub: the database's own page ceiling is pinned
// at its current size, and a BEFORE DELETE trigger on `nodes` makes the
// sweep's last step ask for pages the database is not allowed to have. SQLite
// answers SQLITE_FULL from inside the sweep's own transaction — the same code
// the user's crash reported — and the assertions are about what the store is
// left holding afterwards:
//
//   - the generation is still exactly a retiring generation, not a half-
//     retired one;
//   - the earlier steps' deletions survived, so the next pass resumes rather
//     than restarting;
//   - the reason is stated in the bounded, path-free vocabulary
//     SafeStorageFailureReason renders, which until this item had no caller at
//     all;
//   - a successful retry retracts it.
//
// Revert-red: return `err` instead of `s.noteRetirementFailure(...)` from
// RetirePayloadGeneration's sweep arm and the StorageFailures assertion fails
// with an empty list.
func TestPrivateRetirementSweepStopsCleanlyOnAFullVolume(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	if store.bulkConn != nil {
		t.Fatal("this page-limit fixture requires the actual non-bulk writer pool")
	}
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	// The ballast table and its trigger are created before the ceiling drops,
	// so the only thing that cannot fit is the trigger's own insert.
	mustExec(t, store, `CREATE TABLE gxh_sweep_ballast(id INTEGER PRIMARY KEY, payload BLOB)`)
	mustExec(t, store, `CREATE TRIGGER gxh_sweep_ballast_trigger BEFORE DELETE ON nodes BEGIN
    INSERT INTO gxh_sweep_ballast(payload) VALUES (zeroblob(8388608));
END`)

	previousLimit, err := privateFullPageLimit(ctx, store, 0)
	if err != nil {
		t.Fatalf("read page ceiling: %v", err)
	}
	var pages int64
	if err := store.writerDB.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatalf("read page count: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if _, err := privateFullPageLimit(context.Background(), store, previousLimit); err != nil {
			t.Errorf("restore page ceiling: %v", err)
		}
	}
	defer restore()
	capped, err := privateFullPageLimit(ctx, store, pages)
	if err != nil || capped != pages {
		t.Fatalf("page ceiling not installed: got=%d want=%d err=%v", capped, pages, err)
	}

	retireErr := store.RetirePayloadGeneration(ctx, generationID, nil)
	if retireErr == nil {
		t.Fatal("retirement succeeded with the volume full")
	}
	var storageErr *StorageError
	if !errors.As(retireErr, &storageErr) {
		t.Fatalf("a full-volume retirement failure is not classified as a storage failure: %T %v", retireErr, retireErr)
	}
	var coded interface{ Code() int }
	if !errors.As(retireErr, &coded) || coded.Code()&0xff != 13 {
		code := -1
		if coded != nil {
			code = coded.Code()
		}
		t.Fatalf("fixture did not cause an actual SQLITE_FULL: code=%d err=%v", code, retireErr)
	}

	// Nothing is half retired: the row is present and retiring.
	state, found := generationState(t, store, generationID)
	if !found || state != ViewGenerationRetiring {
		t.Fatalf("after a full-volume sweep the catalog says found=%v state=%q, want a retiring row", found, state)
	}
	// The nodes step is the last one, so everything before it committed: the
	// failure stopped the sweep, it did not roll it back.
	if got := countAtGeneration(t, store, "symbol_fts_rowid", generationID); got != 0 {
		t.Fatalf("the steps before the failing one were rolled back: symbol_fts_rowid still holds %d rows", got)
	}
	if got := countAtGeneration(t, store, "nodes", generationID); got == 0 {
		t.Fatal("the failing step deleted rows anyway, so the transaction did not roll back")
	}

	failures := store.StorageFailures()
	if len(failures) != 1 || failures[0].GenerationID != generationID {
		t.Fatalf("storage failures after a full volume = %+v, want one for generation %d", failures, generationID)
	}
	if !strings.Contains(failures[0].Reason, "SQLite code 13") ||
		!strings.Contains(failures[0].Reason, "free disk space") {
		t.Fatalf("stated reason %q does not name a full volume in the bounded vocabulary", failures[0].Reason)
	}
	if strings.Contains(failures[0].Reason, "gxh_sweep_ballast") {
		t.Fatalf("the stated reason leaks store internals: %q", failures[0].Reason)
	}

	// Free the space and retry: the sweep resumes at the step that failed.
	restore()
	mustExec(t, store, `DROP TRIGGER gxh_sweep_ballast_trigger`)
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire after the volume recovered: %v", err)
	}
	if _, found := generationState(t, store, generationID); found {
		t.Fatal("the catalog row survived the recovered retirement")
	}
	for _, table := range payloadGenerationTables() {
		if got := countAtGeneration(t, store, table, generationID); got != 0 {
			t.Fatalf("%s still holds %d rows at generation %d after the recovered retirement", table, got, generationID)
		}
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a successful retry left the failure standing: %+v", failures)
	}
}

// TestAFailedSweepReasonStatesTheLastAttempt pins the retraction rule on its
// own: the surface answers for the attempt that just ran, not for a history.
func TestAFailedSweepReasonStatesTheLastAttempt(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	if !store.RecordStorageFailure(generationID, &StorageError{}) {
		t.Fatal("a *StorageError was not recorded as a storage failure")
	}
	if failures := store.StorageFailures(); len(failures) != 1 {
		t.Fatalf("storage failures after recording one = %+v", failures)
	}
	// A refusal that is not the storage layer's is not recorded at all.
	if store.RecordStorageFailure(generationID, errors.New("a caller went away")) {
		t.Fatal("an ordinary error was recorded as a storage failure")
	}

	// The next attempt supersedes it, whatever its outcome.
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("RetirePayloadGeneration: %v", err)
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a completed retirement left a stale reason standing: %+v", failures)
	}
}

// TestARefusedAttemptRetractsTheLastReason is the retraction rule at its
// weakest point.
//
// The reason is durable state a person reads as the daemon's present, and the
// only thing that makes it present rather than historical is that every
// attempt on the generation retracts the last one's answer. An attempt that is
// refused before it reaches the sweep — the generation is leased again, or a
// reference reappeared — is still an attempt, and a generation that is merely
// held must not go on claiming the volume is why it is still there. Held is a
// fact the census already reports, one field up, as a lease.
//
// Revert-red: remove the ClearStorageFailure call at the head of
// retirePayloadGeneration and the stale reason survives the refusal.
func TestARefusedAttemptRetractsTheLastReason(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	if !store.RecordStorageFailure(generationID, &StorageError{}) {
		t.Fatal("a *StorageError was not recorded as a storage failure")
	}
	if failures := store.StorageFailures(); len(failures) != 1 {
		t.Fatalf("storage failures after recording one = %+v", failures)
	}

	held := func(int64) bool { return true }
	err := store.RetirePayloadGeneration(ctx, generationID, held)
	if !errors.Is(err, ErrPayloadGenerationInUse) {
		t.Fatalf("retire of a leased generation = %v, want %v", err, ErrPayloadGenerationInUse)
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a generation that is merely leased still states a storage failure: %+v", failures)
	}
	// The refusal left the generation exactly as it found it.
	state, found := generationState(t, store, generationID)
	if !found || state == ViewGenerationRetiring {
		t.Fatalf("a refused attempt moved the generation: found=%v state=%q", found, state)
	}
}

// TestACatalogDeleteRefusedByTheStorageLayerIsStated covers the last of
// retirement's three post-fence arms.
//
// The sweep arm is pinned by the full-volume test above; this is the arm after
// it, where every payload row is already gone and only the catalog row is
// left. A storage layer that refuses that delete leaves the most confusing
// state of all — a retiring generation that owns nothing — and it is the one
// state where the census's counts say least, so the reason matters most.
//
// The refusal is injected as a trigger rather than a page ceiling because what
// is being pinned is the arm, not the cause: any driver error from this delete
// is the storage layer speaking.
//
// Revert-red: return `err` instead of s.noteRetirementFailure(...) from the
// DeleteViewGeneration arm and the census assertion fails with an empty list.
func TestACatalogDeleteRefusedByTheStorageLayerIsStated(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	mustExec(t, store, `CREATE TRIGGER gxh_generation_delete_guard BEFORE DELETE ON view_generations BEGIN
    SELECT RAISE(ABORT, 'gxh_trigger: the store refused the catalog delete');
END`)

	err := store.RetirePayloadGeneration(ctx, generationID, nil)
	if err == nil {
		t.Fatal("retirement reported success although the catalog row could not be deleted")
	}
	var storageErr *StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("a refused catalog delete is not classified as a storage failure: %T %v", err, err)
	}
	state, found := generationState(t, store, generationID)
	if !found || state != ViewGenerationRetiring {
		t.Fatalf("after a refused catalog delete the row says found=%v state=%q, want retiring", found, state)
	}
	failures := store.StorageFailures()
	if len(failures) != 1 || failures[0].GenerationID != generationID {
		t.Fatalf("storage failures after a refused catalog delete = %+v, want one for generation %d",
			failures, generationID)
	}
	if !strings.Contains(failures[0].Reason, "graph storage") ||
		!strings.Contains(failures[0].Reason, "SQLite code") {
		t.Fatalf("stated reason %q does not name the storage layer in the bounded vocabulary", failures[0].Reason)
	}
	if strings.Contains(failures[0].Reason, "gxh_trigger") {
		t.Fatalf("the stated reason leaks the driver message: %q", failures[0].Reason)
	}

	// The payload is already gone, so the resumed attempt is the catalog
	// delete on its own — and it takes the reason with it.
	mustExec(t, store, `DROP TRIGGER gxh_generation_delete_guard`)
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire after the catalog delete was allowed again: %v", err)
	}
	if _, found := generationState(t, store, generationID); found {
		t.Fatal("the catalog row survived the recovered retirement")
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a completed retirement left the failure standing: %+v", failures)
	}
}

// TestTheStorageFailureRegisterNeverMintsAGeneration keeps the register
// bounded by what this process is holding.
//
// StorageFailures is ranged on every health census, and the map it ranges is
// the seal map — per-generation state whose size is otherwise bounded by the
// generations this process has opened a handle on and not yet retired. The
// recording half takes a generation id as an argument, so if it created state
// for whatever it was handed, any caller outside the payload lifecycle could
// grow that map permanently: a seal is dropped only by a successful retirement
// of that same id, which an id nothing ever minted never gets.
//
// Revert-red: look the seal up with payloadSealFor instead of
// payloadSealIfPresent and the stray id acquires permanent state.
func TestTheStorageFailureRegisterNeverMintsAGeneration(t *testing.T) {
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	const stray = int64(987_654_321)
	if store.RecordStorageFailure(stray, &StorageError{}) {
		t.Fatal("a storage failure was recorded against a generation this process holds nothing for")
	}
	if _, ok := store.payloadSeals.Load(stray); ok {
		t.Fatal("the register minted permanent per-generation state for an id it was merely handed")
	}
	store.ClearStorageFailure(stray)
	if _, ok := store.payloadSeals.Load(stray); ok {
		t.Fatal("retracting a reason minted permanent per-generation state for a stray id")
	}
	if failures := store.StorageFailures(); len(failures) != 0 {
		t.Fatalf("a stray id reached the census: %+v", failures)
	}

	// A generation this process is holding is recorded as it always was.
	if !store.RecordStorageFailure(generationID, &StorageError{}) {
		t.Fatal("a storage failure against a live generation was refused")
	}
	if failures := store.StorageFailures(); len(failures) != 1 || failures[0].GenerationID != generationID {
		t.Fatalf("storage failures = %+v, want one for generation %d", failures, generationID)
	}
}

func mustExec(t *testing.T, store *Store, query string) {
	t.Helper()
	if _, err := store.writerDB.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// TestAStorageFailureReadRunsBesideARetirement is the concurrency pin for the
// new shared state. The census is assembled on the daemon's status path while
// the janitor is retiring generations on its own loop, so the register is read
// concurrently with every write to it by construction.
func TestAStorageFailureReadRunsBesideARetirement(t *testing.T) {
	ctx := context.Background()
	store := openPayloadStore(t)
	seedPayloadBase(t, store)
	seedPayloadControlPlane(t, store)
	generationID := publishedPayloadGeneration(t, store)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Both halves of the surface, in the order the census reads
				// them: the whole register, then one generation's reason.
				_ = store.StorageFailures()
				store.RecordStorageFailure(generationID, &StorageError{})
			}
		}()
	}
	retireErr := store.RetirePayloadGeneration(ctx, generationID, nil)
	close(stop)
	readers.Wait()
	if retireErr != nil {
		t.Fatalf("RetirePayloadGeneration beside concurrent census reads: %v", retireErr)
	}
	if _, found := generationState(t, store, generationID); found {
		t.Fatal("the catalog row survived the retirement")
	}
}
