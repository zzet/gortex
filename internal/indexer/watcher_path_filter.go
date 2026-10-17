package indexer

import (
	"path/filepath"
	"strings"
)

// watcherExcludeFilter adapts the indexer's exclude matcher to the file
// watcher's PathFilter interface, so events under ignored trees are
// dropped inside the backend rather than crossing the event channel, the
// aggregator and the storm counter first.
//
// It is an optimisation, not a security barrier: handleEvent still runs
// the full isExcluded check, which additionally refuses symlinks that
// escape the repo root. ConfigManager-owned watchers check its cached ignore
// chain here too, so a removed ignore rule cannot keep hiding future events.
// Unchanged policy reuses the compiled matcher; only a policy change schedules
// the existing coalesced reconcile.
type watcherExcludeFilter struct{ w *Watcher }

// ShouldInclude reports whether the backend should deliver an event for
// path. A watcher with no matcher includes everything.
func (f *watcherExcludeFilter) ShouldInclude(path string) bool {
	// Handshake probes are the watcher's own liveness signal, not repository
	// content, and the startup barrier writes them into <root>/.git so they
	// stay out of git status. Excluding them here would drop the very event
	// Start is waiting for and hang the barrier until it times out. Every
	// downstream consumer (mergeInitialReplayEvent, the barrier tail loop,
	// handleEvent) already discards probe paths, so admitting them costs one
	// channel send and never reaches the indexer.
	if isProbeMarkerPath(path) {
		return true
	}
	if f == nil || f.w == nil {
		return true
	}
	m := f.w.currentExcludeMatcher()
	if m == nil {
		return true
	}
	root := f.w.indexer.rootPath
	if root == "" {
		return !m.MatchRel(path)
	}
	return !m.MatchAbs(path, root)
}

// isProbeMarkerPath reports whether path names a watcher handshake probe.
// Single definition so the backend filter and every event-loop consumer
// agree on what a probe looks like.
func isProbeMarkerPath(path string) bool {
	return strings.Contains(filepath.Base(path), probeMarker)
}
