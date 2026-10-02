package pi

// legacy.go takes back the extension Gortex rendered before the package
// migration: a templated `index.ts` under an `extensions/gortex/`
// directory, written by `gortex init` at project scope and by `gortex
// install` at user scope.
//
// # Why it has to go
//
// Pi auto-discovers `.pi/extensions/*/index.ts` and its global
// counterpart. A copy left beside the `pi-gortex` package therefore runs
// a second bridge in the same session: two `gortex mcp` children, the
// tool surface registered twice, and a duplicate hook on every tool call.
// Both scopes are swept on every run, in either mode, because a stale
// global copy double-loads in every project and a project-scoped init
// that ignored it would not actually fix the repo it was run in.
//
// # Ownership, not location
//
// Deletion is gated on legacyMarker, the header every rendered copy
// carried, never on the path alone. The rendered file was templated per
// machine, so there is no stable shipped body to byte-compare against the
// way the codex / copilotcli / opencode removers do; the header is the
// evidence that remains. An extension somebody else wrote under the same
// name is kept, with a warning.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// legacyMarker is the header every rendered index.ts carried.
const legacyMarker = "// Gortex extension for the Pi coding agent"

// LegacyExtensionPath is the index.ts earlier releases rendered under an
// extensions root.
func LegacyExtensionPath(extensionsRoot string) string {
	if extensionsRoot == "" {
		return ""
	}
	return filepath.Join(extensionsRoot, "gortex", "index.ts")
}

// GlobalLegacyExtensionPath is the user-level copy under the agent dir in
// force now, shared with uninstall so the remover and the wizard's
// preview name one path.
func GlobalLegacyExtensionPath(home string) string {
	dir := AgentDir(home)
	if dir == "" {
		return ""
	}
	return LegacyExtensionPath(filepath.Join(dir, "extensions"))
}

// globalLegacyExtensionPaths is every user-level location a release may
// have rendered into. AgentDir honours $PI_CODING_AGENT_DIR, while the
// adapter that wrote these files joined <home>/.pi/agent unconditionally:
// with the override set the copy sits under the default dir, so the
// override path on its own would never find it.
func globalLegacyExtensionPaths(home string) []string {
	if home == "" {
		return nil
	}
	var paths []string
	paths = appendPath(paths, GlobalLegacyExtensionPath(home))
	paths = appendPath(paths, LegacyExtensionPath(
		filepath.Join(home, filepath.FromSlash(defaultAgentDir), "extensions")))
	return paths
}

// legacyExtensionPaths lists every rendered extension to take back,
// project and global, regardless of the mode being applied.
func legacyExtensionPaths(env agents.Env) []string {
	var paths []string
	if env.Root != "" {
		paths = appendPath(paths, LegacyExtensionPath(filepath.Join(env.Root, ".pi", "extensions")))
	}
	for _, global := range globalLegacyExtensionPaths(env.Home) {
		paths = appendPath(paths, global)
	}
	return paths
}

// appendPath appends a non-empty path the list does not already hold, so
// a sweep visits each file once when the scopes resolve to the same one.
func appendPath(paths []string, path string) []string {
	if path == "" {
		return paths
	}
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}

// ownsLegacyExtension reports whether path holds an index.ts Gortex
// rendered. It is the same test removeLegacyExtension applies, so a plan
// never promises a deletion Apply will decline, and the uninstall wizard
// never previews one RemoveGlobal will keep.
func ownsLegacyExtension(path string) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), legacyMarker)
}

// removeLegacyExtension deletes one rendered index.ts and prunes the
// directory it emptied, reporting whether it removed anything. Both the
// init-time sweep and the uninstall path go through here so the two can
// only ever apply the same ownership test.
func removeLegacyExtension(w io.Writer, path, logFormat string, opts agents.ApplyOpts) (agents.FileAction, bool, error) {
	if path == "" {
		return agents.FileAction{}, false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return agents.FileAction{}, false, nil
	}
	if err != nil {
		return agents.FileAction{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.Contains(string(data), legacyMarker) {
		internalutil.Warnf(w, "keeping %s: not an extension Gortex wrote", path)
		return agents.FileAction{}, false, nil
	}
	action, err := deleteFile(w, path, logFormat, opts)
	if err != nil {
		return agents.FileAction{}, false, err
	}
	if !opts.DryRun {
		// Each prune is an os.Remove, which refuses a directory still
		// holding anything.
		_ = os.Remove(filepath.Dir(path))
		_ = os.Remove(filepath.Dir(filepath.Dir(path)))
	}
	return action, true, nil
}

// removeLegacyExtensions is the init-time sweep across both scopes. A
// path that cannot be read costs a warning rather than the run: the
// package is already configured by the time this runs, and a duplicate
// bridge is a worse outcome to report than to fail on.
func removeLegacyExtensions(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	var actions []agents.FileAction
	for _, path := range legacyExtensionPaths(env) {
		action, removed, err := removeLegacyExtension(env.Stderr, path, "[gortex init] removed %s", opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "pi: %v", err)
			continue
		}
		if removed {
			actions = append(actions, action)
		}
	}
	if len(actions) > 0 {
		internalutil.Logf(env.Stderr,
			"[gortex init] Pi now loads Gortex from the %s package; the extension Gortex used to write is gone",
			PackageSpec)
	}
	return actions
}

// removeGlobalLegacyExtension is the uninstall-time half, reporting in
// the (removed, failures) shape `gortex uninstall` collects.
func removeGlobalLegacyExtension(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	for _, path := range globalLegacyExtensionPaths(env.Home) {
		_, gone, err := removeLegacyExtension(env.Stderr, path, "[gortex uninstall] removed %s", opts)
		switch {
		case err != nil:
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
		case gone:
			removed++
		}
	}
	return removed, failures
}
