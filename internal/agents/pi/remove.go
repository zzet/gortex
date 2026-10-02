package pi

// remove.go is the counterpart to a ModeGlobal Apply for `gortex
// uninstall --global`.
//
// # What comes out, and what stays
//
// Two artifacts: the user-level sidecar, and the extension earlier
// releases rendered beside it (legacy.go). Both are files Gortex authored
// end to end.
//
// The `packages` entry in ~/.pi/agent/settings.json stays. That file is
// the user's, we only ever appended one string to it, and `pi remove
// npm:pi-gortex` is the command that owns taking it back. Leaving it
// costs a load attempt for a package that is no longer wanted; removing
// it would mean rewriting a settings file whose every other line is
// theirs.
//
// Project-scoped copies stay too, for a duller reason: they live in
// whichever repos the user ran `gortex init` in, and a machine-level
// uninstall cannot enumerate those.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// RemoveGlobal strips the user-level Pi footprint `gortex install` wrote.
// Returns the number of artifacts removed and any per-artifact failures.
// Signature mirrors claudecode.Adapter.RemoveGlobal so `gortex uninstall`
// can call every host through the same shape.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if env.Home == "" {
		return 0, []string{"pi: global cleanup requires a resolved home directory"}
	}

	// 1. <agentDir>/extensions/gortex.json — the posture sidecar.
	sidecarRemoved, sidecarFailures := removeSidecar(env, opts)
	removed += sidecarRemoved
	failures = append(failures, sidecarFailures...)

	// 2. <agentDir>/extensions/gortex/index.ts — the pre-package bridge,
	// under both the agent dir in force and the default one (legacy.go).
	legacyRemoved, legacyFailures := removeGlobalLegacyExtension(env, opts)
	removed += legacyRemoved
	failures = append(failures, legacyFailures...)

	return removed, failures
}

// GlobalArtifacts lists the user-level Pi paths that currently carry a
// Gortex footprint, sorted. It applies the same ownership tests
// RemoveGlobal does, so the uninstall wizard's preview can never promise
// a deletion that will not happen.
func GlobalArtifacts(home string) []string {
	if home == "" {
		return nil
	}
	var present []string
	if path := GlobalSidecarPath(home); ownsSidecar(path) {
		present = append(present, path)
	}
	for _, path := range globalLegacyExtensionPaths(home) {
		if ownsLegacyExtension(path) {
			present = append(present, path)
		}
	}
	sort.Strings(present)
	return present
}

// removeSidecar deletes the user-level sidecar when it is still a file
// this adapter would write. A hand-authored file with other keys in it is
// somebody's configuration and is kept with a warning.
func removeSidecar(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	path := GlobalSidecarPath(env.Home)
	if path == "" {
		return 0, nil
	}
	if !fileExists(path) {
		return 0, nil
	}
	if !ownsSidecar(path) {
		internalutil.Warnf(env.Stderr, "keeping customised %s", path)
		return 0, nil
	}
	if _, err := deleteFile(env.Stderr, path, "[gortex uninstall] removed %s", opts); err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	if !opts.DryRun {
		_ = os.Remove(filepath.Dir(path))
	}
	return 1, nil
}

// ownsSidecar reports whether path is a sidecar this adapter would have
// produced: a JSON object whose every key is one sidecarConfig writes.
// Anything else is the user's own configuration.
func ownsSidecar(path string) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	for key := range cfg {
		switch key {
		case "bin", "hook_mode", "enforce":
		default:
			return false
		}
	}
	return true
}
