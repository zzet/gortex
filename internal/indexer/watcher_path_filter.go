package indexer

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
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
		return !m.MatchRelEither(path)
	}
	// An ignore file's events keep the per-folder rule cache current, even
	// when the file itself is excluded; the full check still keeps it out
	// of the index.
	if slices.Contains(dirIgnoreFiles, filepath.Base(path)) {
		return true
	}
	rel, err := filepath.Rel(root, path)
	// Defer MCP config paths to the full check, without statting paths in
	// this backend hot path. Other agent-config paths fall to the Builtin
	// exclusion below.
	if err == nil && (excludes.IsAgentConfigMCPDir(rel) || excludes.IsAgentConfigMCPFile(rel)) {
		return true
	}
	// The backend does not know whether this is a file or a folder.
	// A directory-only negation must keep its folder events deliverable.
	if err != nil {
		return true
	}
	return !m.MatchRelEither(rel)
}

// isProbeMarkerPath reports whether path names a watcher handshake probe.
// Single definition so the backend filter and every event-loop consumer
// agree on what a probe looks like.
func isProbeMarkerPath(path string) bool {
	return strings.Contains(filepath.Base(path), probeMarker)
}
