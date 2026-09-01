package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Resolving on-disk paths under a view.
//
// A view of a committed tree has no working copy, so its content comes out of
// the object store through refViewFiles and no path it resolves names a file:
// see errViewHasNoWorkingCopy below. A worktree view is the other case — it has
// a working copy, just not the one the path resolvers anchor to. Every resolver
// joins a repo-relative path onto the repository's canonical root, which is the
// checkout the index was built from: a different branch, and different bytes,
// from the one the request is reading through.
//
// The fix is one step at the end of resolution rather than a second resolver:
// the anchoring rules (repo prefixes, containment, sole-repo inference) are
// unchanged, and the checkout they land in is chosen last. That keeps a request
// with no view byte-identical to what it was, and makes "which checkout" a
// property of the request instead of a property of each call site.

// viewPathRoot is the working copy a request reads through, plus the repository
// it serves. The zero value is what every request that is not routed to a
// worktree view carries, and every method on it is then identity.
type viewPathRoot struct {
	root       string
	repoPrefix string
}

// requestViewPathRoot reports the working copy this request's view reads. It is
// empty for the base corpus and for a view of a committed tree — the latter
// serves bytes through refViewFiles, which replaces path resolution rather than
// re-rooting it.
func requestViewPathRoot(ctx context.Context) viewPathRoot {
	view := requestViewFromContext(ctx)
	if view == nil || view.viewRoot == "" {
		return viewPathRoot{}
	}
	return viewPathRoot{root: view.viewRoot, repoPrefix: viewRepoPrefix(view)}
}

// serves reports whether paths in a repository read through this view. An
// unknown prefix on either side means there is only one repository in play,
// which is the single-repo posture every other resolver takes.
func (v viewPathRoot) serves(repoPrefix string) bool {
	if v.root == "" {
		return false
	}
	return v.repoPrefix == "" || repoPrefix == "" || v.repoPrefix == repoPrefix
}

// contains reports whether an absolute path belongs to the selected checkout,
// accepting the physical spelling of a symlinked temp/root path as well as its
// lexical spelling. A symlink beneath the checkout is still checked by the
// existing repository confinement guard after resolution.
func (v viewPathRoot) contains(abs string) bool {
	if v.root == "" || abs == "" {
		return false
	}
	if pathContainedIn(filepath.Clean(abs), filepath.Clean(v.root)) {
		return true
	}
	realRoot, err := filepath.EvalSymlinks(v.root)
	if err != nil || realRoot == "" {
		realRoot = filepath.Clean(v.root)
	}
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil || realAbs == "" {
		realAbs = resolveNearestExistingAncestor(abs)
	}
	return realAbs != "" && pathContainedIn(filepath.Clean(realAbs), filepath.Clean(realRoot))
}

// validateAbsolute refuses a path that names another checkout while the
// request reads a selected worktree. Accepting it would combine graph rows from
// one view with bytes from a sibling checkout merely because both roots are
// registered and therefore pass the general repository confinement guard.
func (v viewPathRoot) validateAbsolute(abs string) error {
	if v.root == "" || !filepath.IsAbs(abs) || v.contains(abs) {
		return nil
	}
	return fmt.Errorf(
		"absolute path %q belongs to a different checkout than the selected view rooted at %q; name the file relative to the selected checkout",
		abs, v.root)
}

// graphRelative renders an absolute path inside the selected checkout using
// the spelling stored in the graph. Linked worktree roots are not canonical
// MultiIndexer roots, so Server.repoRelative cannot attribute them and would
// otherwise leak the absolute checkout path into mutation responses, sessions,
// syntax checks, and graph lookups.
//
// Prefer the lexical spelling so a symlinked file inside the checkout keeps
// its own graph identity. When the caller used the physical spelling of a
// symlinked checkout root (for example /private/tmp for /tmp on macOS), fall
// back to physical root and target spellings solely to compute the same
// checkout-relative suffix.
func (v viewPathRoot) graphRelative(abs string) (string, bool) {
	if v.root == "" || abs == "" || !filepath.IsAbs(abs) {
		return "", false
	}
	rel, ok := relativeWithinRoot(v.root, abs)
	if !ok {
		return "", false
	}
	if v.repoPrefix != "" {
		rel = filepath.Join(v.repoPrefix, rel)
	}
	return rel, true
}

// relativeWithinRoot renders target beneath root without letting a filesystem
// alias change the relative suffix. The lexical fast path is allocation-light;
// the canonical fallback handles /tmp versus /private/tmp and symlinked
// workspace roots. A missing target resolves through its nearest existing
// ancestor so newly-created files keep working too.
func relativeWithinRoot(root, target string) (string, bool) {
	if root == "" || target == "" {
		return "", false
	}
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if !pathContainedIn(target, root) {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil || realRoot == "" {
			return "", false
		}
		realTarget, err := filepath.EvalSymlinks(target)
		if err != nil || realTarget == "" {
			realTarget = resolveNearestExistingAncestor(target)
		}
		root, target = filepath.Clean(realRoot), filepath.Clean(realTarget)
		if !pathContainedIn(target, root) {
			return "", false
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// rooted moves an absolute path that resolved against root into the checkout
// this view reads. A path already inside the view, or one that never sat under
// root, is returned untouched — re-rooting is a translation between two
// checkouts of one repository, not a way to invent a location.
func (v viewPathRoot) rooted(abs, root string) string {
	if v.root == "" || abs == "" || root == "" {
		return abs
	}
	if pathContainedIn(abs, v.root) {
		return abs
	}
	rel, ok := relativeWithinRoot(root, abs)
	if !ok {
		return abs
	}
	return filepath.Clean(filepath.Join(v.root, rel))
}

// errViewHasNoWorkingCopy is what node and graph path resolution answers a
// request reading a committed tree.
//
// Refusing is the point: every repository root on disk holds some other state
// of the world, so a path joined against one would name real bytes that are not
// this view's. Callers that tolerate a resolution failure fall back to the
// view's own file surface or report no path at all; callers that do not, refuse
// — which is the honest answer for a location that does not exist.
var errViewHasNoWorkingCopy = errors.New(
	"this request reads a committed tree that is not checked out anywhere, so it has no path on disk")

// requestReadsCommittedTree reports whether this request's view serves its
// content out of the object store rather than a working copy.
func requestReadsCommittedTree(ctx context.Context) bool {
	view := requestViewFromContext(ctx)
	return view != nil && view.files != nil && view.viewRoot == ""
}

// checkoutRootedPath places a resolved path in the checkout that owns it for
// this request.
//
// A routed view answers outright: it names the working copy the request reads,
// so the existence heuristic must not get a vote — it would happily move the
// path into a third checkout that happens to carry the file. Every other
// request keeps the heuristic it has always had.
func (s *Server) checkoutRootedPath(ctx context.Context, abs, root, repoPrefix string) string {
	if view := requestViewPathRoot(ctx); view.serves(repoPrefix) {
		return view.rooted(abs, root)
	}
	return worktreeRootedPath(abs, root, s.multiIndexer)
}
