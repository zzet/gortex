package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResolveFamilyDirs resolves dir's own Git directory and shared common
// directory without enumerating worktrees. Both returned paths are absolute.
// It uses the same read-only resolver and old-Git compatibility path as
// Inventory. A failure returns empty paths and wraps ErrInventoryUnavailable.
func ResolveFamilyDirs(ctx context.Context, dir string) (gitDir, commonDir string, err error) {
	abs, err := absDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("gitstate: resolve %q: %w: %w", dir, ErrInventoryUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return "", "", fmt.Errorf("gitstate: resolve git directories for %s: %w: %w", abs, ErrInventoryUnavailable, err)
	}
	gitDir, commonDir, err = resolveFamilyDirs(ctx, abs)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return "", "", fmt.Errorf("gitstate: resolve git directories for %s: %w: %w", abs, ErrInventoryUnavailable, err)
	}
	return gitDir, commonDir, nil
}

// MarkerCommonDir reads, without running Git, the common directory of the
// checkout that holds dir: the nearest .git marker at or above dir (a
// directory or a "gitdir:" file), then the commondir file of the Git
// directory it names. root is the directory holding the marker, and commonDir
// is spelled the way ResolveFamilyDirs reports it. ok is false when no marker
// is found or this read cannot follow it; Git remains the authority on
// anything that must be proven.
func MarkerCommonDir(dir string) (root, commonDir string, ok bool) {
	root, err := absDir(dir)
	if err != nil {
		return "", "", false
	}
	for {
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", "", false
		}
		root = parent
	}
	if _, commonDir = resolveGitDirsFromDotGit(root); commonDir == "" {
		return "", "", false
	}
	// Git reports the common directory with its symlinks resolved.
	if commonDir, err = filepath.EvalSymlinks(commonDir); err != nil {
		return "", "", false
	}
	return root, commonDir, true
}
