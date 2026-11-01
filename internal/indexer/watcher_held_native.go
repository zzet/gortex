package indexer

// holdNativeForToolLocked attaches a native event to outstanding tool work.
// Explicit drift-repair admission never uses this path. w.mu must be held.
func (w *Watcher) holdNativeForToolLocked(path string, kind ChangeKind) bool {
	if w.stopping {
		return true
	}
	for _, waiter := range w.mutationWaiters[path] {
		if waiter.tool {
			if w.heldNative == nil {
				w.heldNative = make(map[string]ChangeKind)
			}
			w.heldNative[path] = kind
			return true
		}
	}
	return false
}

// drainHeldNative uses the existing persisted content receipt after the last
// tool waiter completes. The receipt includes content and stable file identity;
// a coarse timestamp or same-size save alone cannot suppress native work.
func (w *Watcher) drainHeldNative(path string) {
	w.mu.Lock()
	kind, held := w.heldNative[path]
	if !held || w.holdNativeForToolLocked(path, kind) {
		w.mu.Unlock()
		return
	}
	delete(w.heldNative, path)
	w.mu.Unlock()
	if idx := w.currentMutationIndexer(); idx != nil && idx.fileMtimeMatches(path) {
		// A provisional hold becomes an echo only after content validation.
		// Multiple native events coalesced in this path count one attachment.
		w.mu.Lock()
		w.mutationEchoesAttached++
		w.mu.Unlock()
		return
	}
	// This rechecks tool ownership at admission in case a tool arrived while
	// the content receipt was being read outside watcher locks.
	w.scheduleNativeEvent(path, kind)
}
