package indexer

import (
	"path/filepath"
	"time"
)

// MutationTierState is the publication evidence for one admitted path. An
// absent timestamp is unobserved, rather than evidence of a fresh tier.
type MutationTierState struct {
	Generation        uint64    `json:"generation"`
	AckAt             time.Time `json:"ack_at"`
	FileGraphAt       time.Time `json:"file_graph_at,omitempty"`
	RelationsAt       time.Time `json:"relations_at,omitempty"`
	DerivedAt         time.Time `json:"derived_at,omitempty"`
	Failed            bool      `json:"failed,omitempty"`
	Partial           bool      `json:"partial,omitempty"`
	failedAt          time.Time
	rebindRefused     bool
	nonRefusalPartial bool
}

func newMutationTierTicket(path string, generation uint64, done <-chan MutationResult) *MutationTicket {
	return &MutationTicket{Path: path, Generation: generation, Done: done,
		tiers: MutationTierState{Generation: generation, AckAt: time.Now()}}
}

func (w *Watcher) clearRefusedFailedPathLocked(path string, at time.Time) {
	prior, ok := w.failedPaths[path]
	if !ok || !prior.rebindRefused {
		return
	}
	if !prior.Failed && !prior.nonRefusalPartial {
		delete(w.failedPaths, path)
		return
	}
	prior.rebindRefused = false
	prior.Partial = prior.nonRefusalPartial
	if !prior.Failed {
		stampRelations(&prior, at)
	}
	w.failedPaths[path] = prior
}

func (w *Watcher) markRelations(path string, generation, repairedGeneration uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	at := time.Now()
	if repairedGeneration != 0 && w.failedPaths[path].Generation <= repairedGeneration {
		w.clearRefusedFailedPathLocked(path, at)
	}
	for ordinal, waiter := range w.mutationWaiters[path] {
		if ordinal <= generation && waiter.ticket != nil {
			waiter.ticket.tierMu.Lock()
			if repairedGeneration != 0 && ordinal <= repairedGeneration && waiter.ticket.tiers.rebindRefused {
				waiter.ticket.tiers.rebindRefused = false
				waiter.ticket.tiers.Partial = waiter.ticket.tiers.nonRefusalPartial
			}
			stampRelations(&waiter.ticket.tiers, at)
			waiter.ticket.tierMu.Unlock()
		}
	}
}

func (w *Watcher) markRepairedFailedPaths(absPaths []string, all bool) {
	if w == nil || (!all && len(absPaths) == 0) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.failedPaths) == 0 {
		return
	}
	at := time.Now()
	if all {
		for p, prior := range w.failedPaths {
			if prior.rebindRefused && len(w.mutationWaiters[p]) == 0 {
				w.clearRefusedFailedPathLocked(p, at)
			}
		}
		return
	}
	for _, p := range absPaths {
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if prior, ok := w.failedPaths[clean]; ok && prior.rebindRefused && len(w.mutationWaiters[clean]) == 0 {
			w.clearRefusedFailedPathLocked(clean, at)
		}
	}
}

func stampRelations(state *MutationTierState, at time.Time) {
	if state.FileGraphAt.IsZero() {
		state.FileGraphAt = at
	}
	if !state.rebindRefused && state.RelationsAt.IsZero() {
		state.RelationsAt = at
	}
}

func (w *Watcher) mutationTierFrontier(paths []string) map[string]uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	frontier := make(map[string]uint64, len(paths))
	for _, path := range paths {
		frontier[path] = w.pendingGeneration[path]
	}
	return frontier
}

func (w *Watcher) markRelationsFrontier(frontier map[string]uint64) {
	for path, generation := range frontier {
		w.markRelations(path, generation, generation)
	}
}

// finishMutationTierLocked runs before removing a waiter or delivering Done.
// Successful paths retain no watcher history; the caller's ticket owns stamps.
// It returns true when the ticket completed cleanly (err == nil && !Partial).
func (w *Watcher) finishMutationTierLocked(path string, ticket *MutationTicket, err error, fanout DerivedFanoutCompleteness) bool {
	if ticket == nil {
		return false
	}
	ticket.tierMu.Lock()
	defer ticket.tierMu.Unlock()
	state := &ticket.tiers
	if err != nil {
		state.Failed = true
		state.failedAt = time.Now()
		w.retainMutationTierFailureLocked(path, *state)
		return false
	}
	at := time.Now()
	deferred := state.Partial
	if !deferred {
		stampRelations(state, at) // identity no-ops and test executors have no tail
		state.DerivedAt = at
	} else if state.FileGraphAt.IsZero() {
		state.FileGraphAt = at
	}
	if fanout.Observed && !fanout.Complete {
		state.nonRefusalPartial = true
	}
	state.Partial = deferred || state.nonRefusalPartial
	if state.Partial {
		state.failedAt = at
		w.retainMutationTierFailureLocked(path, *state)
		return false
	}
	if prior := w.failedPaths[path]; !prior.rebindRefused && prior.Generation <= state.Generation {
		delete(w.failedPaths, path)
	}
	return true
}

func (w *Watcher) retainMutationTierFailureLocked(path string, state MutationTierState) {
	prior := w.failedPaths[path]
	// A checked refusal is retained by the point publisher. Its later
	// completion cannot restore the gap after a newer point publishes.
	if prior.Generation > state.Generation || (state.rebindRefused && !prior.rebindRefused) {
		return
	}
	state.rebindRefused = prior.rebindRefused
	if w.failedPaths == nil {
		w.failedPaths = make(map[string]MutationTierState)
	}
	w.failedPaths[path] = state
}

// TierPaths returns outstanding paths and unrepaired per-path failures.
// Receipt expiry is not content evidence; only a successful patch retires gaps.
// It shares the waiter's existing path index; idle reads allocate no map.
func (w *Watcher) TierPaths() map[string]MutationTierState {
	w.mu.Lock()
	defer w.mu.Unlock()
	var states map[string]MutationTierState
	put := func(path string, state MutationTierState) {
		if states == nil {
			states = make(map[string]MutationTierState)
		}
		if states[path].Generation <= state.Generation {
			states[path] = state
		}
	}
	for path, state := range w.failedPaths {
		put(path, state)
	}
	for path, byGeneration := range w.mutationWaiters {
		for _, waiter := range byGeneration {
			if waiter.ticket != nil {
				put(path, waiter.ticket.TierState())
			}
		}
	}
	return states
}

func (w *Watcher) TierState(path string) MutationTierState {
	w.mu.Lock()
	defer w.mu.Unlock()
	path = filepath.Clean(path)
	state := w.failedPaths[path]
	for generation, waiter := range w.mutationWaiters[path] {
		if generation >= state.Generation && waiter.ticket != nil {
			state = waiter.ticket.TierState()
		}
	}
	return state
}

// CaptureTierRepair binds a successful explicit single-file reparse to the
// failure it observed before starting. The caller invokes it only after proof
// that this exact file was reparsed successfully. Bounded derived incompleteness
// is separate evidence and cannot be repaired by a file parse alone.
func (w *Watcher) CaptureTierRepair(path string) func() {
	path = filepath.Clean(path)
	w.mu.Lock()
	observed := w.failedPaths[path]
	w.mu.Unlock()
	if !observed.Failed || observed.Partial {
		return nil
	}
	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		current, ok := w.failedPaths[path]
		if !ok {
			return
		}
		if observed.rebindRefused && !current.rebindRefused {
			current.rebindRefused = true
		}
		if current == observed {
			delete(w.failedPaths, path)
		}
	}
}

func (mw *MultiWatcher) CaptureTierRepair(path string) func() {
	mw.mu.Lock()
	_, prefix := mw.multi.IndexerForFile(path)
	watcher := mw.watchers[prefix]
	mw.mu.Unlock()
	if watcher != nil {
		// Capture this exact owner now. Looking it up after the reparse could
		// clear a replacement watcher's failure that the pass never observed.
		return watcher.CaptureTierRepair(path)
	}
	return nil
}

func (mw *MultiWatcher) TierPaths() map[string]MutationTierState {
	mw.mu.Lock()
	watchers := make([]*Watcher, 0, len(mw.watchers))
	for _, watcher := range mw.watchers {
		watchers = append(watchers, watcher)
	}
	mw.mu.Unlock()
	var states map[string]MutationTierState
	for _, watcher := range watchers {
		for path, state := range watcher.TierPaths() {
			if states == nil {
				states = make(map[string]MutationTierState)
			}
			states[path] = state
		}
	}
	return states
}

// TierAuthority reports whether this canonical repository has a live, healthy
// watcher. An empty outstanding-path set alone cannot establish that fact.
func (mw *MultiWatcher) TierAuthority(prefix string) bool {
	if mw == nil {
		return false
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	w := mw.watchers[prefix]
	return !mw.stopped && mw.started[prefix] && w != nil && w.DegradedReason() == ""
}

func (mw *MultiWatcher) TierState(path string) MutationTierState {
	mw.mu.Lock()
	_, prefix := mw.multi.IndexerForFile(path)
	watcher := mw.watchers[prefix]
	mw.mu.Unlock()
	if watcher != nil {
		return watcher.TierState(path)
	}
	return MutationTierState{}
}

// RelationsPending reports a restarted workspace's shared resolver boundary.
// The early serving boundary can precede cross-repository reference binding.
func (mi *MultiIndexer) RelationsPending() bool { return mi.relationsPending.Load() }

// TierState reads a ticket's live stamps; the ticket survives waiter removal.
func (t *MutationTicket) TierState() MutationTierState {
	if t == nil {
		return MutationTierState{}
	}
	t.tierMu.RLock()
	defer t.tierMu.RUnlock()
	return t.tiers
}

// CompleteStartupRelations is called only after successful warmup resolution.
// ResetBatch also serves cancelled warmup and cannot certify this boundary.
func (mi *MultiIndexer) CompleteStartupRelations() { mi.relationsPending.Store(false) }

func (w *Watcher) markDeferredMutationTiers(frontier map[string]uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, generation := range frontier {
		for ordinal, waiter := range w.mutationWaiters[path] {
			if ordinal <= generation && waiter.ticket != nil {
				waiter.ticket.tierMu.Lock()
				waiter.ticket.tiers.nonRefusalPartial = true
				waiter.ticket.tiers.Partial = true
				waiter.ticket.tierMu.Unlock()
			}
		}
	}
}

// Publish the checked gap while the point still owns the repository lane.
// A same-byte successor may enter the lane before this ticket completes.
func (w *Watcher) markRefusedRelationTiers(path string, generation uint64, duplicate bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prior := w.failedPaths[path]
	if duplicate {
		if !prior.rebindRefused || prior.Generation > generation {
			return
		}
	}
	at := time.Now()
	latest := prior
	for ordinal, waiter := range w.mutationWaiters[path] {
		if ordinal <= generation && waiter.ticket != nil {
			waiter.ticket.tierMu.Lock()
			state := &waiter.ticket.tiers
			if duplicate {
				state.nonRefusalPartial = state.nonRefusalPartial || prior.nonRefusalPartial
			}
			state.rebindRefused = true
			state.Partial = true
			state.RelationsAt = time.Time{}
			stampRelations(state, at)
			if ordinal >= latest.Generation {
				latest = *state
				latest.failedAt = at
			}
			waiter.ticket.tierMu.Unlock()
		}
	}
	if latest.rebindRefused {
		if w.failedPaths == nil {
			w.failedPaths = make(map[string]MutationTierState)
		}
		w.failedPaths[path] = latest
	}
}
