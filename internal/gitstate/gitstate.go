// Package gitstate reads the authoritative state of a git checkout
// family — the main worktree, every linked worktree, and the shared
// administrative directory they hang off — straight out of git plumbing.
//
// The inventory is NUL-safe. `git worktree list --porcelain -z` emits
// one NUL-terminated entry per attribute and an empty entry between
// records, so a worktree path (or a lock reason) may legally contain
// spaces and newlines without corrupting the stream. A line-oriented
// parser silently mis-splits those records; this package never splits
// on newlines.
//
// Everything here is read-only. No function runs a mutating git
// command: no `worktree add` / `remove` / `prune`, no `checkout`, no
// `fetch`. Callers that want to change the family must do so
// themselves; this package only observes.
package gitstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/pathkey"
)

// MainAdminName is the sentinel AdminName carried by the main worktree.
// Only linked worktrees own a directory under `<commondir>/worktrees/`,
// so the main checkout has no administrative name of its own; this
// sentinel keeps the field non-empty and un-collidable with a real
// admin directory name (git never creates one starting with '@').
const MainAdminName = "@main"

// ErrInventoryUnavailable reports that the checkout family could not be
// enumerated at all — git refused the directory, the porcelain call
// failed, or the shared directories could not be resolved. It is the
// hard signal that the returned inventory says nothing: a caller
// comparing against a previous inventory MUST NOT read missing records
// as removals when this error is present.
var ErrInventoryUnavailable = errors.New("git worktree inventory unavailable")

// ErrHEADUnavailable reports that HEAD could not be sampled — the
// directory is not a git working tree, or its HEAD is unreadable. As
// with ErrInventoryUnavailable, the zero HEADState returned alongside
// it carries no information.
var ErrHEADUnavailable = errors.New("git HEAD state unavailable")

// FamilyInventory is one snapshot of a checkout family.
type FamilyInventory struct {
	// CommonDir is the shared git directory every worktree in the
	// family reads refs and objects from (the main checkout's `.git`,
	// or the repository itself when it is bare). Always absolute.
	CommonDir string
	// GitDir is the git directory of the queried directory alone. It
	// equals CommonDir for the main checkout and points into
	// `<CommonDir>/worktrees/<name>` for a linked worktree. Always
	// absolute.
	GitDir string
	// Records are the family's worktrees in git's own order. The first
	// record is always the main worktree.
	Records []WorktreeRecord
}

// WorktreeRecord is one worktree of a checkout family, as git reports
// it. A record is present because git's administrative data says the
// worktree exists — not because its directory was found on disk. Use
// RootAccessible to tell the two apart.
type WorktreeRecord struct {
	// Path is the worktree's root directory, exactly as git spells it.
	// It may contain spaces and newlines.
	Path string
	// IsMain is true for the family's main worktree. It is decided by
	// the record's position in git's output, never by the shape of the
	// path.
	IsMain bool
	// AdminName is the basename of the worktree's administrative
	// directory under `<CommonDir>/worktrees/`. It is MainAdminName for
	// the main worktree, and may be empty for a linked worktree whose
	// administrative directory could not be identified.
	//
	// The admin name is not the basename of Path: git sanitizes and
	// de-duplicates it, so a worktree at `.../wt space` is administered
	// under `wt-space`.
	AdminName string
	// Branch is the short branch name checked out in the worktree
	// ("feature/foo"), empty when the worktree is detached or bare.
	Branch string
	// HEADRef is the full ref HEAD points at ("refs/heads/feature/foo"),
	// empty when the worktree is detached or bare.
	HEADRef string
	// HEADOID is the commit HEAD resolves to. It is empty when the
	// worktree is bare or its branch is unborn; git's all-zero
	// placeholder is never reported as an OID.
	HEADOID string
	// Detached is true when HEAD points straight at a commit.
	Detached bool
	// Unborn is true when the worktree is on a branch that has no
	// commit yet.
	Unborn bool
	// Bare is true for a bare repository's own entry.
	Bare bool
	// Locked is true when the worktree is locked against pruning.
	Locked bool
	// LockReason is the reason recorded with the lock, empty when the
	// worktree is unlocked or was locked without a reason.
	LockReason string
	// Prunable is true when git considers the worktree eligible for
	// `git worktree prune`.
	Prunable bool
	// PruneReason is git's explanation of why the worktree is prunable,
	// empty when it is not.
	PruneReason string
	// RootAccessible is true when Path could be statted. A false value
	// is not an inventory failure: git still knows about the worktree,
	// the directory just is not reachable right now.
	RootAccessible bool
	// RootErr is the stat error behind RootAccessible == false.
	RootErr error
}

// oidPattern matches a full object id in any hash algorithm git
// currently emits (40 hex chars for SHA-1, 64 for SHA-256).
var oidPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// isOID reports whether s is a syntactically valid object id. Every
// value this package reports as an OID passes through it, so a
// truncated or diagnostic line from git can never be mistaken for one.
func isOID(s string) bool { return oidPattern.MatchString(s) }

// isZeroOID reports whether s is git's all-zero placeholder, which
// stands for "no object" — the value `git worktree list` prints for an
// unborn branch.
func isZeroOID(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "0") == ""
}

// Inventory enumerates the checkout family that dir belongs to.
//
// It runs two read-only git commands: `worktree list --porcelain -z`
// for the records, and `rev-parse` for the family's shared and
// per-worktree git directories. A failure of either yields
// ErrInventoryUnavailable and a nil inventory.
//
// A record whose root directory cannot be statted is still returned,
// with RootAccessible false and RootErr set. That case is deliberately
// not an error: git listing a worktree whose directory is gone is
// exactly the state a caller needs to see.
func Inventory(ctx context.Context, dir string) (*FamilyInventory, error) {
	abs, err := absDir(dir)
	if err != nil {
		return nil, fmt.Errorf("gitstate: resolve %q: %w: %w", dir, ErrInventoryUnavailable, err)
	}

	gitDir, commonDir, err := resolveFamilyDirs(ctx, abs)
	if err != nil {
		return nil, fmt.Errorf("gitstate: resolve git directories for %s: %w: %w", abs, ErrInventoryUnavailable, err)
	}

	// -z is required, not preferred: without it a path or lock reason
	// containing a newline silently splits into bogus records. A git
	// too old to accept -z cannot produce a trustworthy inventory, so
	// the call fails rather than degrading to newline parsing.
	out, err := gitcmd.Run(ctx, abs, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("gitstate: list worktrees in %s: %w: %w", abs, ErrInventoryUnavailable, err)
	}

	inv := &FamilyInventory{
		CommonDir: commonDir,
		GitDir:    gitDir,
		Records:   parsePorcelainZ(out),
	}
	annotateRecords(ctx, inv, commonDir)
	return inv, nil
}

// absDir normalizes a caller-supplied directory into a cleaned absolute
// path. Absolutizing is also a small hardening step: the result is
// handed to git as the value of `-C`, and an absolute path can never be
// mistaken for an option.
func absDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// resolveFamilyDirs returns the queried directory's own git dir and the
// family's shared common dir, both absolute.
//
// The preferred form asks git for absolute paths outright. Older git
// releases reject `--path-format`, so on failure the plain form is
// retried and a relative common dir is resolved against dir — which is
// the process working directory git printed it relative to.
func resolveFamilyDirs(ctx context.Context, dir string) (gitDir, commonDir string, err error) {
	out, err := gitcmd.Output(ctx, dir, "rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir")
	if err == nil {
		if g, c, ok := twoPaths(out); ok {
			return filepath.Clean(g), filepath.Clean(c), nil
		}
	}

	out, fallbackErr := gitcmd.Output(ctx, dir, "rev-parse", "--absolute-git-dir", "--git-common-dir")
	if fallbackErr != nil {
		return "", "", fallbackErr
	}
	g, c, ok := twoPaths(out)
	if !ok {
		return "", "", fmt.Errorf("unexpected rev-parse output %q", out)
	}
	return filepath.Clean(g), resolveAgainst(dir, c), nil
}

// twoPaths splits rev-parse output that is expected to carry exactly
// two path lines.
func twoPaths(out string) (first, second string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if l != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) != 2 {
		return "", "", false
	}
	return kept[0], kept[1], true
}

// resolveAgainst turns a possibly-relative git path into an absolute
// one, anchored at the directory git was run in.
func resolveAgainst(dir, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}

// parsePorcelainZ parses `git worktree list --porcelain -z`.
//
// The stream is a flat sequence of NUL-terminated entries. Each entry
// is either `<key>` or `<key> <value>`; an empty entry ends the current
// record. Values are taken verbatim, so a path or lock reason keeps its
// spaces and newlines.
func parsePorcelainZ(out []byte) []WorktreeRecord {
	var records []WorktreeRecord
	cur := WorktreeRecord{}
	open := false

	flush := func() {
		if open && cur.Path != "" {
			records = append(records, finalizeRecord(cur))
		}
		cur = WorktreeRecord{}
		open = false
	}

	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			flush()
			continue
		}
		key, value := splitAttribute(string(entry))
		switch key {
		case "worktree":
			flush()
			cur.Path = value
			open = true
		case "HEAD":
			cur.HEADOID = value
			open = true
		case "branch":
			cur.HEADRef = value
			cur.Branch = strings.TrimPrefix(value, "refs/heads/")
			open = true
		case "detached":
			cur.Detached = true
			open = true
		case "bare":
			cur.Bare = true
			open = true
		case "locked":
			cur.Locked = true
			cur.LockReason = value
			open = true
		case "prunable":
			cur.Prunable = true
			cur.PruneReason = value
			open = true
		default:
			// An attribute a newer git added. Ignore it rather than
			// letting it end the record.
		}
	}
	flush()
	return records
}

// splitAttribute splits a porcelain entry into its key and its
// (possibly empty) value at the first space. Keys never contain a
// space; values may contain anything but NUL.
func splitAttribute(entry string) (key, value string) {
	if i := strings.IndexByte(entry, ' '); i >= 0 {
		return entry[:i], entry[i+1:]
	}
	return entry, ""
}

// finalizeRecord normalizes a freshly parsed record: it drops git's
// all-zero HEAD placeholder and derives the unborn state from it.
func finalizeRecord(r WorktreeRecord) WorktreeRecord {
	if r.HEADOID != "" && (isZeroOID(r.HEADOID) || !isOID(r.HEADOID)) {
		r.HEADOID = ""
	}
	// A bare repository has no working HEAD to be unborn about; every
	// other record without a resolvable HEAD is on a branch that has no
	// commit yet.
	if !r.Bare && r.HEADOID == "" {
		r.Unborn = true
	}
	return r
}

// annotateRecords fills in the fields that porcelain output does not
// carry: which record is the main worktree, whether each root is
// reachable on disk, and each linked worktree's administrative name.
func annotateRecords(ctx context.Context, inv *FamilyInventory, commonDir string) {
	var adminIndex []adminEntry
	indexed := false
	adminIndexFor := func(path string) string {
		if !indexed {
			adminIndex, indexed = buildAdminIndex(commonDir), true
		}
		for _, entry := range adminIndex {
			if pathkey.EqualPaths(entry.root, path) {
				return entry.name
			}
		}
		return ""
	}

	for i := range inv.Records {
		r := &inv.Records[i]
		if _, err := os.Lstat(r.Path); err == nil {
			r.RootAccessible = true
		} else {
			r.RootErr = err
		}

		if i == 0 {
			r.IsMain = true
			r.AdminName = MainAdminName
			continue
		}
		if r.RootAccessible {
			r.AdminName = adminNameFromWorktree(ctx, r.Path)
		}
		if r.AdminName == "" {
			r.AdminName = adminIndexFor(r.Path)
		}
	}
}

// adminNameFromWorktree asks git for the worktree's own git directory
// and returns its basename, which is the administrative name. It
// returns "" unless the answer really points inside a `worktrees/`
// directory.
func adminNameFromWorktree(ctx context.Context, path string) string {
	out, err := gitcmd.Output(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil || out == "" {
		return ""
	}
	gitDir := filepath.Clean(out)
	if filepath.Base(filepath.Dir(gitDir)) != "worktrees" {
		return ""
	}
	return filepath.Base(gitDir)
}

// adminEntry pairs the worktree root one administrative directory
// records with that directory's name.
type adminEntry struct {
	// root is the worktree root read out of the admin directory's
	// `gitdir` file, with the trailing `.git` component removed.
	root string
	// name is the basename of the administrative directory, which is
	// the administrative name of the worktree it records.
	name string
}

// buildAdminIndex pairs each linked worktree's root path with its
// administrative directory name by reading the `gitdir` file every
// admin directory keeps. It is the fallback for a worktree whose root
// is gone: the admin directory outlives the checkout, and its `gitdir`
// file still records where the checkout used to be.
//
// The result is a slice matched through pathkey.EqualPaths rather than
// a map keyed on the root, because the two spellings being matched do
// not agree byte for byte. The `gitdir` file holds git's own spelling,
// which uses "/" on every platform, and filepath.Dir folds it to the
// host separator — "C:\\wt" on Windows. The path being looked up is a
// WorktreeRecord.Path, which is git's porcelain spelling untouched:
// "C:/wt". A map keyed on either one therefore misses every lookup on
// Windows, leaving a departed worktree with no administrative name to
// key an identity on. Folding both sides also settles a drive letter
// the two spellings disagree on.
func buildAdminIndex(commonDir string) []adminEntry {
	if commonDir == "" {
		return nil
	}
	base := filepath.Join(commonDir, "worktrees")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	index := make([]adminEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(base, e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		// The file holds "<worktree root>/.git" plus one trailing
		// newline. Only that final newline is stripped — the path
		// itself may contain newlines.
		recorded := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if recorded == "" {
			continue
		}
		index = append(index, adminEntry{root: filepath.Dir(recorded), name: e.Name()})
	}
	return index
}
