package indexer

import "path/filepath"

// StormActiveForFile reports only an exact live path in the current queued
// storm batch. It is a reason hint, not freshness or lifecycle authority.
// Once a drain detaches its batch, its waiters are generic pending work and
// this method conservatively returns false. No in-flight mode is inferred.
func (w *Watcher) StormActiveForFile(absPath string) bool {
	if w == nil || !filepath.IsAbs(absPath) {
		return false
	}
	absPath = filepath.Clean(absPath)
	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	if w.stormStopped || !w.stormActive {
		return false
	}
	if _, queued := w.stormBatch[absPath]; !queued {
		return false
	}
	generation := w.stormGenerations[absPath]
	if generation == 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping || w.pendingGeneration[absPath] != generation {
		return false
	}
	waiter, live := w.mutationWaiters[absPath][generation]
	ticket := waiter.ticket
	return live && ticket != nil && ticket.Path == absPath && ticket.Generation == generation && waiter.done != nil && ticket.Done == waiter.done
}

// StormActiveForFile snapshots the path's current started owner. The child
// observation takes only that watcher's existing locks after releasing mu;
// unrelated repositories cannot supply a positive hint.
func (mw *MultiWatcher) StormActiveForFile(absPath string) bool {
	if mw == nil || mw.multi == nil || !filepath.IsAbs(absPath) {
		return false
	}
	absPath = filepath.Clean(absPath)
	prefix := mw.multi.RepoForFile(absPath)
	mw.mu.Lock()
	watcher := mw.watchers[prefix]
	started := mw.started[prefix]
	stopped := mw.stopped
	mw.mu.Unlock()
	return !stopped && started && watcher != nil && watcher.StormActiveForFile(absPath)
}
