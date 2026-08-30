package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/zzet/gortex/internal/gitcmd"
)

const (
	emptyTreeOIDSHA1   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	emptyTreeOIDSHA256 = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
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

// CanonicalHeadTreeOID turns a sampled HEAD tree into the immutable tree oid
// lifecycle generations use as their content identity.
//
// A repository before its first commit has no HEAD tree object, but Git still
// defines one canonical empty-tree oid for each object format. Git's revision
// and diff plumbing accepts that oid without the object being materialized in
// the object database, which lets an unborn checkout use the same immutable
// base/commit/dirty pipeline as every other checkout. A non-empty tree is
// returned unchanged. An empty tree paired with a real commit remains an
// error: that is an unresolved committed HEAD, not an unborn repository.
func CanonicalHeadTreeOID(
	ctx context.Context,
	dir string,
	commitOID string,
	treeOID string,
) (string, error) {
	if treeOID != "" {
		return treeOID, nil
	}
	if commitOID != "" {
		return "", fmt.Errorf(
			"gitstate: commit %s in %s has no resolvable tree: %w",
			commitOID, dir, ErrHEADUnavailable,
		)
	}

	abs, err := absDir(dir)
	if err != nil {
		return "", fmt.Errorf("gitstate: resolve %q: %w: %w", dir, ErrHEADUnavailable, err)
	}
	format, err := gitcmd.Output(ctx, abs, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("gitstate: read object format in %s: %w: %w", abs, ErrHEADUnavailable, err)
	}
	switch format {
	case "sha1":
		return emptyTreeOIDSHA1, nil
	case "sha256":
		return emptyTreeOIDSHA256, nil
	default:
		return "", fmt.Errorf(
			"gitstate: unsupported object format %q in %s: %w",
			format, abs, ErrHEADUnavailable,
		)
	}
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
