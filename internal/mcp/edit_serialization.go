package mcp

import (
	"context"
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
// mutation. A receipt is present only when the bounded request-path wait ended
// before the watcher ticket did.
type mutationReindexOutcome struct {
	Reindexed         bool
	Pending           bool
	Receipt           string
	Generation        uint64
	AppliedGeneration uint64
	Err               error
}

type mutationReceipt struct {
	id         string
	repo       string
	path       string
	generation uint64
	done       chan struct{}
	mu         sync.RWMutex
	result     indexer.MutationResult
	completed  bool
}

type mutationScheduler interface {
	EnqueueFileMutation(context.Context, string) (*indexer.MutationTicket, error)
}

// acquireMutationPath waits for exclusive mutation access to path. Waiting is
// context-aware: a cancelled MCP request leaves the queue immediately, which
// lets its dispatcher goroutine finish and release admission capacity.
func acquireMutationPath(ctx context.Context, path string) (func(), error) {
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
	receipt := &mutationReceipt{
		id:         fmt.Sprintf("mutation-%d", mutationReceiptSequence.Add(1)),
		repo:       repo,
		path:       ticket.Path,
		generation: ticket.Generation,
		done:       make(chan struct{}),
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
		if result.Err == nil && result.Reindexed {
			s.resolveSupersededFailedReceipts(receipt.path, receipt.generation, result)
		}
		close(receipt.done)
		time.AfterFunc(mutationReceiptRetention, func() {
			s.mutationReceipts.Delete(receipt.id)
		})
	}()
	return receipt
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
		if !ok {
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
		if !ok || filepath.Clean(receipt.path) != candidate {
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
		if !ok || filepath.Clean(receipt.path) != cleanPath {
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
		Pending:    pending,
		Receipt:    r.id,
		Generation: r.generation,
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

// awaitMutationFreshnessForRepos waits once, under one shared budget, for every
// ticket in the requested repositories. Receipts with unknown ownership remain
// in scope so incomplete metadata cannot disarm the safety gate. On timeout it
// reports every pending and terminally failed generation, not only the first.
func (s *Server) awaitMutationFreshnessForRepos(ctx context.Context, repos ...string) error {
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
		if len(repoScope) > 0 && receipt.repo != "" {
			if _, included := repoScope[receipt.repo]; !included {
				return true
			}
		}
		receipts = append(receipts, receipt)
		return true
	})
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
	if outcome.Reindexed {
		if health := s.fileSyntaxHealth(relPath, absPath); health != nil {
			resp["syntax_health"] = health
		}
	}
}
