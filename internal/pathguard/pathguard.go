// Package pathguard confines filesystem paths to a repository root.
//
// A symbolic link inside an indexed repository can point anywhere on the
// host filesystem. Any code that admits a file into the indexed corpus —
// or that later re-reads one of those files to serve its content — must
// refuse a link whose real target escapes the repository. Otherwise the
// repository boundary stops being a security boundary: a link committed
// as `pwn.go -> /home/user/.ssh/id_rsa` is indexed like ordinary source
// and its content is served verbatim by any tool that reads the corpus.
//
// The two halves of that rule are deliberately separate:
//
//   - Admission (the indexer walk) keeps escaping links out of the corpus
//     in the first place, so every downstream reader inherits the fix.
//   - Read time (each content sink) re-checks, so a corpus built by some
//     other path — an older on-disk index, a caller that assembles its own
//     file list — still cannot serve out-of-repo bytes.
//
// Confinement is always relative to the ONE root that owns the file, never
// to the union of every tracked root: repo A must not read repo B's files
// through a link, even when both are indexed by the same daemon.
package pathguard

import (
	"os"
	"path/filepath"
	"strings"
)

// WithinRoot reports whether path lies at or beneath root. The comparison
// is purely lexical, so a caller that cares about symlinks must resolve
// both sides first — see ResolveRoot and SymlinkEscapes.
//
// An empty root confines nothing and always reports true: a caller with no
// known root has no boundary to enforce, and refusing everything there
// would break unindexed / control-client paths that never had one.
func WithinRoot(path, root string) bool {
	if root == "" {
		return true
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Only a leading ".." component escapes. A sibling directory whose name
	// merely starts with dots ("..config") stays inside, so the check tests
	// for a whole component rather than a string prefix.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ResolveRoot returns root with symlinks evaluated, so containment tests
// compare like with like. Repository roots routinely sit under symlinked
// prefixes (macOS /tmp -> /private/tmp, /var -> /private/var, a symlinked
// checkout), and without this an in-repo file would appear to escape its
// own root. Falls back to the lexical clean when root cannot be resolved.
func ResolveRoot(root string) string {
	if root == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(root)
}

// SymlinkEscapes reports whether path is a link (see IsLink) whose real target
// resolves outside root. A regular file is never an escape, and that fast
// path costs a single Lstat — the root is resolved only for the rare entry
// that actually is a link, which keeps this affordable on a walk that
// visits every file in a large repository.
//
// A link that cannot be resolved at all (broken, or a resolution loop) is
// reported as an escape: it can serve no legitimate content, and refusing
// it is strictly safer than guessing where it points.
func SymlinkEscapes(path, root string) bool {
	if !isLinkPath(path) {
		return false
	}
	return linkTargetEscapes(path, ResolveRoot(root))
}

// EscapesResolvedRoot is SymlinkEscapes for a caller that has already paid
// for ResolveRoot once and is testing many paths under that same root.
func EscapesResolvedRoot(path, resolvedRoot string) bool {
	if !isLinkPath(path) {
		return false
	}
	return linkTargetEscapes(path, resolvedRoot)
}

// IsLink reports whether info, the os.Lstat result for path, is a link the
// operating system follows: a symbolic link or a Windows directory junction.
// Go reports a junction as ModeIrregular rather than ModeSymlink, so checking
// for ModeSymlink alone mistakes one for an ordinary entry.
func IsLink(path string, info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0 || isJunction(path, info)
}

// EvalLinks is filepath.EvalSymlinks for confinement checks. On Windows it also
// follows directory junctions, which filepath.EvalSymlinks returns unchanged at
// the end of a path and fails on anywhere else. A path with no junction in it
// resolves exactly as filepath.EvalSymlinks would resolve it.
func EvalLinks(path string) (string, error) {
	return evalLinks(path)
}

func isLinkPath(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && IsLink(path, info)
}

func linkTargetEscapes(path, resolvedRoot string) bool {
	real, err := EvalLinks(path)
	if err != nil {
		return true
	}
	if WithinRoot(real, resolvedRoot) {
		return false
	}
	// ResolveRoot keeps a junction in the root's own spelling, because its other
	// callers compare it with paths spelled by filepath.EvalSymlinks. real has
	// been through any such junction, so compare again with the root's real
	// location before calling this an escape.
	root, err := EvalLinks(resolvedRoot)
	return err != nil || !WithinRoot(real, root)
}
