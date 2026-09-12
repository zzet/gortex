package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The retirement sweep's budget, its resume state, and the reason it stopped.
//
// Retirement is the one operation in the payload lifecycle whose cost is set
// by the corpus rather than by the request: a generation that replaced a large
// repository's whole graph carries millions of rows, and the sweep that
// collects it runs inside a janitor pass that has other generations waiting
// behind it. Until this file existed the sweep simply drained every table
// until a chunk removed nothing — bounded per chunk, unbounded in total — and
// a disk-full or damaged store surfaced as an unclassified error with nothing
// durable to read afterwards.
//
// Three things live here, and they are the same thing seen from three sides:
//
//   - a *budget*, so one pass yields instead of running to completion;
//   - a *cursor*, so the next pass resumes where the last one stopped rather
//     than re-walking the tables it already emptied;
//   - a *reason*, so a pass that stopped because the storage layer refused it
//     leaves a bounded, path-free sentence a person can read on the health
//     census.
//
// None of the three is an authority. The authority is the catalog's `retiring`
// fence, which is committed before the first chunk and removed only after the
// last: a yielded, failed or crashed sweep leaves that fence exactly as it
// was, so no partial retirement is ever visible and the next pass is free to
// start over from scratch if the in-memory cursor is gone.

// Payload-sweep budget defaults.
//
// The two row-shaped budgets are expressed in the sweep's own unit — one chunk
// is at most payloadGenerationSweepBatch rows, each in its own transaction —
// so they bound the number of times the mutation gate is taken as well as the
// number of rows removed. The elapsed budget bounds the one thing the other
// two cannot: a pass whose chunks are individually cheap but collectively
// slower than the caller can afford, because the disk is busy or the rows are
// wide.
//
// These are *ceilings on the pathological case*, not fair-share slices, and the
// difference is load-bearing. A yield is only free for a caller that offers the
// generation again: the coordinator backlog, the lifecycle's owed set and the
// ref-view retention pass all do, but the repository-cleanup path does not —
// internal/indexer/repository_cleanup.go:264 returns any retirement error that
// is not ErrCatalogNotFound, and only ErrRepositoryCleanupPending is mapped
// onto a retryable untrack. A default small enough to fire on an ordinary
// retirement would therefore turn untracking a large repository from "this
// takes a while" into "this failed". So the defaults are set well above the
// payload any real generation carries — roughly sixty-four million rows, or
// ten minutes of continuous deleting, against the low millions a very large
// repository's whole graph amounts to — and what they actually stop is a sweep
// that is not converging at all.
//
// Lowering them into a scheduler slice is a real improvement and a separate
// change: it needs repository_cleanup.go to map ErrPayloadSweepBudgetExhausted
// onto its pending sentinel first. TestTheDefaultSweepBudgetIsACeilingNotASlice
// states that dependency where someone lowering these numbers will read it.
const (
	payloadSweepMaxRows    = 64_000_000
	payloadSweepMaxChunks  = 64_000
	payloadSweepMaxElapsed = 10 * time.Minute
)

// ErrPayloadSweepBudgetExhausted means a retirement sweep stopped on its own
// budget with work left to do. It is not a failure: the generation keeps its
// retiring fence, its payload rows are a strict subset of what they were, and
// the next pass continues from the step this one stopped on.
//
// Most callers resume it without needing to know that is what they are doing —
// the coordinator backlog, the lifecycle's owed set and the ref-view retention
// pass all offer a refused retirement again. One does not:
// internal/indexer/repository_cleanup.go:264 propagates every retirement error
// but ErrCatalogNotFound, and an untrack only reports a retryable "pending" for
// ErrRepositoryCleanupPending. That is why the default budget above is a
// runaway ceiling rather than a slice, and it is the reason to fix before
// lowering it.
var ErrPayloadSweepBudgetExhausted = errors.New("store_sqlite: payload generation sweep yielded on its budget")

// payloadSweepBudget bounds one sweep pass. A zero or negative field is
// unbounded on that axis, which is what a caller that must finish (a test
// asserting a whole generation is gone) asks for.
//
// now is the clock the elapsed budget reads. It exists because a wall-clock
// deadline in a test is a flake generator: the budget is a yield policy, never
// a fence, so a test that wants to prove the elapsed axis works must be able
// to move time itself rather than sleep through it.
type payloadSweepBudget struct {
	maxRows    int64
	maxChunks  int64
	maxElapsed time.Duration
	now        func() time.Time
}

// defaultPayloadSweepBudget is what production retirement runs under.
func defaultPayloadSweepBudget() payloadSweepBudget {
	return payloadSweepBudget{
		maxRows:    payloadSweepMaxRows,
		maxChunks:  payloadSweepMaxChunks,
		maxElapsed: payloadSweepMaxElapsed,
	}
}

// payloadSweepPass is one pass's accounting against a budget.
type payloadSweepPass struct {
	budget  payloadSweepBudget
	started time.Time
	rows    int64
	chunks  int64
}

// begin opens a pass. The clock is read once here so that every later
// comparison is against the same start.
func (b payloadSweepBudget) begin() *payloadSweepPass {
	if b.now == nil {
		b.now = time.Now
	}
	return &payloadSweepPass{budget: b, started: b.now()}
}

// spent reports that the pass has used its budget and must yield.
//
// It is asked between chunks and never inside one, so a yield always lands on
// a committed transaction boundary: the rows a chunk removed are removed, the
// rows it did not are still addressable by the identical statement, and the
// generation is in exactly the state a crash at the same point would leave.
//
// A nil pass is unbounded, which keeps every internal caller that has no
// budget to spend from having to invent one.
func (p *payloadSweepPass) spent() bool {
	if p == nil {
		return false
	}
	// A pass always runs at least one chunk. Without this, a budget smaller
	// than the machine's own step — an elapsed budget on a disk slower than
	// it, a row budget below one batch — would yield before removing
	// anything, and the retry loop that resumes it would spin forever making
	// no progress. One chunk per pass is what turns "bounded per pass" into
	// "terminates".
	if p.chunks == 0 {
		return false
	}
	if p.budget.maxChunks > 0 && p.chunks >= p.budget.maxChunks {
		return true
	}
	if p.budget.maxRows > 0 && p.rows >= p.budget.maxRows {
		return true
	}
	if p.budget.maxElapsed > 0 && p.budget.now().Sub(p.started) >= p.budget.maxElapsed {
		return true
	}
	return false
}

// spend records one committed chunk. Chunks count even when they removed
// nothing: an empty chunk is still a transaction on the mutation gate, and a
// sweep across many already-empty tables is exactly the shape whose cost the
// row budget cannot see.
func (p *payloadSweepPass) spend(rows int64) {
	if p == nil {
		return
	}
	p.chunks++
	p.rows += rows
}

// payloadSweepState is the per-generation retirement state a sweep carries
// between passes.
//
// It hangs off the generation's payloadSeal because that is the object every
// handle on the generation already shares, and because the seal's lifetime is
// exactly the right one: it is dropped when the generation is finally retired,
// which is the moment both the cursor and the reason stop being true.
//
// Both fields are hints, not authority. Losing them — a daemon restart, a seal
// evicted by a failed resolve — costs one cheap re-walk of the already-empty
// tables and an unexplained absence on the census, never a wrong deletion.
type payloadSweepState struct {
	mu     sync.Mutex
	step   int
	reason string
}

func (p *payloadSweepState) cursor() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.step
}

func (p *payloadSweepState) advance(step int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if step > p.step {
		p.step = step
	}
}

func (p *payloadSweepState) setReason(reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reason = reason
}

func (p *payloadSweepState) failureReason() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason
}

// StorageFailure is one payload generation's last storage-layer maintenance
// failure: the generation it happened to, and the bounded, path-free sentence
// SafeStorageFailureReason renders for it.
//
// It carries no error, no path and no SQL. A readiness payload is read by a
// person and shipped off a machine, and the full cause — which is kept on the
// *StorageError the failing call returned, and logged by the caller — belongs
// in the daemon log rather than in a status poll.
type StorageFailure struct {
	GenerationID int64  `json:"generation_id"`
	Reason       string `json:"reason"`
}

// RecordStorageFailure states that a generation's storage maintenance failed,
// and reports whether the error was a storage failure at all.
//
// Only the storage layer's own refusals are recorded (see storageFailure): a
// cancelled context, a lost retirement fence or a lifecycle refusal is a
// normal outcome of a background pass, and putting "check the store volume" in
// front of a person over one would make the surface useless the first time it
// mattered. A caller that needs every failure remembered wants its own log
// line, not this.
//
// The reason replaces whatever the generation last recorded, so the surface
// states the most recent attempt's outcome rather than a history.
// RetirePayloadGeneration retracts it at the start of every attempt on the
// generation, refusals included, and a successful retirement drops the whole
// seal and takes the reason with it.
//
// It returns false for a generation this process holds no state about. That is
// not a fallback: a build or a sweep that failed necessarily held a handle on
// the generation it was writing, and that handle is what creates the state
// this hangs on. Refusing to create it here is what bounds the register by the
// generations this process is actually working on rather than by the ids its
// callers pass in — the register is ranged on every health census, and an
// id-shaped argument from outside the lifecycle must not be able to grow it.
func (s *Store) RecordStorageFailure(generationID int64, err error) bool {
	reason, ok := storageFailureReason(err)
	if !ok {
		return false
	}
	return s.recordStorageFailureReason(generationID, reason)
}

func (s *Store) recordStorageFailureReason(generationID int64, reason string) bool {
	state := s.payloadSweepStateFor(generationID)
	if state == nil {
		return false
	}
	state.setReason(reason)
	return true
}

// ClearStorageFailure retracts a generation's stated reason, and like the
// recording half it never creates state for a generation this process is not
// already holding.
func (s *Store) ClearStorageFailure(generationID int64) {
	s.payloadSweepStateFor(generationID).setReason("")
}

// StorageFailures reports every generation whose storage maintenance last
// failed, in generation order.
//
// It is a read of in-process state with no catalog query behind it, bounded by
// the number of generations this process still holds a seal for, which is what
// makes it cheap enough to ride on every health census.
func (s *Store) StorageFailures() []StorageFailure {
	if s.coreless() {
		return nil
	}
	var out []StorageFailure
	s.payloadSeals.Range(func(key, value any) bool {
		generationID, ok := key.(int64)
		if !ok {
			return true
		}
		seal, ok := value.(*payloadSeal)
		if !ok {
			return true
		}
		if reason := seal.sweep.failureReason(); reason != "" {
			out = append(out, StorageFailure{GenerationID: generationID, Reason: reason})
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].GenerationID < out[j].GenerationID })
	return out
}

// noteRetirementFailure classifies a retirement failure, records it against
// the generation when it is the storage layer refusing, and returns the error
// the caller propagates.
//
// The returned error is the classified one: a raw driver error becomes a
// *StorageError, so a caller that already recovers storage failures (the
// mutation coordinator, the watcher) recognizes a failed retirement the same
// way it recognizes a failed write, and one that does not recover them is
// unaffected — the cause is still reachable through errors.Is/As.
func (s *Store) noteRetirementFailure(generationID int64, err error) error {
	classified, ok := storageFailure(err)
	if !ok {
		return err
	}
	s.recordStorageFailureReason(generationID, SafeStorageFailureReason(classified))
	return classified
}

// payloadSweepStep is one drain the sweep performs, in order. A step is done
// when its chunk removes nothing, and the order is the one sweepPayloadRun
// documents: nothing a later step deletes is the only handle on something an
// earlier one left behind.
type payloadSweepStep struct {
	name string
	run  func(ctx context.Context, pass *payloadSweepPass) error
}

// payloadSweepSteps is the ordered drain plan for one generation.
//
// The plan is derived, not listed: the FTS maps come from the docid registry
// and the middle block from the sidecar and mask registries, so a table added
// to either is swept without being named twice. Each step builds its own
// statement when it runs, so a resumed sweep pays nothing for the steps it
// skips.
func (s *Store) payloadSweepSteps(generationID int64) []payloadSweepStep {
	base := s.atBase()
	steps := []payloadSweepStep{{
		name: "analysis_generations",
		run: func(ctx context.Context, pass *payloadSweepPass) error {
			return base.sweepAnalysisGenerations(ctx, generationID, pass)
		},
	}}
	for _, docidMap := range generationFTSDocidMaps {
		steps = append(steps, payloadSweepStep{
			name: docidMap.fts,
			run: func(ctx context.Context, pass *payloadSweepPass) error {
				return base.deletePayloadChunks(ctx, generationID, deleteFTSDocidChunk(docidMap, generationID), pass)
			},
		})
	}
	for _, table := range payloadSweepTables() {
		steps = append(steps, payloadSweepStep{
			name: table,
			run: func(ctx context.Context, pass *payloadSweepPass) error {
				query, err := base.payloadSweepDeleteSQL(table)
				if err != nil {
					return fmt.Errorf("payload generation gc: %s: %w", table, err)
				}
				return base.deletePayloadChunks(ctx, generationID, deleteGenerationRowsChunk(query, generationID), pass)
			},
		})
	}
	for _, spec := range []struct {
		name  string
		query string
	}{
		{name: "edges", query: deleteGenerationEdgesSQL},
		{name: "nodes", query: deleteGenerationNodesSQL},
	} {
		steps = append(steps, payloadSweepStep{
			name: spec.name,
			run: func(ctx context.Context, pass *payloadSweepPass) error {
				return base.deletePayloadChunks(ctx, generationID, deleteGenerationRowsChunk(spec.query, generationID), pass)
			},
		})
	}
	return steps
}

// sweepPayloadGeneration deletes every payload row a generation owns, in
// bounded chunks and within one pass's budget.
//
// The order is what makes an interrupted sweep safe. The whole-graph analysis
// cache goes first: its rows hang off analysis_generations.generation_id
// rather than carrying view_gen, so neither sweep registry names them, and
// leaving them would strand an analysis describing a corpus that no longer
// exists. The FTS documents go next, each chunk taking the map rows that
// addressed it along, because the map is the only handle on an FTS row. The
// core tables go last, so a sweep that stops half way never leaves a sidecar
// row pointing at a node that is already gone.
//
// Resumption skips the steps a previous pass finished. That is sound because
// the generation is sealed before the first chunk (payloadSealRetired) and the
// catalog refuses new references to a retiring generation, so no step can
// re-populate a table an earlier step emptied. A lost cursor costs one empty
// chunk per completed step and nothing else.
func (s *Store) sweepPayloadGeneration(ctx context.Context, generationID int64, pass *payloadSweepPass) error {
	steps := s.payloadSweepSteps(generationID)
	state := s.payloadSweepStateFor(generationID)
	// The last attempt's reason was already retracted by the attempt that
	// called this — see RetirePayloadGeneration, which does it for the
	// refusals as well, so the retraction rule is "per attempt" everywhere
	// rather than "per attempt that reached the sweep".
	start := state.cursor()
	if start < 0 || start > len(steps) {
		start = 0
	}
	for i := start; i < len(steps); i++ {
		if err := steps[i].run(ctx, pass); err != nil {
			return err
		}
		state.advance(i + 1)
	}
	return nil
}

// payloadSweepStateFor is the generation's sweep state, or nil for one this
// process holds no seal for — the base corpus, which is never retired, a
// coreless handle, and a generation nothing here has opened.
//
// It looks the seal up without creating one. Retirement has already sealed the
// generation (payloadSealRetired) before the sweep asks for this, and a
// generation no handle was ever opened on has nothing to report; see
// payloadSealIfPresent for why the difference matters.
func (s *Store) payloadSweepStateFor(generationID int64) *payloadSweepState {
	seal := s.payloadSealIfPresent(generationID)
	if seal == nil {
		return nil
	}
	return &seal.sweep
}

// sweepAnalysisGenerations removes every analysis generation stamped with the
// retiring payload view generation, its active pointer included.
//
// Unlike PruneAnalysisGenerations — the retention window, which never touches
// an active generation — this collects the active one too: the corpus it
// describes is being deleted. The pointer goes first because it holds
// ON DELETE RESTRICT against the manifest rows, then each generation's
// children child-first, then its manifest row. Every step rides
// deletePayloadChunks, so each batch is its own transaction under the mutation
// gate, re-checks that the generation is still retiring, and spends the pass's
// budget like any other chunk.
func (s *Store) sweepAnalysisGenerations(ctx context.Context, generationID int64, pass *payloadSweepPass) error {
	analysisIDs, err := s.analysisGenerationIDsForView(ctx, generationID)
	if err != nil {
		return fmt.Errorf("payload generation gc: analysis generations: %w", err)
	}
	if len(analysisIDs) == 0 {
		// No manifest row means no pointer either: the foreign key cannot be
		// satisfied without one.
		return nil
	}
	if err := s.deletePayloadChunks(ctx, generationID, deleteAnalysisPointerChunk(generationID), pass); err != nil {
		return err
	}
	for _, analysisID := range analysisIDs {
		for _, table := range analysisGenerationGCTables {
			if err := s.deletePayloadChunks(ctx, generationID, deleteAnalysisChildChunk(table, analysisID), pass); err != nil {
				return fmt.Errorf("payload generation gc: analysis %d %s: %w", analysisID, table.name, err)
			}
		}
		if err := s.deletePayloadChunks(ctx, generationID, deleteAnalysisManifestChunk(analysisID), pass); err != nil {
			return fmt.Errorf("payload generation gc: analysis %d manifest: %w", analysisID, err)
		}
	}
	return nil
}
