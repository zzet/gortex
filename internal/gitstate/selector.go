package gitstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/zzet/gortex/internal/platform"
)

// ErrRefNotAvailableLocally reports that a selector is well formed but the
// local object store cannot answer it: the ref does not exist here, or the
// object it names was never fetched. It never means "fetch and retry" —
// resolution is local-only by construction, so a caller that sees it must
// either pick another selector or arrange the fetch itself.
var ErrRefNotAvailableLocally = errors.New("gitstate: ref or object is not available locally")

// ErrRefNotCommit reports that a selector resolved to an object that is not a
// commit and cannot be peeled to one: a tree, a blob, or a tag whose target is
// neither.
var ErrRefNotCommit = errors.New("gitstate: selector does not resolve to a commit")

// ViewSelectorKind names the two ways a view selector points at committed
// state. The values match the wire vocabulary the API layer validates against.
type ViewSelectorKind string

const (
	// ViewSelectorGitRef names the commit a full ref points at.
	ViewSelectorGitRef ViewSelectorKind = "git_ref"
	// ViewSelectorCommit names one commit by object id.
	ViewSelectorCommit ViewSelectorKind = "commit"
)

// ResolvedSelector is what one selector names at the moment it was resolved.
// FullRef is empty for a commit selector, which names no ref.
type ResolvedSelector struct {
	// FullRef is the complete ref name the selector asked for, echoed back
	// exactly as it was spelled.
	FullRef string
	// CommitOID is the commit the selector resolves to, peeled through an
	// annotated tag when the ref carries one.
	CommitOID string
	// TreeOID is that commit's tree — the state a view of this selector is
	// built from.
	TreeOID string
}

// ResolveViewSelector resolves a view selector against the local object store
// of the repository at repoDir.
//
// Nothing here consults ambient state and nothing reaches the network. A ref
// is looked up by its literal full name through `show-ref --verify`, which
// does no DWIM at all: a short name, HEAD, or a revision expression fails
// before git is invoked, so the object a view pins cannot change meaning with
// the caller's HEAD or remote configuration. Everything after that first
// lookup runs on object ids this package has validated, so no caller-supplied
// string ever reaches a git command line as an option or a revision.
//
// Resolution is deliberately re-run on every selection rather than cached:
// a ref that has not moved costs three plumbing calls, and nothing watches
// refs, so a ref that has moved is only ever noticed here.
func ResolveViewSelector(ctx context.Context, repoDir string, kind ViewSelectorKind, value string) (ResolvedSelector, error) {
	abs, err := absDir(repoDir)
	if err != nil {
		return ResolvedSelector{}, fmt.Errorf("gitstate: resolve %q: %w", repoDir, err)
	}
	switch kind {
	case ViewSelectorGitRef:
		return resolveRefSelector(ctx, abs, value)
	case ViewSelectorCommit:
		return resolveCommitSelector(ctx, abs, value)
	default:
		return ResolvedSelector{}, fmt.Errorf("gitstate: unknown view selector kind %q", string(kind))
	}
}

// resolveRefSelector reads the object a full ref names and peels it to a
// commit.
func resolveRefSelector(ctx context.Context, dir, ref string) (ResolvedSelector, error) {
	if err := checkViewRefName(ref); err != nil {
		return ResolvedSelector{}, err
	}
	out, err := runLocalGit(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		if localGitExitCode(err) == 1 {
			return ResolvedSelector{}, fmt.Errorf("gitstate: ref %s in %s: %w", ref, dir, ErrRefNotAvailableLocally)
		}
		return ResolvedSelector{}, fmt.Errorf("gitstate: resolve ref %s in %s: %w", ref, dir, err)
	}
	oid, ok := firstField(out)
	if !ok || !isOID(oid) || isZeroOID(oid) {
		return ResolvedSelector{}, fmt.Errorf("gitstate: show-ref reported %q for %s", oid, ref)
	}
	commit, err := peelToCommit(ctx, dir, oid)
	if err != nil {
		return ResolvedSelector{}, fmt.Errorf("gitstate: ref %s: %w", ref, err)
	}
	tree, err := treeOfCommit(ctx, dir, commit)
	if err != nil {
		return ResolvedSelector{}, fmt.Errorf("gitstate: ref %s: %w", ref, err)
	}
	return ResolvedSelector{FullRef: ref, CommitOID: commit, TreeOID: tree}, nil
}

// resolveCommitSelector verifies that an object id names a commit present in
// the local store. It does not peel: the selector says commit, so a tag object
// is the wrong kind of object rather than a step on the way to the right one.
func resolveCommitSelector(ctx context.Context, dir, oid string) (ResolvedSelector, error) {
	if !isOID(oid) || isZeroOID(oid) {
		return ResolvedSelector{}, fmt.Errorf("gitstate: commit selector %q is not a full object id", oid)
	}
	kind, err := objectType(ctx, dir, oid)
	if err != nil {
		unavailable, probeErr := localObjectUnavailable(ctx, dir, oid)
		if probeErr == nil && unavailable {
			return ResolvedSelector{}, fmt.Errorf("gitstate: commit %s in %s: %w", oid, dir, ErrRefNotAvailableLocally)
		}
		if probeErr != nil {
			return ResolvedSelector{}, fmt.Errorf("gitstate: inspect commit %s in %s: %w (local availability probe failed: %v)", oid, dir, err, probeErr)
		}
		return ResolvedSelector{}, fmt.Errorf("gitstate: inspect commit %s in %s: %w", oid, dir, err)
	}
	if kind != "commit" {
		return ResolvedSelector{}, fmt.Errorf("gitstate: object %s is a %s: %w", oid, kind, ErrRefNotCommit)
	}
	tree, err := treeOfCommit(ctx, dir, oid)
	if err != nil {
		return ResolvedSelector{}, fmt.Errorf("gitstate: commit %s: %w", oid, err)
	}
	return ResolvedSelector{CommitOID: oid, TreeOID: tree}, nil
}

// peelToCommit resolves oid to the commit it names, following an annotated tag
// to its target.
//
// The peel fails identically for an object that is not here and an object that
// is not a commit, and those are different answers, so a failure is classified
// by asking whether the object exists at all.
func peelToCommit(ctx context.Context, dir, oid string) (string, error) {
	commit, err := revParseOID(ctx, dir, oid+"^{commit}")
	if err == nil {
		return commit, nil
	}
	if _, typeErr := objectType(ctx, dir, oid); typeErr != nil {
		unavailable, probeErr := localObjectUnavailable(ctx, dir, oid)
		if probeErr == nil && unavailable {
			return "", fmt.Errorf("object %s: %w", oid, ErrRefNotAvailableLocally)
		}
		if probeErr != nil {
			return "", fmt.Errorf("inspect object %s: %w (local availability probe failed: %v)", oid, typeErr, probeErr)
		}
		return "", fmt.Errorf("inspect object %s: %w", oid, typeErr)
	}
	return "", fmt.Errorf("object %s: %w", oid, ErrRefNotCommit)
}

// treeOfCommit reads a commit's tree. A commit whose tree is absent is a
// partial clone that never fetched it, which is an availability failure rather
// than a malformed selector.
func treeOfCommit(ctx context.Context, dir, commit string) (string, error) {
	tree, err := revParseOID(ctx, dir, commit+"^{tree}")
	if err != nil {
		if localGitExitCode(err) == 1 {
			return "", fmt.Errorf("tree of %s: %w", commit, ErrRefNotAvailableLocally)
		}
		return "", fmt.Errorf("resolve tree of %s: %w", commit, err)
	}
	return tree, nil
}

// revParseOID resolves a revision expression built from an already-validated
// object id and checks that what came back is an object id.
func revParseOID(ctx context.Context, dir, rev string) (string, error) {
	out, err := runLocalGit(ctx, dir, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(out))
	if !isOID(oid) || isZeroOID(oid) {
		return "", fmt.Errorf("gitstate: rev-parse reported %q for %s", oid, rev)
	}
	return oid, nil
}

// objectType reports git's type name for an object id: commit, tag, tree or
// blob. An error means the local store does not hold the object.
func objectType(ctx context.Context, dir, oid string) (string, error) {
	out, err := runLocalGit(ctx, dir, "cat-file", "-t", oid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func localObjectUnavailable(ctx context.Context, dir, oid string) (bool, error) {
	_, err := runLocalGit(ctx, dir, "cat-file", "-e", oid)
	if err == nil {
		return false, nil
	}
	if localGitExitCode(err) == 1 {
		return true, nil
	}
	return false, err
}

// firstField returns the first whitespace-delimited field of git's output.
func firstField(out []byte) (string, bool) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// viewRefNamespaces are the full-ref prefixes a view selector may name.
// Anything else — HEAD, refs/notes/, a bare branch name — is not a branch, a
// tag, or a remote branch.
var viewRefNamespaces = []string{"refs/heads/", "refs/tags/", "refs/remotes/"}

// refRejectedBytes are the bytes that must never reach git inside a ref name:
// git-check-ref-format forbids them, and several are revision-expression
// syntax that would turn a name into a lookup.
const refRejectedBytes = "~^:?*[\\"

// checkViewRefName is the gate that keeps an ambiguous name away from git.
//
// The API layer validates a selector against the full git-check-ref-format
// rules before it ever gets here; this is the defence that does not depend on
// that having happened. It refuses everything whose meaning could come from
// somewhere other than the literal name: anything outside the three full-ref
// namespaces, and anything carrying revision-expression syntax.
func checkViewRefName(ref string) error {
	if ref == "" {
		return errors.New("gitstate: git_ref selector requires a ref name")
	}
	inNamespace := false
	for _, prefix := range viewRefNamespaces {
		if strings.HasPrefix(ref, prefix) && len(ref) > len(prefix) {
			inNamespace = true
			break
		}
	}
	if !inNamespace {
		return fmt.Errorf("gitstate: ref %q is not a full ref name under refs/heads/, refs/tags/, or refs/remotes/", ref)
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return fmt.Errorf("gitstate: ref %q carries revision-expression syntax", ref)
	}
	for i := 0; i < len(ref); i++ {
		b := ref[i]
		if b <= ' ' || b == 0x7f || strings.IndexByte(refRejectedBytes, b) >= 0 {
			return fmt.Errorf("gitstate: ref %q contains %q", ref, string(b))
		}
	}
	return nil
}

// localGitEnv is the environment every resolution command runs under.
//
// GIT_NO_LAZY_FETCH=1 is the one that matters: in a partial clone a missing
// object would otherwise send git to the network mid-resolution, turning a
// selector lookup into an unbounded download and making "the object is not
// here" unanswerable. Together with using only plumbing that cannot fetch, it
// makes local-only a property of the process rather than a promise.
// GIT_TERMINAL_PROMPT=0 keeps a credential prompt from blocking a daemon with
// no terminal, and GIT_OPTIONAL_LOCKS=0 keeps these read-only commands off the
// index lock.
var fixedLocalGitEnv = []string{
	"GIT_NO_LAZY_FETCH=1",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_OPTIONAL_LOCKS=0",
}

func localGitEnv() []string {
	return localGitEnvFrom(os.Environ())
}

func localGitEnvFrom(base []string) []string {
	env := make([]string, 0, len(base)+len(fixedLocalGitEnv))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && isFixedLocalGitEnvKey(key) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, fixedLocalGitEnv...)
}

func isFixedLocalGitEnvKey(key string) bool {
	for _, entry := range fixedLocalGitEnv {
		fixedKey, _, _ := strings.Cut(entry, "=")
		if key == fixedKey {
			return true
		}
	}
	return false
}

func localGitExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// runLocalGit runs one plumbing command in dir and returns its stdout.
//
// It does not go through internal/gitcmd: that helper owns the global git
// concurrency limiter but offers no way to set the child's environment, and
// every child here must carry localGitEnv. Resolution is three commands per
// selection rather than a per-file fan-out, so it is not what the limiter
// exists to bound.
func runLocalGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = localGitEnv()
	platform.ConfigureBackgroundCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.Bytes(), fmt.Errorf("git %s: %w", args[0], ctxErr)
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}
