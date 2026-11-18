package agents

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// PendingAutomaticCheckoutGuidance is shared by initialize and lifecycle hooks.
// Family recognition does not grant access, and retrying frames rechecks
// admission without requiring a new MCP session.
const PendingAutomaticCheckoutGuidance = "This checkout belongs to an already tracked or configured Git family; Gortex is identifying it. " +
	"Graph calls return `view_building` until discovery completes; retry them, no reconnect is needed. Do not run `gortex track`. " +
	"Use `gortex repos explain-view` for discovery details; explicit tracking is only for a user-requested separate graph."

// FamilyRepoRoots unions declarations with the live registry without treating
// either list as an admission decision. The caller supplies its own config.
func FamilyRepoRoots(registered, configured []string) []string {
	roots := make([]string, 0, len(registered)+len(configured))
	for _, group := range [][]string{registered, configured} {
		for _, root := range group {
			if root = strings.TrimSpace(root); root != "" {
				roots = append(roots, root)
			}
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

// PendingAutomaticCheckout reports whether cwd is inside a Git checkout whose
// family is already represented by one of trackedRoots, but the checkout
// itself has not reached the tracked checkout registry yet. Callers must first
// establish that cwd is not already covered; this helper only proves family
// identity.
//
// Initialize and lifecycle hooks use this distinction before recommending a
// tracking action. An unrelated repository can legitimately be explicitly
// tracked; any other checkout of an existing family is discovered
// automatically and must not be converted into a dedicated graph merely
// because reconciliation has not published its route yet. This includes both
// directions: a new linked worktree beside a tracked primary, and the primary
// checkout when a linked worktree is the family member currently tracked.
//
// The probe is read-only and filesystem-only. It walks up to the nearest .git
// entry, then reuses the indexer's canonical worktree resolver; it never runs
// Git, starts a daemon, or mutates configuration.
func PendingAutomaticCheckout(cwd string, trackedRoots []string) bool {
	_, represented := AutomaticCheckoutFamily(cwd, trackedRoots)
	return represented != ""
}

// AutomaticCheckoutFamily returns the nearest Git checkout and the first
// configured or registered root representing its family. A nonempty second
// result proves only family identity; callers still decide admission using
// the registry/catalog. No Git process or configuration write is involved.
func AutomaticCheckoutFamily(cwd string, knownRoots []string) (checkout, represented string) {
	root := nearestGitRoot(cwd)
	if root == "" {
		return "", ""
	}
	candidate := indexer.ResolveWorktree(root)
	if candidate.GitCommonDir == "" {
		return root, ""
	}
	candidateCommon := canonicalFamilyPath(candidate.GitCommonDir)
	for _, trackedRoot := range knownRoots {
		trackedRoot = strings.TrimSpace(trackedRoot)
		if trackedRoot == "" {
			continue
		}
		tracked := indexer.ResolveWorktree(trackedRoot)
		if tracked.GitCommonDir == "" {
			continue
		}
		if pathkey.EqualPaths(candidateCommon, canonicalFamilyPath(tracked.GitCommonDir)) {
			return root, trackedRoot
		}
	}
	return root, ""
}

func nearestGitRoot(path string) string {
	return nearestGitRootWithLstat(path, os.Lstat)
}

func nearestGitRootWithLstat(path string, lstat func(string) (os.FileInfo, error)) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	current := filepath.Clean(abs)
	if info, statErr := os.Stat(current); statErr == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		if _, err := lstat(filepath.Join(current, ".git")); err == nil {
			return current
		} else if !os.IsNotExist(err) {
			// A nearer checkout boundary is unknown, not absent. Never
			// attribute it to an ancestor family on permission/I/O failure.
			return ""
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func canonicalFamilyPath(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	return cleaned
}
