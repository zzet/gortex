package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/zzet/gortex/internal/gitcmd"
)

// HEADState is a point-in-time sample of what HEAD points at in one
// working tree.
type HEADState struct {
	// Ref is the full ref HEAD symbolically points at
	// ("refs/heads/main"), empty when HEAD is detached.
	Ref string
	// Detached is true when HEAD names a commit directly instead of a
	// ref.
	Detached bool
	// Unborn is true when HEAD points at a ref that does not exist yet
	// — a fresh repository before its first commit. Ref is set in that
	// case; CommitOID and TreeOID are not.
	Unborn bool
	// CommitOID is the commit HEAD resolves to, empty when unborn.
	CommitOID string
	// TreeOID is that commit's tree, empty when unborn or unresolvable.
	TreeOID string
}

// SampleHEAD reports what HEAD points at in the working tree at dir.
//
// It distinguishes the three states a HEAD can be in: attached to a ref
// that resolves, attached to a ref that does not exist yet (unborn),
// and detached at a commit. A directory that is not a git working tree
// yields ErrHEADUnavailable.
//
// Every object id it returns is checked against git's own object-id
// syntax first, so a diagnostic line can never be handed back as an OID.
func SampleHEAD(ctx context.Context, dir string) (HEADState, error) {
	abs, err := absDir(dir)
	if err != nil {
		return HEADState{}, fmt.Errorf("gitstate: resolve %q: %w: %w", dir, ErrHEADUnavailable, err)
	}

	var state HEADState
	ref, err := gitcmd.Output(ctx, abs, "symbolic-ref", "-q", "HEAD")
	switch {
	case err == nil:
		state.Ref = ref
	case exitCode(err) == 1:
		// `symbolic-ref -q` exits 1, silently, exactly when HEAD is not
		// a symbolic ref — that is the detached case, not a failure.
		state.Detached = true
	default:
		return HEADState{}, fmt.Errorf("gitstate: read HEAD in %s: %w: %w", abs, ErrHEADUnavailable, err)
	}

	commit, commitErr := gitcmd.Output(ctx, abs, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	switch {
	case commitErr == nil && isOID(commit) && !isZeroOID(commit):
		state.CommitOID = commit
	case state.Detached:
		// A detached HEAD names a commit by definition; if it will not
		// resolve, the repository is not in a state worth reporting.
		return HEADState{}, fmt.Errorf("gitstate: resolve detached HEAD in %s: %w", abs, ErrHEADUnavailable)
	default:
		state.Unborn = true
	}

	if state.CommitOID != "" {
		tree, treeErr := gitcmd.Output(ctx, abs, "rev-parse", "--verify", "-q", "HEAD^{tree}")
		if treeErr == nil && isOID(tree) && !isZeroOID(tree) {
			state.TreeOID = tree
		}
	}
	return state, nil
}

// exitCode returns the process exit status behind a git error, or -1
// when the error did not come from a process that ran to completion
// (a cancelled context, a missing binary).
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
