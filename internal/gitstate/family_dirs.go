package gitstate

import (
	"context"
	"errors"
	"fmt"
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
