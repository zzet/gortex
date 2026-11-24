package gitstate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/pathkey"
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
	root, ok = checkoutMarkerRoot(dir)
	if !ok {
		return "", "", false
	}
	if _, commonDir = resolveGitDirsFromDotGit(root); commonDir == "" {
		return "", "", false
	}
	// Git reports the common directory with its symlinks resolved.
	commonDir, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", "", false
	}
	return root, commonDir, true
}

// LinkedCheckoutMain reads the linked checkout's reciprocal Git markers.
// A copied .git file does not name a member unless the family's admin
// entry points back to that exact checkout. Bare families have no main.
func LinkedCheckoutMain(dir string) string {
	root, ok := checkoutMarkerRoot(dir)
	if !ok {
		return ""
	}
	marker := filepath.Join(root, ".git")
	target, ok := strings.CutPrefix(readWorktreeMarker(marker), "gitdir: ")
	if !ok || target == "" {
		return ""
	}
	admin, err := filepath.EvalSymlinks(resolveAgainst(root, target))
	if err != nil {
		return ""
	}
	commonMarker := readWorktreeMarker(filepath.Join(admin, "commondir"))
	if commonMarker == "" {
		return ""
	}
	common, err := filepath.EvalSymlinks(resolveAgainst(admin, commonMarker))
	if err != nil || filepath.Base(common) != ".git" ||
		!pathkey.SamePathIdentity(filepath.Dir(admin), filepath.Join(common, "worktrees")) {
		return ""
	}
	backlink := readWorktreeMarker(filepath.Join(admin, "gitdir"))
	if backlink == "" {
		return ""
	}
	back, err := filepath.EvalSymlinks(resolveAgainst(admin, backlink))
	if err != nil || filepath.Base(back) != ".git" {
		return ""
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil || !pathkey.SamePathIdentity(filepath.Dir(back), physicalRoot) {
		return ""
	}
	return filepath.Dir(common)
}

func checkoutMarkerRoot(dir string) (string, bool) {
	root, err := absDir(dir)
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			return root, true
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", false
		}
		root = parent
	}
}

func readWorktreeMarker(path string) string {
	const maxMarkerBytes = 64 * 1024
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMarkerBytes {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxMarkerBytes+1))
	if err != nil || len(raw) > maxMarkerBytes {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
}
