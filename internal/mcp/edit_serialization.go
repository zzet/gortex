package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/indexer"
)

const (
	defaultMutationReindexWait = 3 * time.Second
	defaultMutationSafetyWait  = 3 * time.Second
	mutationReceiptRetention   = 10 * time.Minute
)

var mutationReceiptSequence atomic.Uint64

// mutationPathLocks serializes read-modify-write tool calls per on-disk path.
// MCP requests can run concurrently, and atomic rename only makes each write
// indivisible; without this lock two handlers can still read the same snapshot
// and silently overwrite one another. Entries are reference-counted so a large
// daemon does not retain one lock for every file ever edited.
var mutationPathLocks = struct {
	sync.Mutex
	byPath map[string]*mutationPathLock
}{byPath: make(map[string]*mutationPathLock)}

type mutationPathLock struct {
	token chan struct{}
	refs  int
}

// mutationReindexOutcome is the complete freshness state produced after a disk
// mutation. A receipt identifies admitted publication work that can outlive the
// request, either on a watcher or on the selected checkout's coordinator.
type mutationReindexOutcome struct {
	Reindexed         bool
	Pending           bool
	Receipt           string
	Generation        uint64
	AppliedGeneration uint64
	Err               error
	// A checkout generation is not reflected in the canonical syntax-health
	// reader. Do not attach another checkout's parse errors to this result.
	checkoutScoped      bool
	checkoutID          string
	checkoutIncarnation string
}

type mutationReceipt struct {
	id                         string
	repo                       string
	path                       string
	generation                 uint64
	done                       chan struct{}
	mu                         sync.RWMutex
	result                     indexer.MutationResult
	completed                  bool
	checkoutScoped             bool
	checkoutID                 string
	checkoutIncarnation        string
	barrierRecoveredGeneration uint64
}

type mutationScheduler interface {
	EnqueueFileMutation(context.Context, string) (*indexer.MutationTicket, error)
}

// acquireMutationPath waits for exclusive mutation access to path. Waiting is
// context-aware: a cancelled MCP request leaves the queue immediately, which
// lets its dispatcher goroutine finish and release admission capacity.
func acquireMutationPath(ctx context.Context, path string) (func(), error) {
	if err := guardCheckoutMutationPath(ctx, path); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)

	mutationPathLocks.Lock()
	entry := mutationPathLocks.byPath[path]
	if entry == nil {
		entry = &mutationPathLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		mutationPathLocks.byPath[path] = entry
	}
	entry.refs++
	mutationPathLocks.Unlock()

	select {
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			releaseMutationPathRef(path, entry)
			return nil, err
		}
	case <-ctx.Done():
		releaseMutationPathRef(path, entry)
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.token <- struct{}{}
			releaseMutationPathRef(path, entry)
		})
	}, nil
}

// acquireMutationPaths locks a mutation set in lexical path order. A stable
// order prevents deadlock between overlapping batches; cancellation while
// waiting releases every previously acquired path in reverse order.
func acquireMutationPaths(ctx context.Context, paths []string) (func(), error) {
	unique := make(map[string]struct{}, len(paths))
	ordered := make([]string, 0, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, exists := unique[clean]; exists {
			continue
		}
		unique[clean] = struct{}{}
		ordered = append(ordered, clean)
	}
	sort.Strings(ordered)

	releases := make([]func(), 0, len(ordered))
	for _, path := range ordered {
		release, err := acquireMutationPath(ctx, path)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		})
	}, nil
}

func releaseMutationPathRef(path string, entry *mutationPathLock) {
	mutationPathLocks.Lock()
	defer mutationPathLocks.Unlock()
	entry.refs--
	if entry.refs == 0 && mutationPathLocks.byPath[path] == entry {
		delete(mutationPathLocks.byPath, path)
	}
}

func (s *Server) trackMutationTicket(ticket *indexer.MutationTicket) *mutationReceipt {
	repo := ""
	if s.multiIndexer != nil {
		repo = s.multiIndexer.RepoForFile(ticket.Path)
	}
	return s.trackScopedMutationTicket(ticket, repo, "", "", nil)
}

func (s *Server) trackCheckoutRefreshTicket(ticket *indexer.CheckoutRefreshTicket) *mutationReceipt {
	return s.trackScopedMutationTicket(ticket.Ticket, ticket.RepoPrefix, ticket.CheckoutID, ticket.Incarnation, nil)
}

// trackCheckoutRecoveryTicket lets verified whole-checkout recovery retire
// historical freshness gaps without rewriting their original result/error.
// eligible must be captured before RequestCheckoutRefresh starts sampling.
func (s *Server) trackCheckoutRecoveryTicket(ticket *indexer.CheckoutRefreshTicket, eligible map[string]struct{}) *mutationReceipt {
	var captured map[string]struct{}
	if ticket.ContentHash == "" && filepath.Clean(ticket.Ticket.Path) == filepath.Clean(ticket.Root) {
		captured = make(map[string]struct{}, len(eligible))
		for id := range eligible {
			captured[id] = struct{}{}
		}
	}
	return s.trackScopedMutationTicket(ticket.Ticket, ticket.RepoPrefix, ticket.CheckoutID, ticket.Incarnation, captured)
}

func (s *Server) trackScopedMutationTicket(ticket *indexer.MutationTicket, repo, checkoutID, incarnation string, recoveryCandidates map[string]struct{}) *mutationReceipt {
	receipt := &mutationReceipt{
		id:                  fmt.Sprintf("mutation-%d", mutationReceiptSequence.Add(1)),
		repo:                repo,
		path:                ticket.Path,
		generation:          ticket.Generation,
		done:                make(chan struct{}),
		checkoutScoped:      checkoutID != "",
		checkoutID:          checkoutID,
		checkoutIncarnation: incarnation,
	}
	s.mutationReceipts.Store(receipt.id, receipt)
	go func() {
		result, ok := <-ticket.Done
		if !ok {
			result = indexer.MutationResult{
				RequestedGeneration: ticket.Generation,
				Err:                 fmt.Errorf("mutation ticket generation %d closed without a result", ticket.Generation),
			}
		}
		receipt.mu.Lock()
		receipt.result = result
		receipt.completed = true
		receipt.mu.Unlock()
		if result.Err == nil && result.Reindexed && !receipt.checkoutScoped {
			s.resolveSupersededFailedReceipts(receipt.path, receipt.generation, result)
		}
		if result.Err == nil && result.Reindexed && result.AppliedGeneration > 0 && receipt.checkoutScoped {
			s.resolveCheckoutRecoveryReceipts(receipt, recoveryCandidates, result.AppliedGeneration)
		}
		close(receipt.done)
		retention := mutationReceiptRetention
		if receipt.checkoutScoped {
			// A commit receipt can be queried long after publication finished.
			// Its freshness ticket must outlive that ledger's polling window.
			retention = mutationCommitRetention
		}
		time.AfterFunc(retention, func() {
			s.mutationReceipts.Delete(receipt.id)
		})
	}()
	return receipt
}

// Only failures already terminal before recovery admission are eligible. An
// in-flight edit which fails later, or a newer edit, must retain its own gate.
func (s *Server) failedCheckoutRefreshReceiptsBefore(checkoutID, incarnation string) map[string]struct{} {
	eligible := make(map[string]struct{})
	s.mutationReceipts.Range(func(_, value any) bool {
		receipt, ok := value.(*mutationReceipt)
		if !ok || !receipt.checkoutScoped || receipt.checkoutID != checkoutID || receipt.checkoutIncarnation != incarnation {
			return true
		}
		receipt.mu.RLock()
		failed := receipt.completed && (receipt.result.Err != nil || !receipt.result.Reindexed) && receipt.barrierRecoveredGeneration == 0
		receipt.mu.RUnlock()
		if failed {
			eligible[receipt.id] = struct{}{}
		}
		return true
	})
	return eligible
}

func (s *Server) resolveCheckoutRecoveryReceipts(recovery *mutationReceipt, eligible map[string]struct{}, generation uint64) {
	for id := range eligible {
		value, ok := s.mutationReceipts.Load(id)
		if !ok {
			continue
		}
		receipt, ok := value.(*mutationReceipt)
		if !ok || !receipt.checkoutScoped || receipt.checkoutID != recovery.checkoutID || receipt.checkoutIncarnation != recovery.checkoutIncarnation || receipt.generation >= recovery.generation {
			continue
		}
		receipt.mu.Lock()
		if receipt.completed && (receipt.result.Err != nil || !receipt.result.Reindexed) {
			receipt.barrierRecoveredGeneration = generation
		}
		receipt.mu.Unlock()
	}
}

// resolveSupersededFailedReceipts resolves terminally failed receipts for a
// path once a later generation of the same path has been applied
// successfully. The graph then reflects newer bytes than the failed
// generation ever wrote, so the stale failure no longer describes a real
// freshness gap — keeping it would only fail freshness barriers that waiting
// cannot heal, because a terminal error never completes differently.
//
// The failed receipt is resolved in place rather than deleted: the
// mutation-commit ledger refreshes its graph half through
// mutationReceiptState, and a deleted receipt would leave that record
// reading "pending" forever. Stamping the superseding apply mirrors how
// completeMutationWaiters resolves earlier waiters with the later apply's
// result. Pending receipts and failures at or above the succeeded
// generation are left untouched. The succeeded result is passed by value so
// the sweep holds no lock besides the receipt it is stamping.
func (s *Server) resolveSupersededFailedReceipts(succeededPath string, succeededGeneration uint64, applied indexer.MutationResult) {
	cleanPath := filepath.Clean(succeededPath)
	s.mutationReceipts.Range(func(_, value any) bool {
		other, ok := value.(*mutationReceipt)
		if !ok || other.checkoutScoped {
			return true
		}
		if other.generation >= succeededGeneration || filepath.Clean(other.path) != cleanPath {
			return true
		}
		other.mu.Lock()
		if other.completed && (other.result.Err != nil || !other.result.Reindexed) {
			other.result = indexer.MutationResult{
				RequestedGeneration: other.generation,
				AppliedGeneration:   applied.AppliedGeneration,
				Reindexed:           true,
			}
		}
		other.mu.Unlock()
		return true
	})
}

// resolveReindexedPathReceipts resolves terminally failed freshness receipts
// for a path that an explicit reindex has just re-parsed. A failed ingest
// fail-closes change.detect for the whole repository until retention lapses,
// and reindex_repository was the obvious recovery that did not work: it
// re-reads the file into the graph but never touched this map, so the barrier
// kept reporting a gap the graph no longer had.
//
// The evidence a failed receipt is waiting for is exactly "the graph has read
// the current bytes of this path", which a successful scoped re-parse
// provides. Pending receipts are left alone: they describe work still in
// flight, not a dead generation, and clearing one would hide a real gap.
// failedReceiptsBefore snapshots the failed freshness receipts a reindex pass
// is entitled to resolve. It MUST be called before the pass starts: a receipt
// that fails its own ingest while the pass is running was never read by it, so
// resolving it would certify bytes the graph never saw. Receipt IDs are the
// bound rather than a timestamp because receipts carry no completion time.
//
// A nil result resolves nothing, which is the correct reading of "no snapshot
// was taken".
func (s *Server) failedReceiptsBefore(paths []string, root string) map[string]struct{} {
	candidate, ok := reindexCandidatePath(paths, root)
	if !ok {
		return nil
	}
	eligible := make(map[string]struct{})
	s.mutationReceipts.Range(func(_, value any) bool {
		receipt, ok := value.(*mutationReceipt)
		if !ok || receipt.checkoutScoped || filepath.Clean(receipt.path) != candidate {
			return true
		}
		receipt.mu.RLock()
		failed := receipt.completed && (receipt.result.Err != nil || !receipt.result.Reindexed)
		receipt.mu.RUnlock()
		if failed {
			eligible[receipt.id] = struct{}{}
		}
		return true
	})
	return eligible
}

func (s *Server) resolveReindexedPathReceipts(reindexedPath string, eligible map[string]struct{}) {
	cleanPath := filepath.Clean(reindexedPath)
	s.mutationReceipts.Range(func(_, value any) bool {
		receipt, ok := value.(*mutationReceipt)
		if !ok || receipt.checkoutScoped || filepath.Clean(receipt.path) != cleanPath {
			return true
		}
		if _, wasFailingBeforeThePass := eligible[receipt.id]; !wasFailingBeforeThePass {
			return true
		}
		receipt.mu.Lock()
		if receipt.completed && (receipt.result.Err != nil || !receipt.result.Reindexed) {
			receipt.result = indexer.MutationResult{
				RequestedGeneration: receipt.generation,
				AppliedGeneration:   receipt.generation,
				Reindexed:           true,
			}
		}
		receipt.mu.Unlock()
		return true
	})
}

func (r *mutationReceipt) outcome(pending bool) mutationReindexOutcome {
	r.mu.RLock()
	defer r.mu.RUnlock()
	outcome := mutationReindexOutcome{
		Pending:             pending,
		Receipt:             r.id,
		Generation:          r.generation,
		checkoutScoped:      r.checkoutScoped,
		checkoutID:          r.checkoutID,
		checkoutIncarnation: r.checkoutIncarnation,
	}
	if r.completed {
		outcome.Reindexed = r.result.Reindexed
		outcome.AppliedGeneration = r.result.AppliedGeneration
		outcome.Err = r.result.Err
		outcome.Pending = false
	}
	return outcome
}

func (s *Server) mutationWaitDuration() time.Duration {
	if s.mutationReindexWait > 0 {
		return s.mutationReindexWait
	}
	return defaultMutationReindexWait
}

func (s *Server) mutationSafetyWaitDuration() time.Duration {
	if s.mutationSafetyWait > 0 {
		return s.mutationSafetyWait
	}
	return defaultMutationSafetyWait
}

// mutationReindexState returns the graph-freshness state to expose after a
// successful disk mutation. A live watcher already owns debouncing, patch
// serialization, and latest-bytes reconciliation, so duplicating IndexFile in
// the request path both blocks the response and races the watcher. Embedded
// servers have no watcher and retain the synchronous freshness contract.
func (s *Server) mutationReindexState(ctx context.Context, absPath string) mutationReindexOutcome {
	if state := checkoutMutationFromContext(ctx); state != nil {
		return s.refreshCheckoutMutation(ctx, absPath, state)
	}
	if watcher := s.currentWatcher(); watcher != nil {
		// Admission is path-scoped and authoritative. Scheduling uses a detached
		// context because the disk commit already happened; client cancellation
		// must not leave the graph permanently stale.
		if scheduler, ok := watcher.(mutationScheduler); ok {
			ticket, scheduleErr := scheduler.EnqueueFileMutation(context.WithoutCancel(ctx), absPath)
			if scheduleErr != nil {
				return mutationReindexOutcome{Err: scheduleErr}
			}
			if ticket != nil {
				receipt := s.trackMutationTicket(ticket)
				timer := time.NewTimer(s.mutationWaitDuration())
				defer timer.Stop()
				select {
				case <-receipt.done:
					outcome := receipt.outcome(false)
					outcome.Receipt = ""
					return outcome
				case <-timer.C:
					return receipt.outcome(true)
				case <-ctx.Done():
					return receipt.outcome(true)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return mutationReindexOutcome{Err: err}
	}
	return mutationReindexOutcome{Reindexed: s.reindexFile(absPath)}
}

// awaitMutationFreshness is the conservative all-repository safety barrier.
// Callers that can resolve a target repository should use the scoped sibling.
func (s *Server) awaitMutationFreshness(ctx context.Context) error {
	return s.awaitMutationFreshnessForRepos(ctx)
}

// mutationCheckoutScope is metadata-only so receipts remain inspectable while
// the selected checkout has no materialized graph. An unselected request is
// canonical; sharing a repository prefix does not give it sibling receipts.
func mutationCheckoutScope(ctx context.Context) (checkoutID, incarnation string) {
	if state := checkoutMutationFromContext(ctx); state != nil {
		return state.checkoutID, state.incarnation
	}
	if control := checkoutControlFromContext(ctx); control != nil {
		if !control.CheckoutScoped {
			return "", ""
		}
		return control.Checkout.CheckoutID, control.Checkout.Incarnation
	}
	if view := requestViewFromContext(ctx); view != nil && view.rider != nil {
		return view.rider.CheckoutID, ""
	}
	return "", ""
}

func mutationCheckoutScopeMatches(checkoutID, incarnation, candidateID, candidateIncarnation string) bool {
	if checkoutID != candidateID {
		return false
	}
	return incarnation == "" || incarnation == candidateIncarnation
}

// mutationReceiptsForRepos selects the same coherent checkout view as the
// request. Unknown repository ownership remains in scope within that view.
func (s *Server) mutationReceiptsForRepos(ctx context.Context, repos ...string) []*mutationReceipt {
	checkoutID, incarnation := mutationCheckoutScope(ctx)
	repoScope := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo != "" {
			repoScope[repo] = struct{}{}
		}
	}

	var receipts []*mutationReceipt
	s.mutationReceipts.Range(func(_, value any) bool {
		receipt, ok := value.(*mutationReceipt)
		if !ok {
			return true
		}
		if !mutationCheckoutScopeMatches(checkoutID, incarnation, receipt.checkoutID, receipt.checkoutIncarnation) {
			return true
		}
		if len(repoScope) > 0 && receipt.repo != "" {
			if _, included := repoScope[receipt.repo]; !included {
				return true
			}
		}
		// Superseded content and failures covered by verified root recovery
		// remain historical receipts, not current-view freshness gaps. This
		// never marks an original result fresh; exact route readiness remains
		// the authority for the current view.
		receipt.mu.RLock()
		historical := receipt.checkoutScoped && receipt.completed && (errors.Is(receipt.result.Err, indexer.ErrCheckoutRefreshSuperseded) || receipt.barrierRecoveredGeneration > 0)
		receipt.mu.RUnlock()
		if historical {
			return true
		}
		receipts = append(receipts, receipt)
		return true
	})
	return receipts
}

// mutationFreshnessSummaryForRepos is a non-blocking version of the freshness
// barrier for partial diagnostic results. It uses the same checkout and repo
// scope as the waiting path, but never turns a pending ticket into a failure.
func (s *Server) mutationFreshnessSummaryForRepos(ctx context.Context, repos ...string) (pending bool, err error) {
	var failures []error
	for _, receipt := range s.mutationReceiptsForRepos(ctx, repos...) {
		outcome := receipt.outcome(true)
		if outcome.Pending {
			pending = true
			continue
		}
		if outcome.Err != nil {
			failures = append(failures, fmt.Errorf("failed receipt=%s path=%q: %w", receipt.id, receipt.path, outcome.Err))
		} else if !outcome.Reindexed {
			failures = append(failures, fmt.Errorf("failed receipt=%s path=%q: reindex not confirmed", receipt.id, receipt.path))
		}
	}
	return pending, errors.Join(failures...)
}

// awaitMutationFreshnessForRepos waits once, under one shared budget, for every
// selected ticket. On timeout it reports all pending and terminally failed
// generations rather than hiding the remaining gaps behind the first one.
func (s *Server) awaitMutationFreshnessForRepos(ctx context.Context, repos ...string) error {
	receipts := s.mutationReceiptsForRepos(ctx, repos...)
	if len(receipts) == 0 {
		return nil
	}
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].repo != receipts[j].repo {
			return receipts[i].repo < receipts[j].repo
		}
		if receipts[i].path != receipts[j].path {
			return receipts[i].path < receipts[j].path
		}
		if receipts[i].generation != receipts[j].generation {
			return receipts[i].generation < receipts[j].generation
		}
		return receipts[i].id < receipts[j].id
	})

	timer := time.NewTimer(s.mutationSafetyWaitDuration())
	defer timer.Stop()
	waitReason := ""
waitLoop:
	for _, receipt := range receipts {
		select {
		case <-receipt.done:
		case <-timer.C:
			waitReason = "wait budget expired"
			break waitLoop
		case <-ctx.Done():
			waitReason = "request cancelled: " + ctx.Err().Error()
			break waitLoop
		}
	}

	issues := make([]string, 0, len(receipts))
	hasTerminalFailure := false
	for _, receipt := range receipts {
		select {
		case <-receipt.done:
			outcome := receipt.outcome(false)
			switch {
			case outcome.Err != nil:
				hasTerminalFailure = true
				issues = append(issues, fmt.Sprintf(
					"failed receipt=%s repo=%q path=%q generation=%d error=%q",
					receipt.id, receipt.repo, receipt.path, receipt.generation, outcome.Err.Error()))
			case !outcome.Reindexed:
				hasTerminalFailure = true
				issues = append(issues, fmt.Sprintf(
					"failed receipt=%s repo=%q path=%q generation=%d error=%q",
					receipt.id, receipt.repo, receipt.path, receipt.generation, "reindex not confirmed"))
			}
		default:
			issues = append(issues, fmt.Sprintf(
				"pending receipt=%s repo=%q path=%q generation=%d",
				receipt.id, receipt.repo, receipt.path, receipt.generation))
		}
	}
	if len(issues) == 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("graph freshness wait cancelled: %w", err)
		}
		return nil
	}

	message := "graph freshness unavailable"
	if waitReason != "" {
		message += " (" + waitReason + ")"
	}
	for i, issue := range issues {
		if i == 0 {
			message += ": "
		} else {
			message += "; "
		}
		message += issue
	}
	if hasTerminalFailure {
		message += "; terminally failed generations do not recover by waiting — " +
			"they clear when a later mutation of the same path succeeds or when " +
			"the receipt retention lapses"
	}
	return fmt.Errorf("%s", message)
}

// mutationReposForSymbolIDs resolves a complete repository scope for a symbol
// request. Any unresolved input returns nil, which deliberately widens the
// barrier to all receipts rather than excluding an unknown mutation.
func (s *Server) mutationReposForSymbolIDs(ctx context.Context, ids []string) []string {
	repos := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		node := s.engineFor(ctx).GetSymbol(id)
		if node == nil || node.RepoPrefix == "" {
			return nil
		}
		repos[node.RepoPrefix] = struct{}{}
	}
	result := make([]string, 0, len(repos))
	for repo := range repos {
		result = append(result, repo)
	}
	sort.Strings(result)
	return result
}

func (s *Server) mutationReceiptState(id string) (mutationReindexOutcome, bool) {
	value, ok := s.mutationReceipts.Load(id)
	if !ok {
		return mutationReindexOutcome{}, false
	}
	receipt, ok := value.(*mutationReceipt)
	if !ok {
		return mutationReindexOutcome{}, false
	}
	select {
	case <-receipt.done:
		return receipt.outcome(false), true
	default:
		return receipt.outcome(true), true
	}
}

// attachMutationFreshness records mutually exclusive freshness states. Syntax
// health is only authoritative after completed reindex; reading it while a
// watcher patch is pending would surface stale parse errors and provoke an
// unnecessary source re-read.
func (s *Server) attachMutationFreshness(resp map[string]any, relPath, absPath string, outcome mutationReindexOutcome) {
	resp["reindexed"] = outcome.Reindexed
	// graph_status is the freshness half of the mutation contract, named so it
	// reads next to disk_status (mutation_commit.go) rather than having to be
	// inferred from the reindexed / reindex_pending / reindex_error triple.
	resp["graph_status"] = graphStatusFor(outcome)
	if outcome.Generation > 0 {
		resp["reindex_generation"] = outcome.Generation
	}
	if outcome.AppliedGeneration > 0 {
		resp["applied_generation"] = outcome.AppliedGeneration
	}
	if outcome.Receipt != "" {
		resp["reindex_receipt"] = outcome.Receipt
	}
	if outcome.Pending {
		resp["reindex_pending"] = true
		return
	}
	if outcome.Reindexed && !outcome.checkoutScoped {
		if health := s.fileSyntaxHealth(relPath, absPath); health != nil {
			resp["syntax_health"] = health
		}
	}
}
