package indexer

// MutationCounters reports bounded aggregate totals for this watcher instance.
// Supersession counts claimed callbacks, not canceled quiet timers. Echoes count
// content-validated held paths, not provisional holds or individual OS events.
func (w *Watcher) MutationCounters() map[string]uint64 {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return map[string]uint64{
		"echoes_attached":                  w.mutationEchoesAttached,
		"generations_superseded":           w.mutationSuperseded,
		"waiters_completed_by_later_patch": w.mutationWaitersLater,
	}
}

func (w *Watcher) noteMutationSuperseded() {
	w.mu.Lock()
	w.mutationSuperseded++
	w.mu.Unlock()
}

// MutationCounters sums only the currently active watcher instances. Retiring a
// watcher retires its totals; this observation does not claim process history.
func (mw *MultiWatcher) MutationCounters() map[string]uint64 {
	if mw == nil {
		return nil
	}
	mw.mu.Lock()
	watchers := make([]*Watcher, 0, len(mw.watchers))
	for _, w := range mw.watchers {
		watchers = append(watchers, w)
	}
	mw.mu.Unlock()
	totals := map[string]uint64{"echoes_attached": 0, "generations_superseded": 0, "waiters_completed_by_later_patch": 0}
	for _, w := range watchers {
		for name, count := range w.MutationCounters() {
			totals[name] += count
		}
	}
	return totals
}
