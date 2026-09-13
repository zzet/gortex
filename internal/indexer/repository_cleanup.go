package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

// ErrRepositoryCleanupPending means logical withdrawal is committed but a
// captured reader/producer still owns payload. ReleaseGraph must return it;
// returning nil would authorize the reconciler to delete the graph too early.
var ErrRepositoryCleanupPending = errors.New("indexer: repository cleanup is pending")

// Suppress only a pure ownership deferral. errors.Is alone would incorrectly
// turn errors.Join(Pending, journalWriteFailure) into a successful public result.
func onlyRepositoryCleanupPending(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyRepositoryCleanupPending(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyRepositoryCleanupPending(wrapped.Unwrap())
	}
	return err == ErrRepositoryCleanupPending
}

func repositoryCleanupUntrackResult(out UntrackResult, err error) (UntrackResult, error) {
	if errors.Is(err, ErrRepositoryCleanupPending) {
		out.Pending = true
	}
	if onlyRepositoryCleanupPending(err) {
		return out, nil
	}
	return out, err
}

type repositoryCleanupPayload interface {
	WaitPayloadBuildFlights(context.Context, ...int64) error
	PayloadBuildFlightActive(int64) bool
	RetirePayloadGeneration(context.Context, int64, func(int64) bool) error
}

type repositoryCleanupState struct {
	mu            sync.Mutex
	graphID       string
	identity      store_sqlite.RepositoryCleanupIdentity
	owner         *graphview.RepositoryDrain
	refs          *repositoryRefViewDrain
	lane          *repositoryCleanupLane
	publisher     <-chan struct{}
	initialized   bool
	producersDone chan struct{}
	attemptDone   chan struct{}
	attemptOnce   sync.Once
	waitReady     chan struct{}
	payloadPurged bool
	finalized     bool
}

type repositoryCleanupRuntime struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	done    chan struct{}
	waiters sync.WaitGroup
	states  map[string]*repositoryCleanupState
	closed  bool
}

func (r *repositoryCleanupRuntime) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func channelClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (l *CheckoutLifecycle) repositoryCleanupRuntime() (*repositoryCleanupRuntime, error) {
	l.repositoryCleanupMu.Lock()
	defer l.repositoryCleanupMu.Unlock()
	if l.repositoryCleanupClosed {
		return nil, graphview.ErrRepositoryAdmissionsStopped
	}
	if l.repositoryCleanup != nil {
		return l.repositoryCleanup, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &repositoryCleanupRuntime{ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), states: make(map[string]*repositoryCleanupState)}
	l.repositoryCleanup = runtime
	go l.runRepositoryCleanup(runtime)
	return runtime, nil
}

func (l *CheckoutLifecycle) pendingRepositoryCleanup(ctx context.Context, graphID string) (*repositoryCleanupRuntime, *repositoryCleanupState, bool, error) {
	runtime, err := l.repositoryCleanupRuntime()
	if err != nil {
		return nil, nil, false, err
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return nil, nil, false, graphview.ErrRepositoryAdmissionsStopped
	}
	state := runtime.states[graphID]
	if state == nil {
		state = &repositoryCleanupState{graphID: graphID, attemptDone: make(chan struct{}), waitReady: make(chan struct{}), producersDone: make(chan struct{})}
		runtime.states[graphID] = state
	}
	runtime.mu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.initialized {
		return runtime, state, true, nil
	}
	identity, owner, found, err := l.closeRepositoryAdmission(ctx, graphID)
	if err != nil {
		return runtime, state, found, err
	}
	if !found {
		runtime.mu.Lock()
		if runtime.states[graphID] == state {
			delete(runtime.states, graphID)
		}
		runtime.mu.Unlock()
		return runtime, nil, false, nil
	}
	state.identity, state.owner = identity, owner
	state.publisher, err = l.closeRepositoryPublisher(identity, owner)
	if err != nil {
		return runtime, state, true, err
	}
	state.refs = l.closeRepositoryRefViews(identity.RepoPrefix)
	state.lane, err = l.mi.beginRepositoryCleanupLane(identity.RepoPrefix)
	if err != nil {
		return runtime, state, true, err
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return nil, nil, false, graphview.ErrRepositoryAdmissionsStopped
	}
	state.initialized = true
	runtime.waiters.Add(1)
	runtime.mu.Unlock()
	go l.waitRepositoryCleanup(runtime, state)
	return runtime, state, true, nil
}

// There is at most one owned waiter per closing graph. It has no request
// context and is joined at shutdown. Ordinary queries create no waiter, timer,
// catalog write or retry task. The MI WaitGroup drain itself is cached.
func (l *CheckoutLifecycle) waitRepositoryCleanup(runtime *repositoryCleanupRuntime, state *repositoryCleanupState) {
	defer runtime.waiters.Done()
	defer func() { close(state.waitReady); runtime.signal() }()
	// Close actors present before the owner fence, then wait for constructor
	// admissions and take a second snapshot. A constructor records its actor
	// before releasing its owner pin, so it cannot escape both snapshots.
	l.closeRepositoryCoordinators(state.identity.RepoPrefix)
	for _, done := range []<-chan struct{}{state.publisher, state.lane.done, state.refs.done, state.owner.Done()} {
		select {
		case <-done:
		case <-runtime.ctx.Done():
			return
		}
	}
	l.closeRepositoryCoordinators(state.identity.RepoPrefix)
	close(state.producersDone)
	ids, err := l.catalog.ListRepositoryCleanupGenerations(runtime.ctx, state.identity.GraphID)
	if err != nil {
		return
	} // The actual retry reports/retains this error.
	if err := l.leases.WaitDrain(runtime.ctx, ids...); err != nil {
		return
	}
	if payload, ok := l.mi.graph.(repositoryCleanupPayload); ok {
		_ = payload.WaitPayloadBuildFlights(runtime.ctx, ids...)
	}
}

// The OUTERMOST saga invokes this captured completion after its final journal
// write. Different cleanup IDs may finish across a same-identity re-registration;
// an old callback therefore must not look up and signal the replacement state.
func (h cleanupHooks) BeginGraphCleanupAttempt(ctx context.Context, graphID string) (func(), error) {
	runtime, state, _, err := h.l.pendingRepositoryCleanup(ctx, graphID)
	if runtime == nil || state == nil {
		return nil, err
	}
	return func() {
		runtime.mu.Lock()
		changed := false
		if runtime.states[state.graphID] == state {
			state.attemptOnce.Do(func() { close(state.attemptDone); changed = true })
		}
		runtime.mu.Unlock()
		if changed {
			runtime.signal()
		}
	}, err
}

func (h cleanupHooks) BeginGraphCleanup(ctx context.Context, graphID string) error {
	_, err := h.BeginGraphCleanupAttempt(ctx, graphID)
	return err
}

func (l *CheckoutLifecycle) releaseRepositoryGraph(ctx context.Context, graphID string) error {
	_, state, found, err := l.pendingRepositoryCleanup(ctx, graphID)
	if err != nil || !found {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.payloadPurged {
		return nil
	}
	if !channelClosed(state.producersDone) {
		return fmt.Errorf("%w: graph %s", ErrRepositoryCleanupPending, graphID)
	}
	ids, err := l.catalog.ListRepositoryCleanupGenerations(ctx, graphID)
	if err != nil {
		return err
	}
	payload, hasPayload := l.mi.graph.(repositoryCleanupPayload)
	if len(ids) != 0 && !hasPayload {
		return fmt.Errorf("indexer: graph %s cleanup backend cannot retire positive generations", graphID)
	}
	for _, id := range ids {
		if l.leases.InUse(id) || payload.PayloadBuildFlightActive(id) {
			return fmt.Errorf("%w: graph %s generation %d", ErrRepositoryCleanupPending, graphID, id)
		}
	}
	if err := l.catalog.ReleaseRepositoryCleanupPublication(ctx, state.identity); err != nil {
		return err
	}
	// Newest-first retirement removes child references before parents. Its
	// existing atomic fence also catches an allocated-before-Join late owner.
	for _, id := range ids {
		if err := payload.RetirePayloadGeneration(ctx, id, l.leases.InUse); err != nil && !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			// A sweep that stopped on its own budget is a YIELD, not a
			// failure: the generation keeps its retiring fence, its rows are
			// a strict subset of what they were, and the next pass resumes
			// from the chunk this one stopped on
			// (store_sqlite/payload_generation_sweep.go:72-85, which names
			// this call site as the one caller that could not resume it).
			// Reported as any other error it would reach reconcile/saga.go,
			// which skips DeleteDedicatedGraph and turns a slow untrack into
			// a failed one; reported as pending it goes back on the cleanup
			// runtime's retry timer and the sweep continues.
			//
			// The sentinel is the whole chain rather than one arm of a join:
			// onlyRepositoryCleanupPending (:22-42) suppresses the public
			// error only for a pure pending chain, so joining the store's
			// sentinel here would make a yield a hard untrack error again.
			// The yield's own text is carried instead.
			if errors.Is(err, store_sqlite.ErrPayloadSweepBudgetExhausted) {
				return fmt.Errorf("%w: graph %s generation %d retirement yielded on its sweep budget: %v",
					ErrRepositoryCleanupPending, graphID, id, err)
			}
			return err
		}
	}
	l.detachWatcherContext(ctx, state.identity.RepoPrefix)
	finalize := l.repositoryConfigFinalizer(state.identity.RootPath)
	_, _, err = l.mi.purgeRepoForCleanup(ctx, state.identity.RepoPrefix, finalize)
	if l.mi.GetMetadata(state.identity.RepoPrefix) == nil {
		l.notifyTrackedSetChanged()
	}
	if err != nil {
		return err
	}
	state.payloadPurged = true
	return nil
}

func (l *CheckoutLifecycle) repositoryConfigFinalizer(rootPath string) func(*RepoMetadata) error {
	return func(meta *RepoMetadata) error {
		if l.cfgMgr == nil {
			return nil
		}
		path := rootPath
		if meta != nil && meta.RootPath != "" {
			path = meta.RootPath
		}
		if path == "" {
			return nil
		}
		_, err := l.cfgMgr.Global().RemoveRepoAndSaveIfPresent(path)
		return err
	}
}

func (r *repositoryCleanupRuntime) actionable() bool {
	r.mu.Lock()
	states := make([]*repositoryCleanupState, 0, len(r.states))
	for _, state := range r.states {
		states = append(states, state)
	}
	r.mu.Unlock()
	for _, state := range states {
		state.mu.Lock()
		ready := channelClosed(state.attemptDone) && (!state.initialized || channelClosed(state.waitReady))
		state.mu.Unlock()
		if ready {
			return true
		}
	}
	return false
}

func (l *CheckoutLifecycle) runRepositoryCleanup(runtime *repositoryCleanupRuntime) {
	defer close(runtime.done)
	var timer *time.Timer
	var retry <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-runtime.ctx.Done():
			return
		case <-runtime.wake:
		case <-retry:
		}
		if runtime.ctx.Err() != nil {
			return
		}
		if !runtime.actionable() {
			continue
		}
		// Retry the existing journal, not a second independent cleanup engine.
		resumeErr := l.rec.Resume(runtime.ctx)
		transitionErr := l.resumeModeTransitions(runtime.ctx)
		pending, finishErr := l.finalizeRepositoryCleanups(runtime.ctx, "")
		if runtime.ctx.Err() != nil {
			return
		}
		if err := errors.Join(resumeErr, transitionErr, finishErr); err != nil && !onlyRepositoryCleanupPending(err) {
			l.logger.Warn("repository cleanup remains pending", zap.Error(err))
		}
		if pending || resumeErr != nil || transitionErr != nil || finishErr != nil {
			// One bounded repair timer for I/O failures or a completing transition.
			// Reader/producer drain normally wakes promptly without polling SQL.
			if timer == nil {
				timer = time.NewTimer(time.Second)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(time.Second)
			}
			retry = timer.C
		} else {
			if timer != nil {
				timer.Stop()
			}
			retry = nil
		}
	}
}

// A prefix filter lets public Untrack finalize only its own successful result;
// an unrelated held repository must not make that result spuriously pending.
func (l *CheckoutLifecycle) finalizeRepositoryCleanups(ctx context.Context, prefix string) (bool, error) {
	l.repositoryCleanupMu.Lock()
	runtime := l.repositoryCleanup
	l.repositoryCleanupMu.Unlock()
	if runtime == nil {
		return false, nil
	}
	runtime.mu.Lock()
	states := make([]*repositoryCleanupState, 0, len(runtime.states))
	for _, state := range runtime.states {
		states = append(states, state)
	}
	runtime.mu.Unlock()
	var pending bool
	var errs []error
	for _, state := range states {
		state.mu.Lock()
		matches := prefix == "" || state.identity.RepoPrefix == prefix
		state.mu.Unlock()
		if !matches {
			continue
		}
		finished, err := l.finalizeRepositoryCleanupState(ctx, state)
		if err != nil {
			errs = append(errs, err)
		}
		if !finished {
			pending = true
			continue
		}
		runtime.mu.Lock()
		if runtime.states[state.graphID] == state {
			delete(runtime.states, state.graphID)
		}
		if len(runtime.states) == 0 {
			runtime.states = make(map[string]*repositoryCleanupState)
		}
		runtime.mu.Unlock()
	}
	return pending, errors.Join(errs...)
}

func (l *CheckoutLifecycle) finalizeRepositoryCleanupState(ctx context.Context, state *repositoryCleanupState) (bool, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finalized {
		return true, nil
	}
	if !state.payloadPurged || !channelClosed(state.attemptDone) {
		return false, nil
	}
	identity := state.identity
	if _, found, err := l.catalog.GetDedicatedGraph(ctx, identity.GraphID); err != nil || found {
		return false, err
	}
	pending, err := l.rec.RepositoryCleanupPending(ctx, identity.GraphID, identity.CheckoutID, identity.Incarnation, identity.FamilyID)
	if err != nil || pending {
		return false, err
	}
	if err := l.finalizeRepositoryRefViews(state.refs); err != nil {
		return false, err
	}
	if err := l.mi.finalizeRepositoryCleanupLane(state.lane); err != nil {
		return false, err
	}
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	if l.repositoryOwners[identity.GraphID] != repositoryCleanupOwner(identity) {
		return false, graphview.ErrRepositoryOwnerConflict
	}
	if state.owner == nil || l.repositoryClosing[identity.GraphID] != state.owner {
		return false, graphview.ErrRepositoryDrainInvalid
	}
	// This bounded callback uses the same lock as publisher registration and
	// closure. Its exact channel capability also fences direct runtime owner
	// replacement; a matching owner tuple is not sufficient authority.
	if l.dedicatedBaseCleanupRuntime != nil {
		owner := store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation}
		if err := l.dedicatedBaseCleanupRuntime.FinalizeDedicatedBaseOwner(identity.GraphID, owner, state.publisher); err != nil {
			return false, err
		}
	}
	if err := l.leases.FinalizeRepositoryCleanup(state.owner); err != nil {
		return false, err
	}
	delete(l.repositoryOwners, identity.GraphID)
	delete(l.repositoryClosing, identity.GraphID)
	if len(l.repositoryOwners) == 0 {
		l.repositoryOwners = nil
	}
	if len(l.repositoryClosing) == 0 {
		l.repositoryClosing = nil
	}
	state.finalized = true
	return true, nil
}

func (l *CheckoutLifecycle) closeRepositoryCleanup() {
	l.repositoryCleanupMu.Lock()
	l.repositoryCleanupClosed = true
	runtime := l.repositoryCleanup
	l.repositoryCleanupMu.Unlock()
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	runtime.closed = true
	runtime.cancel()
	runtime.mu.Unlock()
	<-runtime.done
	runtime.waiters.Wait()
}
