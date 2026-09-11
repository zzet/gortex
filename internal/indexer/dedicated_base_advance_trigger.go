package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
)

// The live committed-base advancement trigger.
//
// W4.2 made a committed base exist: one publication per dedicated repository
// per daemon start, plus an advance for a tree that moved while the process
// was down. Nothing advanced it while the daemon was UP, so a running daemon's
// committed base aged out the moment somebody committed.
//
// This file is the live half. It has exactly one source — the Git watcher's
// HEAD-change finalize path (GitWatcher.finalizeReconcile) — and it deliberately
// has no others:
//
//   - polling must never allocate a generation. The 15 s checkout-coordinator
//     poll, the hourly janitor and the filesystem poller (poller.go) all run on
//     timers whose firing says nothing about whether anything moved; wiring any
//     of them here would allocate a payload generation for the passage of time.
//     The Git watcher fires on a ref transition, and finalizeReconcile is
//     reached only after `oldSHA != newSHA` — an observed movement, not a tick.
//   - the non-Git Watcher path must keep writing generation 0 only. Both of its
//     IncrementalReindexPaths entry points (watcher.go's overflow reconcile and
//     incremental_watcher_batch.go's storm batch) describe working-copy edits,
//     which are not committed content and have no committed tree to publish.
//     They are not wired here and must not be.
//
// Advancement is not activation. Adoption moves
// `dedicated_graphs.active_generation_id` so DEPENDENT checkouts key on an
// immutable lower snapshot; the owning repository's own request route stays on
// legacy generation 0 until W4.5.

// dedicatedBaseAdvanceRegistry binds one daemon's watchers to that daemon's
// trigger, keyed on the MultiIndexer.
//
// The two halves are built by sites that do not know about each other. A
// GitWatcher is constructed by MultiWatcher, which holds a *MultiIndexer and
// nothing else — no catalog, no CheckoutLifecycle, no publication runtime. The
// trigger is constructed by the committed-base publisher, which holds the
// lifecycle and the runtime but never sees a watcher. The MultiIndexer is the
// one object both sides already hold: every Indexer carries it as
// repositoryMutationOwner, and a CheckoutLifecycle is built over it.
//
// Keying on that pointer is therefore a binding, not a global: two daemons (or
// two test fixtures) in one process each have their own MultiIndexer and
// resolve their own trigger, and a fixture that drops its stack leaves no
// entry behind because Close removes it.
var dedicatedBaseAdvanceRegistry sync.Map // *MultiIndexer -> *DedicatedBaseAdvanceTrigger

// dedicatedBaseAdvanceTriggerFor resolves the trigger bound to one process's
// repositories, or nil when there is none to reach: no committed-base publisher
// is installed (a non-sqlite backend, or a stack with no CheckoutLifecycle at
// all), or the one that was has stopped.
//
// "Has stopped" covers both stops, and covering the second here rather than
// only in Close is what makes shutdown ordering a property of the lookup. The
// daemon does not call InitialBasePublisher.Close: it closes publisher
// admission on the RUNTIME, which cancels every registered driver's context
// (dedicated_base_runtime.go) and is the first thing CheckoutLifecycle.Close
// does — while the watchers keep running until MultiWatcher.Stop joins them.
// A watcher that observes a HEAD change inside that window must find no
// trigger, not one whose publications can never run.
func dedicatedBaseAdvanceTriggerFor(mi *MultiIndexer) *DedicatedBaseAdvanceTrigger {
	if mi == nil {
		return nil
	}
	value, ok := dedicatedBaseAdvanceRegistry.Load(mi)
	if !ok {
		return nil
	}
	trigger, _ := value.(*DedicatedBaseAdvanceTrigger)
	if !trigger.live() {
		return nil
	}
	return trigger
}

// live reports whether this trigger can still publish.
func (t *DedicatedBaseAdvanceTrigger) live() bool {
	if t == nil || t.publisher == nil || t.publisher.ctx == nil {
		return false
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	return !closed && t.publisher.ctx.Err() == nil
}

// DedicatedBaseAdvance is one live advancement outcome.
type DedicatedBaseAdvance struct {
	RepoPrefix string
	CommitOID  string
	TreeOID    string
	// GenerationID is the generation that is active after the advance: a newly
	// published delta (or root), or the one that was already active when the
	// observed commit named a tree the base already describes.
	GenerationID int64
	// Published is true when this advance allocated and adopted a NEW
	// generation. A same-tree commit re-adopts and reports false.
	Published bool
	// Coalesced is true when no physical build ran — the claim resolved to a
	// generation that already describes this identity.
	Coalesced bool
	Skipped   string
	Err       error
}

// DedicatedBaseAdvanceTrigger turns an observed HEAD movement into committed
// advancement.
//
// It owns no queue of its own: requests go into the publisher's single pending
// list, so a startup publication and a live advance for the same graph can
// never reach two concurrent physical builds of the same tree. What it does own
// is the accepted-commit memo that keeps a repeat observation of the SAME
// commit from paying for a fresh observation (a daemon-wide roster lease plus a
// catalog read per cohort member) for a movement that already landed.
type DedicatedBaseAdvanceTrigger struct {
	publisher *InitialBasePublisher
	logger    *zap.Logger

	mu sync.Mutex
	// accepted memoises, per repository, the commit whose publication
	// SUCCEEDED. A dispatch naming it again is dropped as a repeat; a failed
	// publication leaves no memo, so the next observation retries.
	accepted map[string]string
	advances []DedicatedBaseAdvance
	closed   bool
}

// newDedicatedBaseAdvanceTrigger builds the trigger for one publisher and
// registers it for that publisher's repositories.
func newDedicatedBaseAdvanceTrigger(publisher *InitialBasePublisher) *DedicatedBaseAdvanceTrigger {
	if publisher == nil || publisher.lifecycle == nil || publisher.lifecycle.mi == nil {
		return nil
	}
	trigger := &DedicatedBaseAdvanceTrigger{
		publisher: publisher,
		logger:    publisher.logger,
		accepted:  map[string]string{},
	}
	dedicatedBaseAdvanceRegistry.Store(publisher.lifecycle.mi, trigger)
	return trigger
}

// close drops the registry binding. It is idempotent, and it removes the entry
// only when this trigger is still the registered one, so a fixture that builds
// a second publisher over the same MultiIndexer does not have its live trigger
// unregistered by the first publisher's teardown.
func (t *DedicatedBaseAdvanceTrigger) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	if t.publisher != nil && t.publisher.lifecycle != nil && t.publisher.lifecycle.mi != nil {
		dedicatedBaseAdvanceRegistry.CompareAndDelete(t.publisher.lifecycle.mi, t)
	}
}

// HeadChanged reports that one working copy's resolved HEAD has moved to
// commitOID, and that the move has been reconciled into the generation-0
// corpus.
//
// It does two things, in this order and both cheaply:
//
//  1. It marks every dependency cohort that could name this repository's bytes
//     as needing a fresh description. A checkout coordinator and a ref-view
//     manager both CACHE the cohort their generation identities are keyed on,
//     and this is the one source the lifecycle could not provide for itself:
//     a workspace sibling's committed tree moving is not a lifecycle event.
//     The mark does not depend on publication succeeding, or on this
//     repository having a committed base at all: it is what the observation
//     MEANS, not a consequence of what the publisher does with it.
//  2. It queues one committed advance for the repository, coalescing to the
//     newest observed commit.
//
// It never blocks and it never publishes on the caller's goroutine: the caller
// is the Git watcher's reconcile goroutine, and the work behind this call is a
// bounded index of a committed tree.
func (t *DedicatedBaseAdvanceTrigger) HeadChanged(repoPrefix, root, commitOID string) {
	if t == nil || repoPrefix == "" || commitOID == "" {
		return
	}
	p := t.publisher
	if p == nil || p.lifecycle == nil {
		return
	}
	// A repeat observation of the commit that already landed is dropped before
	// anything else: nothing moved, so no cohort went stale and there is no
	// advance to queue. Everything below is for a movement.
	t.mu.Lock()
	stopped := t.closed
	repeat := t.accepted[repoPrefix] == commitOID
	t.mu.Unlock()
	if stopped || repeat {
		return
	}
	p.lifecycle.invalidateDependencyCohortsForPrefix(repoPrefix,
		fmt.Sprintf("git watcher: %s advanced to %s", repoPrefix, shortCommit(commitOID)))

	request := basePublishRequest{
		prefix: repoPrefix,
		root:   root,
		target: dedicatedBaseTarget{CommitOID: commitOID},
		done: func(outcome InitialBasePublication) {
			t.record(commitOID, outcome)
		},
	}
	if !p.enqueueAdvance(request) && t.logger != nil {
		t.logger.Debug("git-watcher: committed-base advancement is stopped; the base stays where it is",
			zap.String("repo", repoPrefix), zap.String("commit", shortCommit(commitOID)))
	}
}

// AdvanceRepo is HeadChanged without the queue: it publishes one advance
// synchronously and returns its outcome. Production dispatches through
// HeadChanged; this is the entry point for a caller that already owns the
// waiting — a one-shot server, or a test.
func (t *DedicatedBaseAdvanceTrigger) AdvanceRepo(ctx context.Context, repoPrefix, root, commitOID string) DedicatedBaseAdvance {
	out := DedicatedBaseAdvance{RepoPrefix: repoPrefix, CommitOID: commitOID}
	if t == nil || t.publisher == nil {
		out.Skipped = "no advancement trigger"
		return out
	}
	if repoPrefix == "" || commitOID == "" {
		out.Skipped = "incomplete head observation"
		return out
	}
	if ctx == nil {
		ctx = t.publisher.ctx
	}
	t.publisher.lifecycle.invalidateDependencyCohortsForPrefix(repoPrefix,
		fmt.Sprintf("git watcher: %s advanced to %s", repoPrefix, shortCommit(commitOID)))
	outcome := t.publisher.publish(ctx, basePublishRequest{
		prefix: repoPrefix, root: root, live: true,
		target: dedicatedBaseTarget{CommitOID: commitOID},
	})
	return t.record(commitOID, outcome)
}

// record turns one publication outcome into an advance, memoising the commit
// when the publication settled.
func (t *DedicatedBaseAdvanceTrigger) record(commitOID string, outcome InitialBasePublication) DedicatedBaseAdvance {
	advance := DedicatedBaseAdvance{
		RepoPrefix:   outcome.RepoPrefix,
		CommitOID:    commitOID,
		TreeOID:      outcome.TreeOID,
		GenerationID: outcome.GenerationID,
		// A re-adoption is not a publication: the observed commit named a tree
		// the active base already describes (an amend with no content change,
		// a commit that only moved a file the index excludes).
		Published: outcome.Err == nil && outcome.Skipped == "" && !outcome.AlreadyAdopted,
		Coalesced: outcome.Coalesced,
		Skipped:   outcome.Skipped,
		Err:       outcome.Err,
	}
	t.mu.Lock()
	if advance.Err == nil && advance.Skipped == "" {
		t.accepted[advance.RepoPrefix] = commitOID
	}
	t.advances = append(t.advances, advance)
	t.mu.Unlock()
	return advance
}

// Advances reports what the trigger has done so far, newest last.
func (t *DedicatedBaseAdvanceTrigger) Advances() []DedicatedBaseAdvance {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]DedicatedBaseAdvance, len(t.advances))
	copy(out, t.advances)
	return out
}

// Wait blocks until every queued advance (and every queued startup
// publication) has been attempted. It shares the publisher's accounting
// because it shares the publisher's queue.
func (t *DedicatedBaseAdvanceTrigger) Wait(ctx context.Context) error {
	if t == nil || t.publisher == nil {
		return nil
	}
	return t.publisher.Wait(ctx)
}

// dedicatedBaseCommitTree resolves the tree a commit names.
//
// This is the one fact the catalog cannot supply on the live path: the
// checkout row's head_tree is written by the reconciler's family pass, not by
// the ref transition, so at the instant of the observation it still names the
// previous tree. Resolving it from Git is also what makes a same-tree commit
// (an amend with no content change, a message-only rewrite) cost nothing: the
// identity it produces equals the active base's, so publication takes the
// catalog's adopted-replay path and writes nothing.
func dedicatedBaseCommitTree(ctx context.Context, root, commitOID string) (string, error) {
	if root == "" || commitOID == "" {
		return "", fmt.Errorf("%w: resolving a committed tree requires a root and a commit", errInitialBasePublisherInput)
	}
	tree, err := gitcmd.Output(ctx, root, "rev-parse", commitOID+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve the tree of %s in %s: %w", shortCommit(commitOID), root, err)
	}
	tree = strings.TrimSpace(tree)
	if tree == "" {
		return "", fmt.Errorf("%w: commit %s names no tree", errInitialBasePublisherInput, shortCommit(commitOID))
	}
	return tree, nil
}

// sameCheckoutRoot decides whether two paths name the same working copy. An
// empty observed root is not a match: a caller that cannot say which working
// copy it watched cannot be trusted to have watched the owner's.
//
// The lexical comparison is not enough on its own. A watcher root is
// filepath.Abs of whatever the caller was given, while a checkout row's root
// comes from Git's own inventory, and the two differ by symlink resolution on
// any platform where the tracked path crosses one (/var -> /private/var on
// macOS is the common case). Falling through to identity-by-inode rather than
// to a refusal is what keeps that from silently disabling advancement.
func sameCheckoutRoot(observed, owner string) bool {
	if observed == "" || owner == "" {
		return false
	}
	if filepath.Clean(observed) == filepath.Clean(owner) {
		return true
	}
	observedInfo, err := os.Stat(observed)
	if err != nil {
		return false
	}
	ownerInfo, err := os.Stat(owner)
	if err != nil {
		return false
	}
	return os.SameFile(observedInfo, ownerInfo)
}

func shortCommit(commitOID string) string {
	if len(commitOID) > 12 {
		return commitOID[:12]
	}
	return commitOID
}
