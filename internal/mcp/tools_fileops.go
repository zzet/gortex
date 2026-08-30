package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/elide"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/tokens"
)

// errPathUnresolved is returned when a relative path cannot be anchored to any
// indexed repo. Callers should surface this as a clear error rather than letting
// os.Open resolve it against the daemon process CWD, which is unrelated to any
// repo and silently produces wrong results.
var errPathUnresolved = errors.New("path is not absolute and no indexed repo could anchor it")

// errPathEscape is returned when a relative or repo-prefixed path resolves
// outside the anchor repo's root via `..` traversal. Absolute paths bypass
// this check by design — agents that hand over an absolute path are
// responsible for the location.
var errPathEscape = errors.New("relative path escapes the indexed repo root")

// resolveFilePath turns a user-supplied path into the absolute filesystem
// path the write should target. Accepts:
//   - absolute paths (still confined: refused if outside every repo root)
//   - repo-prefixed paths (e.g. "gortex/internal/foo.go" in multi-repo mode)
//   - paths relative to the single indexer's root (single-repo mode only)
//
// Returns the absolute path, the repo-relative form for session
// bookkeeping, and an error sentinel describing WHY the input was
// rejected when one of the rejection paths fires:
//
//   - errPathUnresolved — empty input, missing indexer, or a multi-repo
//     bare-relative path that doesn't match any registered prefix
//     (no implicit "primary repo", so the daemon refuses to fall
//     through to its process CWD).
//   - errPathEscape — any input (relative, repo-prefixed, or absolute)
//     whose resolved location lands outside every indexed repo root,
//     whether via `..` segments, an out-of-root absolute path, or a
//     symlink that points out. Catches `../../etc/passwd` and
//     `/etc/passwd` style attempts at the boundary instead of letting
//     them silently land on system files.
//
// Both relative and absolute inputs are confined to the indexed
// repository roots: an absolute path that points outside every root is
// refused, not honoured. See the confinement guard at the top of the body.
//
// ctx decides which checkout a resolved repo-relative path lands in: a request
// routed to a worktree view reads that checkout's working copy, every other
// request the canonical one. An absolute input names a location outright and is
// never moved between checkouts.
func (s *Server) resolveFilePath(ctx context.Context, rawPath string) (absPath, relPath string, err error) {
	// Post-resolution confinement (the SECURITY.md "confined to indexed
	// repository roots" invariant): whatever path the body resolves below —
	// relative or absolute — is refused if its real, symlink-resolved
	// location falls outside every indexed repository root. This is the
	// central choke point; no caller, read or write, may bypass it. It is a
	// no-op for control clients that have no known roots (the same posture
	// guardSymlinkWithinRepo takes on the read path).
	defer func() {
		if err == nil {
			if guardErr := s.guardSymlinkWithinRepo(ctx, absPath); guardErr != nil {
				absPath, relPath, err = "", "", guardErr
			}
		}
	}()
	if rawPath == "" {
		return "", "", fmt.Errorf("%w: path is empty", errPathUnresolved)
	}

	if filepath.IsAbs(rawPath) {
		abs := filepath.Clean(rawPath)
		view := requestViewPathRoot(ctx)
		if viewErr := view.validateAbsolute(abs); viewErr != nil {
			return "", "", viewErr
		}
		if rel, ok := view.graphRelative(abs); ok {
			return abs, rel, nil
		}
		return abs, s.repoRelative(abs), nil
	}

	if s.multiIndexer != nil {
		// A repo-prefixed path resolves directly. A bare-relative path is
		// ambiguous across several tracked repos and must be refused rather
		// than fall through to the daemon process CWD — but with exactly one
		// tracked repo there is nothing to be ambiguous about, so it anchors
		// to that repo's root. This is what lets an agent (or a hook, or the
		// review pack) name `internal/foo.go` in a solo workspace without
		// first learning the prefix, including for a file it is about to
		// create.
		prefix := matchedRepoPrefix(s.multiIndexer, rawPath)
		if prefix == "" {
			if soleRepo, root, ok := soleTrackedRepo(s.multiIndexer); ok {
				abs := filepath.Clean(filepath.Join(root, rawPath))
				if !pathContainedIn(abs, root) {
					return "", "", fmt.Errorf("%w: %q resolves to %q, outside repo root %q", errPathEscape, rawPath, abs, root)
				}
				abs = s.checkoutRootedPath(ctx, abs, root, soleRepo)
				// relPath is the GRAPH's spelling, not the caller's: it is
				// what downstream node lookups (get_file_summary, savings
				// recording, session bookkeeping) key on, and graph file
				// keys are prefixed. Echoing the bare input back resolved
				// the bytes correctly and then reported file_not_indexed
				// for a file that was indexed.
				return abs, soleRepo + "/" + rawPath, nil
			}
			// No matched prefix and no lone repo: an unprefixed path is
			// normally ambiguous across tracked repos. But when it names
			// an existing file under exactly one repo there is no
			// ambiguity — anchor it there, so an agent can edit
			// gortex/internal/x.go as internal/x.go without first
			// learning the prefix. A brand-new file (matches == 0) still
			// requires an explicit prefix; a path present in several
			// repos (matches > 1) is reported as such.
			if abs, rel, matches := anchorUnprefixedExisting(s.multiIndexer, requestViewPathRoot(ctx), rawPath); matches == 1 {
				return abs, rel, nil
			} else if matches > 1 {
				return "", "", fmt.Errorf("%w: path %q names a file in multiple tracked repos; prefix it with one of: %s/",
					errPathUnresolved, rawPath, strings.Join(s.multiIndexer.RepoPrefixes(), "/, "))
			}
			prefixes := s.multiIndexer.RepoPrefixes()
			return "", "", fmt.Errorf("%w: path %q does not start with a known repo prefix; expected one of: %s/, or an absolute path",
				errPathUnresolved, rawPath, strings.Join(prefixes, "/, "))
		}
		root, ok := s.multiIndexer.RepoRoot(prefix)
		if !ok {
			return "", "", fmt.Errorf("%w: repo prefix %q has no root path", errPathUnresolved, prefix)
		}
		// A leading segment matching a tracked prefix IS that prefix, and
		// stripping it is unambiguous. This used to need a stat()-based
		// collision guard: a lone repo's indexed paths were raw relative
		// paths, so for a repo "api" containing its own api/ directory,
		// "api/handlers.go" could mean either the prefixed form or the raw
		// one, and the guard picked whichever existed on disk. Against
		// prefixed paths that guard inverts — it would return
		// <root>/api/handlers.go where the caller meant <root>/handlers.go.
		abs := filepath.Clean(filepath.Join(root, strings.TrimPrefix(rawPath, prefix+"/")))
		if !pathContainedIn(abs, root) {
			return "", "", fmt.Errorf("%w: %q resolves to %q, outside repo root %q", errPathEscape, rawPath, abs, root)
		}
		// Re-root the write into the linked worktree the file belongs
		// to when the resolved root is a main checkout that shares its
		// index identity with one. relPath stays the repo-prefixed form
		// for session bookkeeping — the prefix names the same repo
		// regardless of which worktree the bytes land in.
		abs = s.checkoutRootedPath(ctx, abs, root, prefix)
		return abs, rawPath, nil
	}

	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			abs := filepath.Clean(filepath.Join(root, rawPath))
			if !pathContainedIn(abs, root) {
				return "", "", fmt.Errorf("%w: %q resolves to %q, outside repo root %q", errPathEscape, rawPath, abs, root)
			}
			return requestViewPathRoot(ctx).rooted(abs, root), rawPath, nil
		}
	}

	return "", "", fmt.Errorf("%w: no indexer is attached and path %q is not absolute", errPathUnresolved, rawPath)
}

// matchedRepoPrefix returns the longest repo prefix that prefixes rawPath
// (with a "/" separator) in the multi-indexer. Used to decide which repo
// anchors a repo-prefixed path before joining with its root.
func matchedRepoPrefix(mi multiRepoLookup, rawPath string) string {
	if mi == nil || rawPath == "" {
		return ""
	}
	best := ""
	for _, prefix := range mi.RepoPrefixes() {
		if prefix == "" {
			continue
		}
		// Longest match wins so a nested repo (prefix "a/b") is chosen
		// over its parent (prefix "a") for a path under the child,
		// independent of RepoPrefixes() iteration order.
		if strings.HasPrefix(rawPath, prefix+"/") && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}

// soleTrackedRepo returns the prefix and root of the only tracked repo, or
// ok=false when zero or several are tracked.
//
// One tracked repo is what makes an unqualified path answerable: there is
// nothing for `internal/foo.go` to be ambiguous between. Callers use it to
// anchor bare repo-relative input — including paths naming files that do not
// exist yet, which an existence check could not resolve.
func soleTrackedRepo(mi multiRepoLookup) (prefix, root string, ok bool) {
	if mi == nil {
		return "", "", false
	}
	prefixes := mi.RepoPrefixes()
	if len(prefixes) != 1 || prefixes[0] == "" {
		return "", "", false
	}
	root, ok = mi.RepoRoot(prefixes[0])
	if !ok || root == "" {
		return "", "", false
	}
	return prefixes[0], root, true
}

// multiRepoLookup is the subset of *MultiIndexer that resolveFilePath
// needs. Pulled out as an interface for testability and to keep the
// resolver decoupled from the broader MultiIndexer surface.
type multiRepoLookup interface {
	RepoPrefixes() []string
	RepoRoot(prefix string) (string, bool)
	// LinkedWorktreeRoots returns the on-disk roots of every tracked
	// linked git worktree that shares a .git common directory with the
	// checkout at the given path.
	LinkedWorktreeRoots(mainRepoPath string) []string
}

// worktreeRootedPath re-roots an edit target into the linked git
// worktree the file actually belongs to. All worktrees of one repo
// reuse a single index identity, so a repo-relative path resolved
// against one checkout's root (root → abs) can name a file that
// physically lives in a sibling worktree. When the resolved root is a
// main checkout, abs does not exist there, and exactly one tracked
// linked worktree of that repo *does* contain the same repo-relative
// file, the write is re-rooted into that worktree — so editing a file
// in a linked worktree modifies the worktree's copy, not the main
// repo's.
//
// The function is deliberately conservative:
//   - It never moves a path that already exists at abs — the resolved
//     root genuinely owns the file.
//   - It never moves a path when the resolved root is itself a linked
//     worktree — abs is already inside the right checkout.
//   - It never moves a brand-new file (one that exists in no checkout):
//     a fresh write_file lands under the prefix the caller named.
//   - It re-roots only when the match is unambiguous (exactly one
//     worktree contains the file); two candidates leave abs untouched.
func worktreeRootedPath(abs, root string, mi multiRepoLookup) string {
	if abs == "" || root == "" || mi == nil {
		return abs
	}
	// Already inside a linked worktree — nothing to re-root.
	if indexer.ResolveWorktree(root).IsWorktree {
		return abs
	}
	// The file is physically present where it resolved — the resolved
	// root owns it.
	if _, err := os.Stat(abs); err == nil {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return abs
	}
	match := ""
	for _, wt := range mi.LinkedWorktreeRoots(root) {
		candidate := filepath.Clean(filepath.Join(wt, rel))
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if match != "" && match != candidate {
			// Ambiguous — more than one worktree carries this file.
			// Leave the path at its originally-resolved location.
			return abs
		}
		match = candidate
	}
	if match != "" {
		return match
	}
	return abs
}

// anchorUnprefixedExisting tries to anchor a bare repo-relative path —
// one that carries no repo prefix — to the single tracked repo that
// actually contains it. It joins rawPath against each repo root and
// counts the repos where the resulting file exists on disk. A unique
// match (matches == 1) is unambiguous and safe for the caller to honour:
// absPath is the worktree-rooted absolute path and relPath the
// repo-prefixed form used for session bookkeeping. matches == 0 means no
// tracked repo has the file (e.g. a brand-new write target) and matches
// > 1 means the path names a file in several repos; in both cases absPath
// and relPath are unset and the caller decides how to report it. The
// existence gate is what keeps this unambiguous — it never invents a
// location for a path that does not already resolve to exactly one file.
// view is the checkout the calling request reads: the zero value keeps the
// canonical anchoring, and a routed one moves both the existence probe and the
// answer into its own working copy.
func anchorUnprefixedExisting(mi multiRepoLookup, view viewPathRoot, rawPath string) (absPath, relPath string, matches int) {
	if mi == nil || rawPath == "" || filepath.IsAbs(rawPath) {
		return "", "", 0
	}
	for _, prefix := range mi.RepoPrefixes() {
		if prefix == "" {
			continue
		}
		root, ok := mi.RepoRoot(prefix)
		if !ok || root == "" {
			continue
		}
		cand := filepath.Clean(filepath.Join(root, rawPath))
		if !pathContainedIn(cand, root) {
			continue
		}
		// Existence is tested in the checkout this request reads. For a base
		// request that is the canonical root, exactly as before; for a routed
		// one it is the view's working copy, so a file only that checkout
		// carries still anchors the path it uniquely names.
		var resolved string
		probe := cand
		if view.serves(prefix) {
			resolved = view.rooted(cand, root)
			probe = resolved
		} else {
			resolved = worktreeRootedPath(cand, root, mi)
		}
		if _, err := os.Stat(probe); err != nil {
			continue
		}
		matches++
		absPath = resolved
		relPath = prefix + "/" + rawPath
	}
	if matches != 1 {
		return "", "", matches
	}
	return absPath, relPath, matches
}

// pathContainedIn reports whether abs sits at or beneath root, after
// cleaning both. The check is purely lexical — it doesn't dereference
// symlinks — but combined with filepath.Clean it catches the standard
// `..` traversal class. A defense-in-depth pass for symlink-target
// containment is left to the OS-level rename, which fails when the
// destination resolves to a different filesystem object.
func pathContainedIn(abs, root string) bool {
	if abs == "" || root == "" {
		return false
	}
	abs = filepath.Clean(abs)
	root = filepath.Clean(root)
	if abs == root {
		return true
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// guardSymlinkWithinRepo refuses to serve a file whose REAL (symlink-resolved)
// location is outside every tracked repository root. resolveFilePath /
// resolveNodePath already block lexical `../` traversal, but a symlink INSIDE
// the repo can still point at an arbitrary file on disk (repo/link ->
// /etc/passwd) that os.ReadFile would happily follow. This closes that hole by
// resolving the real path and requiring it to stay within an indexed root.
// Roots are symlink-resolved too, so a repo under a symlinked prefix (macOS
// /tmp -> /private/tmp, a temp-dir test root) still matches. A broken / missing
// target, or a control client with no known roots, is left to the normal read
// path — there is nothing to leak.
func (s *Server) guardSymlinkWithinRepo(ctx context.Context, absPath string) error {
	real, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// Not-yet-created file (or broken symlink): EvalSymlinks can't resolve
		// the leaf, so resolve the nearest existing ancestor and re-append the
		// trailing components. This keeps a brand-new file under a symlinked
		// root (e.g. macOS /var -> /private/var, or a symlinked checkout) inside
		// the root, while an out-of-repo target still resolves outside and is
		// refused below.
		real = resolveNearestExistingAncestor(absPath)
	}
	return s.guardResolvedPathWithinRepo(ctx, absPath, real)
}

// guardResolvedPathWithinRepo validates the resolved target that was actually
// observed, not a fresh resolution of the caller's path. Physical reads use
// this to bind repository confinement to the file handle whose bytes were
// hashed, even if a symlink is retargeted out of the repo and restored before
// the request returns.
func (s *Server) guardResolvedPathWithinRepo(ctx context.Context, requestedPath, resolvedPath string) error {
	real := filepath.Clean(resolvedPath)
	if view := requestViewPathRoot(ctx); view.root != "" {
		// A routed worktree is a single coherent filesystem view. A symlink
		// may be lexically inside it while resolving into the primary or a
		// sibling checkout; accepting the union of all tracked roots here
		// would then expose or mutate bytes from a different graph view.
		physicalRoot, err := filepath.EvalSymlinks(view.root)
		if err != nil || physicalRoot == "" {
			physicalRoot = resolveNearestExistingAncestor(view.root)
		}
		physicalRoot = filepath.Clean(physicalRoot)
		if pathContainedIn(real, physicalRoot) {
			return nil
		}
		return fmt.Errorf(
			"%w: %q resolves to %q, outside the selected checkout root %q",
			errPathEscape, requestedPath, real, physicalRoot)
	}

	roots := s.guardRepoRoots(ctx)
	if len(roots) == 0 {
		return nil // no known roots (control client / unindexed) — nothing to enforce
	}
	for _, root := range roots {
		if pathContainedIn(real, root) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q resolves to %q, outside every indexed repository root", errPathEscape, requestedPath, real)
}

func (s *Server) guardedResolvedPath(ctx context.Context, absPath string) (string, error) {
	if err := s.guardSymlinkWithinRepo(ctx, absPath); err != nil {
		return "", err
	}
	return absPath, nil
}

// resolveNearestExistingAncestor symlink-resolves the longest existing prefix
// of absPath and rejoins the non-existent trailing components, so the
// confinement check on a not-yet-created file compares like-for-like against
// the (symlink-resolved) repository roots. Falls back to the lexical clean of
// absPath when nothing along the chain resolves.
func resolveNearestExistingAncestor(absPath string) string {
	cur := filepath.Clean(absPath)
	var suffix string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return real
			}
			return filepath.Join(real, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(absPath) // reached the filesystem root unresolved
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

// guardRepoRoots returns the symlink-resolved root paths of every tracked repo
// (multi-repo) plus the lone indexer (single-repo), for guardSymlinkWithinRepo.
//
// A request routed to a worktree view adds that checkout's root: it is a
// working copy of a tracked repository the request was deliberately routed to,
// and it is where every path this request resolves now lands. Without it the
// confinement guard would refuse the view's own files.
func (s *Server) guardRepoRoots(ctx context.Context) []string {
	var roots []string
	add := func(root string) {
		if root == "" {
			return
		}
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		roots = append(roots, filepath.Clean(root))
	}
	if s.multiIndexer != nil {
		for _, prefix := range s.multiIndexer.RepoPrefixes() {
			if root, ok := s.multiIndexer.RepoRoot(prefix); ok {
				add(root)
			}
		}
	}
	if s.indexer != nil {
		add(s.indexer.RootPath())
	}
	add(requestViewPathRoot(ctx).root)
	return roots
}

// resolveNodePath returns the absolute filesystem path for a graph node.
// Uses node.RepoPrefix to find the owning repo's root in multi-repo mode;
// falls back to the lone indexer's RootPath in single-repo mode. Returns an
// error (not a relative path) when no repo root is available, to keep callers
// from passing a bare-relative path to os.Open and resolving against the
// daemon process CWD.
func (s *Server) resolveNodePath(ctx context.Context, node *graph.Node) (string, error) {
	if node == nil {
		return "", errors.New("nil node")
	}
	if requestReadsCommittedTree(ctx) {
		return "", errViewHasNoWorkingCopy
	}
	if node.FilePath == "" {
		return "", fmt.Errorf("node %q has no file path", node.ID)
	}
	if filepath.IsAbs(node.FilePath) {
		return s.guardedResolvedPath(ctx, filepath.Clean(node.FilePath))
	}
	if s.multiIndexer != nil {
		if root, ok := s.multiIndexer.RepoRoot(node.RepoPrefix); ok {
			// applyRepoPrefix stamps `<repoPrefix>/` onto node.FilePath
			// at index time, so a node's FilePath looks like
			// `gortex/internal/mcp/tools_fileops.go`. RepoRoot returns
			// the on-disk path that ALREADY corresponds to the repo
			// Joining as-is duplicates the prefix segment when the repo's basename
			// matches the prefix — strip the leading `<prefix>/` from
			// the file path before joining so the result is the real
			// on-disk file regardless of basename collision.
			rel := node.FilePath
			if node.RepoPrefix != "" {
				rel = strings.TrimPrefix(rel, node.RepoPrefix+"/")
			}
			abs := filepath.Clean(filepath.Join(root, rel))
			// Re-root onto the linked worktree the file belongs to —
			// same reasoning as resolveFilePath: worktrees of one repo
			// share an index identity, so a node's resolved path can
			// land on a sibling checkout.
			return s.guardedResolvedPath(ctx, s.checkoutRootedPath(ctx, abs, root, node.RepoPrefix))
		}
		return "", fmt.Errorf("could not resolve repo root for node %q (repo_prefix=%q)", node.ID, node.RepoPrefix)
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			abs := filepath.Clean(filepath.Join(root, node.FilePath))
			return s.guardedResolvedPath(ctx, requestViewPathRoot(ctx).rooted(abs, root))
		}
	}
	return "", fmt.Errorf("%w: node=%q file=%q", errPathUnresolved, node.ID, node.FilePath)
}

// withAbsPath returns a shallow copy of n with AbsoluteFilePath populated
// from the indexer roots. The canonical graph node is never mutated, so
// this is safe to call from concurrent request handlers; AbsoluteFilePath
// is left empty when the path cannot be resolved (callers still carry the
// repo-relative file_path).
func (s *Server) withAbsPath(ctx context.Context, n *graph.Node) *graph.Node {
	if n == nil {
		return nil
	}
	cp := *n
	if abs, err := s.resolveNodePath(ctx, n); err == nil {
		cp.AbsoluteFilePath = abs
	}
	return &cp
}

// withAbsPaths maps withAbsPath over a slice, returning a fresh slice of
// copies. The input slice and its nodes are left untouched.
func (s *Server) withAbsPaths(ctx context.Context, nodes []*graph.Node) []*graph.Node {
	if nodes == nil {
		return nil
	}
	out := make([]*graph.Node, len(nodes))
	for i, n := range nodes {
		out[i] = s.withAbsPath(ctx, n)
	}
	return out
}

// resolveGraphPath returns the absolute filesystem path for a repo-prefixed
// graph path (e.g. "gortex/internal/foo.go"). Mirrors resolveNodePath but
// works on raw path strings — used for edges, search results, and other
// references that don't carry a Node pointer. Returns errPathUnresolved
// rather than letting os.Open resolve against the daemon process CWD.
func (s *Server) resolveGraphPath(ctx context.Context, graphPath string) (string, error) {
	if graphPath == "" {
		return "", errors.New("empty path")
	}
	if requestReadsCommittedTree(ctx) {
		return "", errViewHasNoWorkingCopy
	}
	if filepath.IsAbs(graphPath) {
		abs := filepath.Clean(graphPath)
		if viewErr := requestViewPathRoot(ctx).validateAbsolute(abs); viewErr != nil {
			return "", viewErr
		}
		return s.guardedResolvedPath(ctx, abs)
	}
	if s.multiIndexer != nil {
		if abs := s.multiIndexer.ResolveFilePath(graphPath); abs != "" {
			abs = filepath.Clean(abs)
			// Re-root onto the checkout this request reads.
			// ResolveFilePath joins against the matched prefix's root;
			// recover that root so the checkout choice has an anchor.
			if prefix := matchedRepoPrefix(s.multiIndexer, graphPath); prefix != "" {
				if root, ok := s.multiIndexer.RepoRoot(prefix); ok {
					abs = s.checkoutRootedPath(ctx, abs, root, prefix)
				}
			} else if _, root, ok := soleTrackedRepo(s.multiIndexer); ok {
				// A lone repo's graph paths are spelled without a prefix, so
				// there is no prefix to recover the root from — but a routed
				// view still has to move the path into its own checkout.
				abs = requestViewPathRoot(ctx).rooted(abs, root)
			}
			return s.guardedResolvedPath(ctx, abs)
		}
		return "", fmt.Errorf("could not resolve repo root for path %q", graphPath)
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			abs := filepath.Clean(filepath.Join(root, graphPath))
			return s.guardedResolvedPath(ctx, requestViewPathRoot(ctx).rooted(abs, root))
		}
	}
	return "", fmt.Errorf("%w: path=%q", errPathUnresolved, graphPath)
}

// graphRelPath normalises a caller-supplied file path to the
// repo-relative form the graph stores its nodes under — repo-prefixed in
// multi-repo mode (internal/x.go -> gortex/internal/x.go), unchanged in
// single-repo mode, and converted from an absolute path when one is
// given. It is the read-side companion to resolveFilePath's relPath:
// both run the same anchoring, but graphRelPath never errors — a path
// that cannot be anchored (e.g. a not-yet-created file) is returned
// as-is so a graph lookup keyed on it degrades to exactly the raw-path
// behaviour callers had before, never worse. Use it before GetFileSymbols
// / FileEditingContext so a repo-relative path doesn't silently miss the
// prefixed nodes in multi-repo mode.
func (s *Server) graphRelPath(ctx context.Context, fp string) string {
	if files := refViewFilesFor(ctx); files != nil {
		if rel, err := files.relPath(fp); err == nil {
			return files.graphPath(rel)
		}
		return fp
	}
	if _, rel, err := s.resolveFilePath(ctx, fp); err == nil && rel != "" {
		return s.graphPathSpelling(rel)
	}
	return fp
}

// graphPathSpelling renders a repo-relative path the way graph node IDs
// spell it: the repo prefix joined with "/", the repo-relative remainder in
// the OS separator. resolveFilePath echoes a caller-supplied relative path
// back verbatim, so without this a forward-slash path — the spelling every
// agent writes — missed every node below the repo root on Windows and the
// read tools answered file_not_indexed for an indexed file. Identity on
// POSIX, where the two separators already agree.
func (s *Server) graphPathSpelling(rel string) string {
	prefixed := false
	if s.multiIndexer != nil {
		prefixed = matchedRepoPrefix(s.multiIndexer, rel) != ""
	}
	return graphMatchPathKey(rel, prefixed)
}

// fileAttributionNode synthesizes a node carrying just the repo prefix
// and language of a file — enough for tokenStats.record to attribute an
// observation to the right per-repo / per-language bucket when no real
// symbol node is in hand.
func (s *Server) fileAttributionNode(relPath, language string) *graph.Node {
	prefix := ""
	if s.multiIndexer != nil {
		prefix = matchedRepoPrefix(s.multiIndexer, relPath)
		// Lone-repo attribution applies only to repo-relative paths —
		// an absolute path that matched no prefix points outside every
		// tracked repo and must stay unattributed rather than polluting
		// the lone repo's buckets.
		if prefix == "" && !filepath.IsAbs(relPath) {
			if prefixes := s.multiIndexer.RepoPrefixes(); len(prefixes) == 1 {
				prefix = prefixes[0]
			}
		}
	}
	return &graph.Node{RepoPrefix: prefix, Language: language, FilePath: relPath}
}

// savingsAttributionNode fills the per-repo attribution for symbol
// nodes minted in single-repo mode (RepoPrefix="") so symbol-tool
// events land in the same per-repo bucket the read-family tools use.
// The graph node itself is never mutated.
func (s *Server) savingsAttributionNode(node *graph.Node) *graph.Node {
	if node == nil || node.RepoPrefix != "" || s.multiIndexer == nil {
		return node
	}
	prefixes := s.multiIndexer.RepoPrefixes()
	if len(prefixes) != 1 {
		return node
	}
	cp := *node
	cp.RepoPrefix = prefixes[0]
	return &cp
}

// recordFileBaselineSavings books a savings observation for a tool whose
// response stands in for reading a whole file. payload is the response
// content actually produced — used both for the returned-token count and
// as the chars-per-token calibration sample — and the baseline is the
// on-disk byte size of the file the agent would otherwise have read.
// Best-effort accounting: files that don't resolve or stat book nothing.
func (s *Server) recordFileBaselineSavings(ctx context.Context, tool, relPath, language, payload string) {
	abs, err := s.resolveGraphPath(ctx, relPath)
	if err != nil {
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return
	}
	returned := tokens.CachedCountInt64(payload)
	fullFile := int64(tokens.EstimateFromSample(int(info.Size()), payload))
	s.tokenStatsFor(ctx).record(s.fileAttributionNode(relPath, language), tool, returned, fullFile)
}

// repoRelative converts an absolute path to a repo-prefixed or root-relative
// string if it falls under any indexed repo, otherwise returns the absolute
// path unchanged.
func (s *Server) repoRelative(absPath string) string {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			if idx := s.multiIndexer.GetIndexer(prefix); idx != nil {
				if rel, err := filepath.Rel(idx.RootPath(), absPath); err == nil {
					return filepath.ToSlash(filepath.Join(prefix, rel))
				}
			}
			return prefix
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, err := filepath.Rel(root, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return absPath
}

func incrementalReindexSucceeded(result *indexer.IndexResult, err error) bool {
	return err == nil && result != nil && len(result.FailedFiles) == 0
}

// reindexFile refreshes the graph for a single file after a write. Best-effort:
// non-source files or files outside any indexed repo are silently skipped.
func (s *Server) reindexFile(absPath string) bool {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			result, err := s.multiIndexer.IncrementalReindexRepo(prefix, []string{absPath})
			if incrementalReindexSucceeded(result, err) {
				return true
			}
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, err := filepath.Rel(root, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				result, reindexErr := s.indexer.IncrementalReindexPaths(root, []string{absPath})
				if incrementalReindexSucceeded(result, reindexErr) {
					return true
				}
			}
		}
	}
	return false
}

// fileSyntaxHealth reads back the parse-error stamp the indexer places on a
// file node during (re)indexing, and returns a syntax_health block to attach
// to an edit response — but ONLY when the file now has parse errors, so a
// clean edit stays quiet. This lets an agent notice immediately that an edit
// left the file syntactically broken, before it trusts graph queries against
// it. Returns nil when the file parsed cleanly or its health is unknown.
func (s *Server) fileSyntaxHealth(
	ctx context.Context,
	relPath, absPath string,
	outcome mutationReindexOutcome,
) map[string]any {
	reader := s.requestBaseReader(ctx)
	if outcome.CheckoutID != "" {
		materializer := s.Materializer()
		if materializer == nil || materializer.Catalog == nil || outcome.PublishedRouteEpoch <= 0 {
			return nil
		}
		route, found, err := materializer.Catalog.GetCheckoutRoute(ctx, outcome.CheckoutID)
		if err != nil || !found || !graphview.RouteReady(route) || route.RouteEpoch != outcome.PublishedRouteEpoch {
			return nil
		}
		view, err := materializer.MaterializeCheckout(ctx, outcome.CheckoutID)
		if err != nil || view == nil {
			return nil
		}
		defer view.Close()
		route, found, err = materializer.Catalog.GetCheckoutRoute(ctx, outcome.CheckoutID)
		if err != nil || !found || !graphview.RouteReady(route) || route.RouteEpoch != outcome.PublishedRouteEpoch {
			return nil
		}
		reader = view.Reader
	}
	if reader == nil {
		return nil
	}
	graphPath := s.resolveOverlayGraphPath(relPath, absPath)
	// Read the request-scoped published generation: routed mutations stamp
	// syntax health in the selected checkout, while canonical requests retain
	// the canonical graph reader.
	for _, n := range reader.GetFileNodes(graphPath) {
		if n == nil || n.Kind != graph.KindFile || n.Meta == nil {
			continue
		}
		broken, _ := n.Meta["has_parse_errors"].(bool)
		if !broken {
			continue
		}
		count := 0
		switch v := n.Meta["parse_errors"].(type) {
		case int:
			count = v
		case int64:
			count = int(v)
		case float64:
			count = int(v)
		}
		return map[string]any{
			"healthy":      false,
			"parse_errors": count,
			"warning": fmt.Sprintf(
				"file has %d parse error(s) after this edit — it may be syntactically broken; review before relying on graph queries for it.",
				count),
		}
	}
	return nil
}

func (s *Server) handleEditFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	oldString, err := req.RequireString("old_string")
	if err != nil {
		return mcp.NewToolResultError("old_string is required"), nil
	}
	newString, err := req.RequireString("new_string")
	if err != nil {
		return mcp.NewToolResultError("new_string is required"), nil
	}
	if oldString == newString {
		return mcp.NewToolResultError("old_string and new_string are identical"), nil
	}
	replaceAll := req.GetBool("replace_all", false)
	dryRun := req.GetBool("dry_run", false)
	baseSHA := normalizeExpectedSHA(req.GetString("base_sha", ""))
	evidenceRequested, evidenceErr := parsePhysicalEvidenceRequest(req)
	if evidenceErr != nil {
		return mcp.NewToolResultError(evidenceErr.Error()), nil
	}
	if evidenceRequested && dryRun {
		return mcp.NewToolResultError(errEvidenceDryRun), nil
	}
	mutationID := strings.TrimSpace(req.GetString("mutation_id", ""))

	absPath, relPath, resolveErr := s.resolveFilePath(ctx, rawPath)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}
	releaseMutation, lockErr := acquireMutationPath(ctx, absPath)
	if lockErr != nil {
		return mcp.NewToolResultError("edit cancelled while waiting for exclusive file access: " + lockErr.Error()), nil
	}
	defer releaseMutation()

	// Replay before reading the file: a landed edit has already consumed
	// old_string, so an un-replayed retry would fail the match check with a
	// confusing "not found" instead of returning the original result.
	fingerprint := mutationFingerprint("edit_file", absPath, oldString, newString, strconv.FormatBool(replaceAll))
	if replay, conflict, matched := s.replayMutation(mutationID, fingerprint); matched {
		if conflict != "" {
			return mcp.NewToolResultError(conflict), nil
		}
		return s.respondJSONOrTOON(ctx, req, replay)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", err)), nil
	}
	// Drift guard: when the caller observed the file at base_sha,
	// refuse to apply on top of a divergent on-disk SHA. Match the
	// overlay-push error shape so callers can re-read and retry.
	if baseSHA != "" && gitBlobSHA(content) != baseSHA {
		return mcp.NewToolResultError(errBaseSHADrift), nil
	}
	// Before anything is written, so a withheld receipt never leaves a
	// half-applied edit behind.
	if evidenceRequested {
		if err := s.guardMutationEvidenceIntent(s.detectLanguageForPath(ctx, absPath, relPath), relPath, content); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	fileStr := string(content)

	matches := findEOLMatches(fileStr, oldString)
	count := matches.count
	if expected := req.GetInt("expected_occurrences", 0); expected > 0 && count != expected {
		return mcp.NewToolResultError(fmt.Sprintf(
			"expected_occurrences=%d but old_string matches %d location(s) — refusing the edit so a wrong-cardinality replacement can't slip through. Adjust the fragment or the expected count.",
			expected, count)), nil
	}
	if count == 0 {
		return mcp.NewToolResultError(
			"old_string not found in file. Use get_file_summary or get_editing_context to inspect the current content."), nil
	}
	if count > 1 && !replaceAll {
		hint := matchSpansHint(fileStr, matches.spans)
		return mcp.NewToolResultError(fmt.Sprintf(
			"old_string matches %d locations%s. Provide a larger fragment for uniqueness or pass replace_all=true.", count, hint)), nil
	}

	var newContent string
	var replacements int
	switch {
	case matches.normalized:
		// The CRLF<->LF fallback matched: splice the real byte spans and
		// write new_string with each region's own line terminators so the
		// edit never introduces mixed endings.
		limit := 1
		replacements = 1
		if replaceAll {
			limit = -1
			replacements = count
		}
		newContent = spliceSpansEOL(fileStr, matches.spans, newString, limit)
		if newContent == fileStr {
			return mcp.NewToolResultError(
				"old_string and new_string are identical after line-ending normalization"), nil
		}
	case replaceAll:
		newContent = strings.ReplaceAll(fileStr, oldString, newString)
		replacements = count
	default:
		newContent = strings.Replace(fileStr, oldString, newString, 1)
		replacements = 1
	}

	newContentBytes := []byte(newContent)
	newSHA := gitBlobSHA(newContentBytes)

	allowParseErrors := req.GetBool("allow_parse_errors", false)
	var gate parseGateResult
	if parseGateEnabled() {
		gate = checkParseGate(relPath, content, newContentBytes)
		if gate.Blocked && !allowParseErrors && !dryRun {
			return mcp.NewToolResultError(parseGateError(relPath, gate)), nil
		}
	}

	if dryRun {
		// Dry-run: validate everything but skip the write + reindex.
		// Returns the same shape so callers can branch on dry_run for a
		// preview before committing.
		preview := map[string]any{
			"path":          relPath,
			"status":        "would_apply",
			"dry_run":       true,
			"replacements":  replacements,
			"bytes_written": len(newContentBytes),
			"reindexed":     false,
			"diff":          unifiedDiff(relPath, fileStr, newContent),
			"new_sha":       newSHA,
		}
		if matches.normalized {
			preview["eol_normalized"] = true
		}
		if info := parseGateInfo(gate, allowParseErrors); info != nil {
			preview["parse_gate"] = info
		}
		return s.respondJSONOrTOON(ctx, req, preview)
	}

	perm := os.FileMode(0o644)
	if info, err := os.Stat(absPath); err == nil {
		perm = info.Mode().Perm()
	}
	commit, writeErr := s.commitFileMutation(ctx, "edit_file", mutationID, fingerprint, relPath, absPath, newContentBytes, perm)
	if writeErr != nil {
		if errors.Is(writeErr, errMutationNotApplied) {
			return mcp.NewToolResultError(mutationNotAppliedMessage("edit", commit, writeErr)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("could not write file: %v", writeErr)), nil
	}

	sess := s.sessionFor(ctx)
	sess.recordModified(relPath)

	reindexOutcome := s.mutationReindexState(ctx, absPath)
	commit.recordGraph(reindexOutcome)

	resp := map[string]any{
		"path":          relPath,
		"status":        "applied",
		"replacements":  replacements,
		"bytes_written": len(newContentBytes),
		"new_sha":       newSHA,
	}
	if matches.normalized {
		resp["eol_normalized"] = true
	}
	if info := parseGateInfo(gate, allowParseErrors); info != nil {
		resp["parse_gate"] = info
	}
	if reindexOutcome.Err != nil {
		resp["reindex_error"] = reindexOutcome.Err.Error()
	}
	s.attachMutationFreshness(ctx, resp, relPath, absPath, reindexOutcome)
	if evidenceRequested {
		s.attachMutationPhysicalEvidence(ctx, resp, absPath, content, true)
	}
	attachMutationCommit(resp, commit)
	// Retained last so a replayed retry returns the complete original payload,
	// physical evidence included.
	commit.retainResponse(resp)
	return s.respondJSONOrTOON(ctx, req, resp)
}

func (s *Server) handleWriteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	content, err := req.RequireString("content")
	if err != nil {
		return mcp.NewToolResultError("content is required"), nil
	}
	dryRun := req.GetBool("dry_run", false)
	baseSHA := normalizeExpectedSHA(req.GetString("base_sha", ""))
	evidenceRequested, evidenceErr := parsePhysicalEvidenceRequest(req)
	if evidenceErr != nil {
		return mcp.NewToolResultError(evidenceErr.Error()), nil
	}
	if evidenceRequested && dryRun {
		return mcp.NewToolResultError(errEvidenceDryRun), nil
	}
	mutationID := strings.TrimSpace(req.GetString("mutation_id", ""))

	absPath, relPath, resolveErr := s.resolveFilePath(ctx, rawPath)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}
	releaseMutation, lockErr := acquireMutationPath(ctx, absPath)
	if lockErr != nil {
		return mcp.NewToolResultError("write cancelled while waiting for exclusive file access: " + lockErr.Error()), nil
	}
	defer releaseMutation()

	// Idempotency runs under the path lock so a retry that races the original
	// call waits for it and then sees a terminal record, rather than both
	// deciding independently that they are the first.
	fingerprint := mutationFingerprint("write_file", absPath, content)
	if replay, conflict, matched := s.replayMutation(mutationID, fingerprint); matched {
		if conflict != "" {
			return mcp.NewToolResultError(conflict), nil
		}
		return s.respondJSONOrTOON(ctx, req, replay)
	}

	status := "created"
	perm := os.FileMode(0o644)
	fileExists := false
	if info, err := os.Stat(absPath); err == nil {
		if info.IsDir() {
			return mcp.NewToolResultError(fmt.Sprintf("path %q is a directory", rawPath)), nil
		}
		status = "overwritten"
		perm = info.Mode().Perm()
		fileExists = true
	}

	// Drift guard: when the caller observed the file at base_sha,
	// refuse to overwrite a divergent on-disk file. If the caller
	// passed base_sha for a file that no longer exists, that is
	// also drift (the file they read is gone).
	if baseSHA != "" {
		if !fileExists {
			return mcp.NewToolResultError(errBaseSHADrift), nil
		}
		existing, readErr := os.ReadFile(absPath)
		if readErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not read file for drift check: %v", readErr)), nil
		}
		if gitBlobSHA(existing) != baseSHA {
			return mcp.NewToolResultError(errBaseSHADrift), nil
		}
	}

	contentBytes := []byte(content)
	newSHA := gitBlobSHA(contentBytes)

	// Read the prior bytes once: the parse gate compares against them, and an
	// evidence receipt digests them as before_sha256.
	var priorContent []byte
	if fileExists {
		priorContent, _ = os.ReadFile(absPath)
	}
	// Before anything is written, so a withheld receipt never leaves an
	// overwritten file behind.
	if evidenceRequested && fileExists {
		if err := s.guardMutationEvidenceIntent(s.detectLanguageForPath(ctx, absPath, relPath), relPath, priorContent); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	allowParseErrors := req.GetBool("allow_parse_errors", false)
	var gate parseGateResult
	if parseGateEnabled() {
		gate = checkParseGate(relPath, priorContent, contentBytes)
		if gate.Blocked && !allowParseErrors && !dryRun {
			return mcp.NewToolResultError(parseGateError(relPath, gate)), nil
		}
	}

	if dryRun {
		// Dry-run: skip the write + reindex but report what would happen,
		// including a unified-diff preview of the change.
		dryStatus := "would_create"
		oldContent := ""
		if status == "overwritten" {
			dryStatus = "would_overwrite"
			if existing, e := os.ReadFile(absPath); e == nil {
				oldContent = string(existing)
			}
		}
		preview := map[string]any{
			"path":          relPath,
			"status":        dryStatus,
			"dry_run":       true,
			"bytes_written": len(contentBytes),
			"reindexed":     false,
			"diff":          unifiedDiff(relPath, oldContent, content),
			"new_sha":       newSHA,
		}
		if info := parseGateInfo(gate, allowParseErrors); info != nil {
			preview["parse_gate"] = info
		}
		return s.respondJSONOrTOON(ctx, req, preview)
	}

	commit, writeErr := s.commitFileMutation(ctx, "write_file", mutationID, fingerprint, relPath, absPath, contentBytes, perm)
	if writeErr != nil {
		if errors.Is(writeErr, errMutationNotApplied) {
			return mcp.NewToolResultError(mutationNotAppliedMessage("write", commit, writeErr)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("could not write file: %v", writeErr)), nil
	}

	sess := s.sessionFor(ctx)
	sess.recordModified(relPath)

	reindexOutcome := s.mutationReindexState(ctx, absPath)
	commit.recordGraph(reindexOutcome)

	resp := map[string]any{
		"path":          relPath,
		"status":        status,
		"bytes_written": len(contentBytes),
		"new_sha":       newSHA,
	}
	if info := parseGateInfo(gate, allowParseErrors); info != nil {
		resp["parse_gate"] = info
	}
	if reindexOutcome.Err != nil {
		resp["reindex_error"] = reindexOutcome.Err.Error()
	}
	s.attachMutationFreshness(ctx, resp, relPath, absPath, reindexOutcome)
	if evidenceRequested {
		s.attachMutationPhysicalEvidence(ctx, resp, absPath, priorContent, fileExists)
	}
	attachMutationCommit(resp, commit)
	// Retained last so a replayed retry returns the complete original payload,
	// physical evidence included.
	commit.retainResponse(resp)
	return s.respondJSONOrTOON(ctx, req, resp)
}

// matchLocationsHint returns a brief " (first match lines X, Y, Z)" hint
// listing up to three line numbers where oldString matches in fileStr
// (EOL-tolerant). Empty when there are zero matches. Helps an agent choose
// a more unique fragment without re-reading the file.
func matchLocationsHint(fileStr, oldString string) string {
	return matchSpansHint(fileStr, findEOLMatches(fileStr, oldString).spans)
}

// handleReadFile returns the full content of a file as a string,
// optionally rewritten through the tree-sitter elider when
// compress_bodies=true. Path resolution shares the same rules as
// edit_file / write_file (absolute, repo-prefixed, or
// single-repo-root-relative); the file does not need to be indexed.
// fileDependents returns the distinct source files that import the given file —
// the files a change to it would ripple into. Only import edges into the file
// node count, so the header reflects real file-to-file dependencies.
func (s *Server) fileDependents(ctx context.Context, fileID string) []string {
	g := s.readerFor(ctx)
	if g == nil || fileID == "" {
		return nil
	}
	seen := map[string]bool{}
	var deps []string
	for _, e := range g.GetInEdges(fileID) {
		if e == nil || e.Kind != graph.EdgeImports {
			continue
		}
		f := fileOfNode(g, e.From)
		if f == "" || f == fileID || seen[f] {
			continue
		}
		seen[f] = true
		deps = append(deps, f)
	}
	sort.Strings(deps)
	return deps
}

// fileOfNode returns the source file a node belongs to: a file node is its own
// file, any other node reports its FilePath; falls back to the id.
func fileOfNode(g graph.Reader, id string) string {
	if n := g.GetNode(id); n != nil {
		if n.Kind == graph.KindFile {
			return n.ID
		}
		if n.FilePath != "" {
			return n.FilePath
		}
	}
	return id
}

// fileDependentsNote renders a one-line dependents header: how many files import
// this file, and the first few of them.
func fileDependentsNote(deps []string) string {
	if len(deps) == 0 {
		return ""
	}
	shown := deps
	suffix := ""
	if len(shown) > 5 {
		shown = shown[:5]
		suffix = fmt.Sprintf(" (+%d more)", len(deps)-5)
	}
	noun := "files import"
	if len(deps) == 1 {
		noun = "file imports"
	}
	return fmt.Sprintf("↑ %d %s this file: %s%s", len(deps), noun, strings.Join(shown, ", "), suffix)
}

// attachFileDependents records the dependents list + one-line header on a file
// tool's result map, when the file has any importers.
func (s *Server) attachFileDependents(ctx context.Context, result map[string]any, fileID string) {
	if deps := s.fileDependents(ctx, fileID); len(deps) > 0 {
		result["dependents"] = deps
		result["dependents_header"] = fileDependentsNote(deps)
	}
}

// windowFileLines slices content to the 1-based line window [offset,
// offset+limit). offset <= 0 starts at line 1; limit <= 0 reads to EOF.
// Returns the windowed bytes, whether a window was applied, and the
// 1-based inclusive start/end lines plus the file's total line count. A
// trailing newline's phantom final line is excluded from the count and the
// window, so total_lines matches what an editor shows. An offset past EOF
// yields empty content (not an error).
func windowFileLines(content []byte, offset, limit int) (out []byte, applied bool, start, end, total int) {
	if offset <= 0 && limit <= 0 {
		return content, false, 0, 0, 0
	}
	lines := strings.Split(string(content), "\n")
	total = len(lines)
	if total > 0 && lines[total-1] == "" {
		// Trailing newline produced a phantom empty final element — don't
		// count it as a line.
		total--
	}
	start = offset
	if start < 1 {
		start = 1
	}
	if start > total {
		return []byte{}, true, start, start - 1, total
	}
	end = total
	if limit > 0 {
		if e := start + limit - 1; e < end {
			end = e
		}
	}
	return []byte(strings.Join(lines[start-1:end], "\n")), true, start, end, total
}

func capReadFileContent(content []byte, maxChars int, binary bool) ([]byte, bool) {
	if maxChars <= 0 || len(content) <= maxChars {
		return content, false
	}
	prefix := content[:maxChars]
	if binary {
		return prefix, true
	}
	// max_chars is an encoded-response budget. If the byte boundary falls
	// inside a multi-byte rune, discard only that incomplete trailing rune.
	return []byte(strings.ToValidUTF8(string(prefix), "")), true
}

type physicalReadEvidence struct {
	resolvedPath    string
	contentSHA256   string
	byteCount       int
	symlinkResolved bool
	readAt          time.Time
}

func samePhysicalFileVersion(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() &&
		os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// readAllSized reads f into a buffer presized from the size the caller already
// observed on the open handle, so the whole-file read costs one allocation
// instead of io.ReadAll's repeated append-and-copy growth (measured 2.1x the
// file size in total allocation for a 128 MiB file, 1.0x once presized). The
// hint is advisory: a file that grew since the stat still reads completely,
// and an implausible size falls back to unhinted growth.
func readAllSized(f *os.File, size int64) ([]byte, error) {
	var buf bytes.Buffer
	if size > 0 && size < math.MaxInt32 {
		buf.Grow(int(size) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readPhysicalFileEvidence hashes the exact buffer returned to the caller from
// one file-handle read. Metadata and path identity checks bound replacement or
// in-place drift during that read without doubling file I/O or peak memory.
func readPhysicalFileEvidence(absPath string) ([]byte, physicalReadEvidence, error) {
	return readPhysicalFileEvidenceObserved(absPath, nil)
}

func readPhysicalFileEvidenceObserved(absPath string, afterRead func()) ([]byte, physicalReadEvidence, error) {
	linkInfo, err := os.Lstat(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not inspect physical path: %w", err)
	}
	if !linkInfo.Mode().IsRegular() && linkInfo.Mode()&os.ModeSymlink == 0 {
		return nil, physicalReadEvidence{}, fmt.Errorf("physical evidence requires a regular file, got %s", linkInfo.Mode().Type())
	}
	resolvedBefore, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not resolve physical file: %w", err)
	}
	f, err := openPhysicalEvidenceFile(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not open physical file: %w", err)
	}
	defer f.Close()

	before, err := f.Stat()
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not stat physical file: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, physicalReadEvidence{}, fmt.Errorf("physical evidence requires a regular file, got %s", before.Mode().Type())
	}
	content, err := readAllSized(f, before.Size())
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not read physical file: %w", err)
	}
	sum := sha256.Sum256(content)
	if afterRead != nil {
		afterRead()
	}

	// Verify the snapshot with a second streaming hash on the same handle.
	// This doubles I/O only for explicit physical evidence, while retaining one
	// full buffer and detecting in-place same-size rewrites whose mtime was
	// restored. Metadata-only checks cannot prove that invariant.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not rewind physical file for verification: %w", err)
	}
	verificationHash := sha256.New()
	if _, err := io.Copy(verificationHash, f); err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not verify physical file content: %w", err)
	}
	if !bytes.Equal(sum[:], verificationHash.Sum(nil)) {
		return nil, physicalReadEvidence{}, errors.New("physical file changed while it was being read; retry")
	}

	after, err := f.Stat()
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not restat physical file: %w", err)
	}
	pathInfo, err := os.Stat(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not verify physical path: %w", err)
	}
	resolvedAfter, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, physicalReadEvidence{}, fmt.Errorf("could not verify physical path resolution: %w", err)
	}
	if filepath.Clean(resolvedBefore) != filepath.Clean(resolvedAfter) ||
		!samePhysicalFileVersion(before, after) ||
		!samePhysicalFileVersion(after, pathInfo) {
		return nil, physicalReadEvidence{}, errors.New("physical file changed while it was being read; retry")
	}

	return content, physicalReadEvidence{
		resolvedPath:    resolvedAfter,
		contentSHA256:   fmt.Sprintf("%x", sum),
		byteCount:       len(content),
		symlinkResolved: linkInfo.Mode()&os.ModeSymlink != 0,
		readAt:          time.Now().UTC(),
	}, nil
}

func (s *Server) handleReadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	// Admission before any work: a fidelity_globs value that breaks a size
	// bound refuses the request. Dropping the offending rule would rewrite
	// a first-match policy silently — an over-budget `omit` disappearing
	// lets a later `full` win, and the content the caller asked to hide
	// comes back in a response that looks like a normal success. Parsed
	// here rather than at the point of use so a malformed request cannot
	// be served by a path that happens not to reach the compressor.
	fidelityRules, fidelityErr := parseFidelityGlobs(req.GetString("fidelity_globs", ""))
	if fidelityErr != nil {
		return mcp.NewToolResultError("read_file: " + fidelityErr.Error()), nil
	}
	physicalEvidenceRequested, evidenceErr := parsePhysicalEvidenceRequest(req)
	if evidenceErr != nil {
		return mcp.NewToolResultError(evidenceErr.Error()), nil
	}

	var (
		absPath, relPath  string
		viewURI           string
		content           []byte
		diskContent       []byte
		physicalEvidence  physicalReadEvidence
		servedFromOverlay bool
	)

	if files := refViewFilesFor(ctx); files != nil {
		// The view is a committed tree, so there is no file on disk to stat,
		// hash or confine — the bytes come out of the object store and the
		// location they came from is the view's own identity.
		if physicalEvidenceRequested {
			return mcp.NewToolResultError(
				"physical_evidence describes a file on disk, and this request reads a committed tree " +
					"that is not checked out anywhere"), nil
		}
		bytes, rel, viewErr := readViewFile(ctx, files, rawPath)
		if viewErr != nil {
			return mcp.NewToolResultError(viewErr.Error()), nil
		}
		content = bytes
		relPath = files.graphPath(rel)
		viewURI = files.uri(rel)
		// The URI stands in for the absolute path in the language probe: it
		// carries the extension, and nothing can open it.
		absPath = viewURI
	} else {
		var resolveErr error
		absPath, relPath, resolveErr = s.resolveFilePath(ctx, rawPath)
		if resolveErr != nil {
			return mcp.NewToolResultError(resolveErr.Error()), nil
		}
		if guardErr := s.guardSymlinkWithinRepo(ctx, absPath); guardErr != nil {
			return mcp.NewToolResultError(guardErr.Error()), nil
		}
		info, statErr := os.Stat(absPath)
		if statErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("could not stat file: %v", statErr)), nil
		}
		if info.IsDir() {
			return mcp.NewToolResultError(fmt.Sprintf("path %q is a directory", rawPath)), nil
		}

		if physicalEvidenceRequested {
			var readErr error
			read := readPhysicalFileEvidence
			if s.physicalEvidenceOverride != nil {
				read = s.physicalEvidenceOverride
			}
			diskContent, physicalEvidence, readErr = read(absPath)
			if readErr != nil {
				return mcp.NewToolResultError(readErr.Error()), nil
			}
			// Bind confinement to the resolved target that supplied the hashed
			// bytes. Re-resolving only absPath is insufficient when a symlink is
			// retargeted outside the repo for the read and restored afterward.
			if guardErr := s.guardResolvedPathWithinRepo(ctx, absPath, physicalEvidence.resolvedPath); guardErr != nil {
				return mcp.NewToolResultError(guardErr.Error()), nil
			}
			// Also reject a path retargeted outside after the evidence snapshot.
			if guardErr := s.guardSymlinkWithinRepo(ctx, absPath); guardErr != nil {
				return mcp.NewToolResultError(guardErr.Error()), nil
			}
		}

		// Honour the editor-buffer overlay if one is active for this path. A
		// drifted overlay is already rejected upstream by the overlay view
		// guard; what reaches here is a live buffer, which we flag as such so
		// the caller knows the bytes are an unsaved editor view, not disk.
		if buf, ok := s.overlayContentFor(ctx, absPath); ok {
			content = []byte(buf)
			servedFromOverlay = true
		} else if physicalEvidenceRequested {
			content = diskContent
		} else {
			b, rerr := os.ReadFile(absPath)
			if rerr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", rerr)), nil
			}
			content = b
		}
	}

	originalBytes := len(content)

	// Line-window: when offset/limit are given, return only that slice of
	// the file's lines. This is the bounded-read path for large files — the
	// salience/compress transforms below reshape a whole body, whereas a
	// window is a plain "lines M..N" cut the caller asked for explicitly.
	windowed := false
	var winStart, winEnd, winTotal int
	if offset := req.GetInt("offset", 0); offset > 0 || req.GetInt("limit", 0) > 0 {
		content, windowed, winStart, winEnd, winTotal = windowFileLines(content, offset, req.GetInt("limit", 0))
	}

	isBinary := looksBinary(content)
	bodiesElided := false
	var keptSymbols []string
	language := s.detectLanguageForPath(ctx, absPath, relPath)
	// Tool-call observer: credit the recent search for the symbols in
	// the file the agent is reading.
	s.creditFileConsumption(ctx, relPath)
	// File symbols power both the `keep` predicate and frecency credit.
	sg := s.engineFor(ctx).GetFileSymbols(relPath)
	if req.GetBool("compress_bodies", false) && language != "" && elide.IsSupported(language) {
		var symbols []*graph.Node
		if sg != nil {
			symbols = sg.Nodes
		}
		keepPred, resolved := resolveKeepPredicate(req.GetString("keep", ""), symbols)
		decide := fidelityDecideForPath(fidelityRules, relPath)
		if out, eerr := elide.CompressWith(content, language, elide.Options{Keep: keepPred, Decide: decide}); eerr == nil && len(out) != len(content) {
			content = out
			bodiesElided = true
			keptSymbols = resolved
		}
	}

	// Salience truncation: collapse leaf-statement runs (and head-cut
	// non-code files) when the file is still over the max_lines budget.
	salienceTruncated := false
	if maxLines := req.GetInt("max_lines", 0); maxLines > 0 {
		if out, truncated, _ := elide.SalienceTruncate(content, language, maxLines); truncated {
			content = out
			salienceTruncated = true
		}
	}

	// Record the access for frecency credit on any node defined in
	// this file. read_file is a heavy access (full file), so we
	// credit every defined symbol — keeps the "agent is working in
	// this area" signal aligned with how the agent burned its
	// budget.
	s.sessionFor(ctx).recordFile(relPath)
	if sg != nil {
		for _, n := range sg.Nodes {
			if n == nil || n.Kind == graph.KindFile {
				continue
			}
			s.frecency.Record(n.ID)
		}
	}

	// Withhold secret-shaped values from config / data-leaf files unless the
	// caller explicitly opts out. Keys stay readable; only secret-shaped values
	// are replaced.
	secretsRedacted := false
	allowSecrets := req.GetBool("allow_secrets", false)
	if !isBinary {
		if red, did := s.maybeRedactConfigLeaf(language, relPath, allowSecrets, string(content)); did {
			content = []byte(red)
			secretsRedacted = true
		}
	}
	// The digest's scope is the full disk buffer, so the secret-intent gate has
	// to be judged on those bytes rather than on whatever survived into the
	// response. Keying it off secretsRedacted let any transform that dropped the
	// secret-shaped lines first — an offset/limit window, a max_lines cut —
	// leave hits at zero, so the gate stayed silent and the full-file digest of
	// a secrets file shipped anyway. Judged on diskContent the window no longer
	// matters. An overlay buffer carrying secrets the file on disk does not is
	// now allowed through, which is correct: the digest describes disk, and the
	// overlay bytes are still redacted in the response.
	if physicalEvidenceRequested && !allowSecrets {
		if _, wouldRedact := s.maybeRedactConfigLeaf(language, relPath, false, string(diskContent)); wouldRedact {
			return mcp.NewToolResultError("physical_evidence for redacted content requires allow_secrets=true"), nil
		}
	}

	maxChars := req.GetInt("max_chars", 0)
	content, contentTruncated := capReadFileContent(content, maxChars, isBinary)

	result := map[string]any{
		"path":           relPath,
		"language":       language,
		"bytes":          len(content),
		"original_bytes": originalBytes,
		"content":        string(content),
	}
	if contentTruncated {
		result["content_truncated"] = true
		result["max_chars"] = maxChars
	}
	if secretsRedacted {
		result["secrets_redacted"] = true
	}
	if servedFromOverlay {
		result["served_from"] = "overlay"
	}
	if viewURI != "" {
		// The file has no location outside the view, so the view's identity is
		// the location: no absolute path is reported, and the one that is
		// reported resolves only against this exact content.
		result["served_from"] = "view"
		result["view_uri"] = viewURI
	}
	if bodiesElided {
		result["bodies_elided"] = true
		if len(keptSymbols) > 0 {
			result["kept_symbols"] = keptSymbols
		}
	}
	if salienceTruncated {
		result["salience_truncated"] = true
	}
	if windowed {
		result["window"] = map[string]any{
			"start_line":  winStart,
			"end_line":    winEnd,
			"total_lines": winTotal,
		}
	}
	if physicalEvidenceRequested {
		contentAltered := servedFromOverlay || isBinary || bodiesElided || salienceTruncated || windowed || secretsRedacted || contentTruncated || !utf8.Valid(content)
		contentSource := "disk"
		if servedFromOverlay {
			contentSource = "overlay"
		}
		result["resolved_path"] = physicalEvidence.resolvedPath
		result["file_kind"] = "regular"
		result["byte_count"] = physicalEvidence.byteCount
		result["content_sha256"] = physicalEvidence.contentSHA256
		result["hash_algorithm"] = "sha256"
		result["hash_scope"] = "full_file"
		result["hash_source"] = "disk"
		result["content_source"] = contentSource
		result["disk_verified"] = true
		result["same_buffer_as_content"] = !contentAltered
		result["symlink_resolved"] = physicalEvidence.symlinkResolved
	}

	// Omission notes: tell the model what the payload deliberately
	// leaves out or reshapes, so it does not reason about absent code.
	omissions := pathOmissions(relPath)
	if servedFromOverlay {
		omissions = append(omissions, omission("overlay",
			"served from an active editor-buffer overlay, not the file currently on disk"))
	}
	if isBinary {
		omissions = append(omissions, omission("binary",
			"file is binary — the content field holds raw bytes, not source text"))
	}
	if bodiesElided {
		omissions = append(omissions, omission("compressed",
			"function and method bodies replaced with elided stubs; signatures and structure kept"))
	}
	if salienceTruncated {
		omissions = append(omissions, omission("truncated",
			"oversized source reduced toward its control-flow skeleton; runs of leaf statements collapsed"))
	}
	if contentTruncated {
		omissions = append(omissions, omission("content_truncated",
			fmt.Sprintf("content limited to max_chars=%d encoded bytes at a valid UTF-8 boundary", maxChars)))
	}
	if windowed {
		omissions = append(omissions, omission("windowed",
			fmt.Sprintf("returned lines %d-%d of %d; the rest of the file was not included (offset/limit window)", winStart, winEnd, winTotal)))
	}
	if secretsRedacted {
		omissions = append(omissions, omission("secrets_withheld",
			"secret-shaped values in this config file were withheld; pass allow_secrets:true to read them"))
	}
	if len(omissions) > 0 {
		result["omissions"] = omissions
	}

	etag := computeETag(result)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	result["etag"] = etag
	if physicalEvidenceRequested {
		// Keep the observation timestamp outside the ETag so conditional reads
		// remain stable while the verified disk bytes are unchanged.
		result["read_at"] = physicalEvidence.readAt.Format(time.RFC3339Nano)
	}

	// Server-side accounting only — read_file is the heaviest source
	// fetch and must show up in the savings ledger even when nothing
	// was saved (an uncompressed read returns the whole file, so
	// returned == baseline and only the call is counted). Recorded
	// after the conditional-fetch return so a not_modified turnaround
	// books nothing and skips the tokenization entirely.
	if !isBinary {
		contentStr := string(content)
		returned := tokens.CachedCountInt64(contentStr)
		fullFile := returned
		if bodiesElided || salienceTruncated || windowed || contentTruncated {
			fullFile = int64(tokens.EstimateFromSample(originalBytes, contentStr))
		}
		s.tokenStatsFor(ctx).record(s.fileAttributionNode(relPath, language), "read_file", returned, fullFile)
	}

	s.attachFileDependents(ctx, result, relPath)

	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// detectLanguageForPath resolves the language code for a file. Prefers
// the indexed file node's Node.Language (canonical: same code the
// extractor stamped at index time). Falls back to the parser
// Registry's extension-based detection so unindexed files (or files
// outside any tracked repo) still get a language tag.
func (s *Server) detectLanguageForPath(ctx context.Context, absPath, relPath string) string {
	// Try the indexed file node first.
	if sg := s.engineFor(ctx).GetFileSymbols(relPath); sg != nil {
		for _, n := range sg.Nodes {
			if n != nil && n.Kind == graph.KindFile && n.Language != "" {
				return n.Language
			}
		}
	}
	// Fall back to the parser registry from whichever indexer owns
	// the file. In multi-repo mode, every indexer holds the same
	// registry instance; in single-repo mode we ask the lone indexer.
	//
	// A bounded prefix read lets the registry's content probe place
	// an ambiguous extension (.h, .m) or an unknown-extension script.
	var head []byte
	if f, err := os.Open(absPath); err == nil {
		buf := make([]byte, 512)
		if n, _ := f.Read(buf); n > 0 {
			head = buf[:n]
		}
		_ = f.Close()
	}
	if s.multiIndexer != nil {
		for _, prefix := range s.multiIndexer.RepoPrefixes() {
			if idx := s.multiIndexer.GetIndexer(prefix); idx != nil {
				if reg := idx.Registry(); reg != nil {
					if lang, ok := reg.DetectLanguageContent(absPath, head); ok {
						return lang
					}
				}
			}
		}
	}
	if s.indexer != nil {
		if reg := s.indexer.Registry(); reg != nil {
			if lang, ok := reg.DetectLanguageContent(absPath, head); ok {
				return lang
			}
		}
	}
	return ""
}
