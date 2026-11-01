package indexer

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/gitcmd"
)

// WorktreeInfo describes a directory's relationship to its git
// repository.
type WorktreeInfo struct {
	// IsWorktree is true when the directory is a linked git worktree
	// rather than the repository's main checkout.
	IsWorktree bool
	// MainRepoPath is the main worktree's root — the shared base that
	// every linked worktree of the repo descends from. It equals the
	// queried path for a main checkout or a non-git directory.
	MainRepoPath string
	// GitCommonDir is the shared .git directory all of a repo's
	// worktrees use. Empty when the directory is not a git repository.
	GitCommonDir string
}

// ResolveWorktree reports whether path is a linked git worktree and
// resolves the main repository it shares a .git directory with.
//
// A linked worktree carries a `.git` *file* (`gitdir: <path>`) instead
// of a directory; the referenced per-worktree gitdir holds a
// `commondir` file pointing back at the shared .git, whose parent is
// the main checkout. A git submodule also uses a `.git` file but has
// no `commondir`, so it resolves to itself — a submodule is a separate
// repository, not a worktree. A main checkout or a non-git directory
// likewise resolves to itself.
//
// A worktree can be tracked either as part of its canonical repo (the
// default) or, via WorktreeInstanceName, as an independent instance
// keyed by its own working-directory path — letting one underlying
// repository have multiple checkouts, each assigned to its own
// workspace and indexed from its own branch / working tree.
func ResolveWorktree(path string) WorktreeInfo {
	info, _ := resolveWorktreeChecked(path)
	return info
}

// resolveWorktreeChecked keeps I/O uncertainty distinct from a non-git root.
// Startup identity publication must not treat a best-effort fallback as proof
// that no configured alias can own a request.
func resolveWorktreeChecked(path string) (WorktreeInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	info := WorktreeInfo{MainRepoPath: abs}

	gitPath := filepath.Join(abs, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return info, nil
		}
		return info, err
	}
	if st.IsDir() {
		info.GitCommonDir = gitPath
		return info, nil // the main checkout
	}

	// `.git` is a file: a linked worktree or a submodule. Read the
	// gitdir indirection.
	content, err := os.ReadFile(gitPath)
	if err != nil {
		return info, err
	}
	line := strings.TrimSpace(string(content))
	wtGitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if wtGitDir == "" || wtGitDir == line {
		return info, fmt.Errorf("malformed gitdir in %s", gitPath)
	}
	if !filepath.IsAbs(wtGitDir) {
		wtGitDir = filepath.Join(abs, wtGitDir)
	}

	gitInfo, err := os.Stat(wtGitDir)
	if err != nil {
		return info, err
	}
	if !gitInfo.IsDir() {
		return info, fmt.Errorf("gitdir %s is not a directory", wtGitDir)
	}

	// Only a worktree's gitdir carries a `commondir` file; a
	// submodule's does not. Its absence means "not a worktree".
	commonRaw, err := os.ReadFile(filepath.Join(wtGitDir, "commondir"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			info.GitCommonDir = filepath.Clean(wtGitDir)
			return info, nil // separate gitdir or submodule: still its own primary
		}
		return info, err
	}
	common := strings.TrimSpace(string(commonRaw))
	if common == "" {
		return info, fmt.Errorf("empty commondir in %s", wtGitDir)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(wtGitDir, common)
	}
	common = filepath.Clean(common)

	info.IsWorktree = true
	info.GitCommonDir = common
	// The main checkout is the directory that contains the shared .git.
	if filepath.Base(common) == ".git" {
		info.MainRepoPath = filepath.Dir(common)
	}
	return info, nil
}

// WorktreeInstanceName decides the repo prefix that absPath should be
// tracked under, given the base prefix it would otherwise take and the
// workspace it declares (via its own `.gortex.yaml` or a global-config
// override). It returns (name, separate): when separate is true the
// caller must register absPath as an INDEPENDENT repo instance under
// `name`, distinct from any canonical checkout that shares the same git
// identity; when false the caller keeps its single-instance behaviour.
//
// A separate instance is created when either:
//   - asWorktree is set (the explicit `--as-worktree` directive), or
//   - absPath is a linked git worktree that declares a workspace which
//     differs from its base prefix. An explicit workspace on a worktree
//     is the signal that the checkout means to join a workspace other
//     than the canonical's, so we honour it automatically.
//
// The instance name is `<basePrefix>@<tag>`, where the tag is the
// declared workspace when present, else the checked-out branch, else a
// short hash of the path. The result is deterministic for a given
// (path, declared-workspace) pair so it is stable across daemon
// restarts. The tag is sanitised so the name never contains '/', which
// would break the `<prefix>/<relpath>` node-ID layout.
func WorktreeInstanceName(absPath, basePrefix, declaredWorkspace string, asWorktree bool) (string, bool) {
	separate := asWorktree
	if !separate && declaredWorkspace != "" && declaredWorkspace != basePrefix {
		separate = ResolveWorktree(absPath).IsWorktree
	}
	if !separate {
		return basePrefix, false
	}

	tag := ""
	if declaredWorkspace != "" && declaredWorkspace != basePrefix {
		tag = declaredWorkspace
	}
	if tag == "" {
		tag = worktreeBranch(absPath)
	}
	if t := sanitizeInstanceTag(tag); t != "" {
		return basePrefix + "@" + t, true
	}
	return basePrefix + "@" + shortPathHash(absPath), true
}

// worktreeBranch returns the short branch name checked out at absPath, or
// "" when the path is not a git working tree or is in detached-HEAD
// state. Used to disambiguate a forced worktree instance that declares
// no workspace of its own.
func worktreeBranch(absPath string) string {
	b, err := gitcmd.Output(context.Background(), absPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	if b == "" || b == "HEAD" { // detached HEAD has no branch name
		return ""
	}
	return b
}

// sanitizeInstanceTag normalises a disambiguator (a workspace slug or a
// branch name) into a token safe to embed in a repo prefix. A repo
// prefix is the leading "<prefix>/" segment of every node ID, so the
// token must never contain '/'; any character outside [A-Za-z0-9._-] is
// folded to '-'. Leading/trailing '-' are trimmed; empty input maps to "".
func sanitizeInstanceTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// WorktreeInstanceCollisionName disambiguates two worktree instances that
// derive the same name, using the same stable suffix as live registration.
func WorktreeInstanceCollisionName(name, absPath string) string {
	return name + "-" + shortPathHash(absPath)
}

// shortPathHash returns a short, stable hash of a filesystem path. It is
// the last-resort disambiguator when a forced worktree instance has
// neither a declared workspace nor a resolvable branch — two different
// checkouts under the same name still get distinct prefixes.
func shortPathHash(absPath string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(absPath)))
	return fmt.Sprintf("%08x", h.Sum32())
}
