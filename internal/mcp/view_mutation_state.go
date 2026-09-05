package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// checkoutMutationLifecycle is the request's already-acquired checkout lease.
// Keeping this narrow also lets handler tests prove that neither a primary
// watcher nor a canonical indexer is used for a selected-worktree mutation.
type checkoutMutationLifecycle interface {
	Prepare(context.Context) error
	Refresh(context.Context) (indexer.CheckoutCycle, error)
}

type checkoutMutationContextKey struct{}

type checkoutMutationState struct {
	mutation checkoutMutationLifecycle
	root     string
}

func withCheckoutMutation(ctx context.Context, mutation checkoutMutationLifecycle, root string) context.Context {
	return context.WithValue(ctx, checkoutMutationContextKey{}, &checkoutMutationState{
		mutation: mutation,
		root:     resolveNearestExistingAncestor(root),
	})
}

func checkoutMutationFromContext(ctx context.Context) *checkoutMutationState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(checkoutMutationContextKey{}).(*checkoutMutationState)
	return state
}

// guardCheckoutMutationPath is stricter than read confinement: an absolute
// path naming another indexed checkout is not a valid target for this lease.
// Resolve the nearest existing ancestor too, so a newly created file cannot
// escape through a symlinked parent. The selected root itself may be an alias.
func guardCheckoutMutationPath(ctx context.Context, path string) error {
	state := checkoutMutationFromContext(ctx)
	if state == nil {
		return nil
	}
	if state.mutation == nil || !filepath.IsAbs(state.root) || !filepath.IsAbs(path) {
		return fmt.Errorf("checkout mutation requires an absolute path in its selected working copy")
	}
	// AtomicWriteFile replaces the named directory entry, not a symlink's
	// target. A link in the primary pointing into this worktree must therefore
	// fail even though the bytes read through it belong to the selected root.
	entry := filepath.Join(resolveNearestExistingAncestor(filepath.Dir(path)), filepath.Base(path))
	if err := guardCheckoutMutationResolvedPath(state.root, entry, path); err != nil {
		return err
	}
	resolved := resolveNearestExistingAncestor(path)
	if resolved == entry {
		return nil
	}
	return guardCheckoutMutationResolvedPath(state.root, resolved, path)
}

func guardCheckoutMutationResolvedPath(root, resolved, path string) error {
	if !pathkey.HasPathPrefix(resolved, root) || pathkey.EqualPaths(resolved, root) {
		return fmt.Errorf("checkout mutation target %q is outside selected working copy %q", path, root)
	}
	// A repository nested below the selected checkout has independent dirty
	// state and a different coordinator, even though lexical confinement passes.
	// Use the same path identity for confinement and termination: a case or
	// separator alias must not make the selected root's own .git look nested.
	dir := resolved
	for ; !pathkey.EqualPaths(dir, root); dir = filepath.Dir(dir) {
		if strings.EqualFold(filepath.Base(dir), ".git") {
			return fmt.Errorf("checkout source mutation cannot modify Git metadata: %q", path)
		}
		if pathkey.EqualPaths(dir, filepath.Dir(dir)) {
			return fmt.Errorf("could not verify selected checkout root for mutation target %q", path)
		}
		if dir == resolved {
			continue // Only ancestors, not the source file, can contain a nested .git.
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return fmt.Errorf("checkout mutation target %q belongs to nested checkout %q; select that checkout instead", path, dir)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("could not verify checkout mutation target %q: %w", path, err)
		}
	}
	if dir != root {
		// Identity folding follows the host default, but a particular volume
		// may be case-sensitive. Never grant a lease for /repo access to a
		// physically distinct /REPO merely because their names fold equally.
		selected, selectedErr := os.Stat(root)
		matched, matchedErr := os.Stat(dir)
		if selectedErr != nil || matchedErr != nil || !os.SameFile(selected, matched) {
			return fmt.Errorf("could not verify selected checkout root identity for mutation target %q", path)
		}
	}
	return nil
}

func prepareCheckoutMutation(ctx context.Context, path string) error {
	state := checkoutMutationFromContext(ctx)
	if state == nil {
		return nil
	}
	if err := guardCheckoutMutationPath(ctx, path); err != nil {
		return err
	}
	return state.mutation.Prepare(ctx)
}

func (s *Server) refreshCheckoutMutation(ctx context.Context, path string, state *checkoutMutationState) mutationReindexOutcome {
	outcome := mutationReindexOutcome{checkoutScoped: true}
	if err := guardCheckoutMutationPath(ctx, path); err != nil {
		outcome.Err = err
		return outcome
	}
	// Disk has committed. Give its selected checkout a bounded refresh even
	// when the caller has disconnected, then let Close signal a retry on failure.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.mutationWaitDuration())
	defer cancel()
	cycle, err := state.mutation.Refresh(refreshCtx)
	if err != nil {
		outcome.Err = fmt.Errorf("checkout graph refresh failed after disk commit: %w", err)
		return outcome
	}
	if cycle.Err != nil || cycle.Rescheduled || cycle.Deferred || cycle.DirtyGenerationID <= 0 || cycle.CommitGenerationID <= 0 {
		outcome.Err = fmt.Errorf("checkout graph refresh did not publish an exact view")
		return outcome
	}
	outcome.Reindexed = true
	outcome.AppliedGeneration = uint64(cycle.DirtyGenerationID)
	return outcome
}
