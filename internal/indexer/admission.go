package indexer

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PathSkip is how the walk would treat one file. ByRule narrows Skipped to "an
// exclude or ignore rule says so", as opposed to an unclaimed language, the
// size cap, or a corpus-admission gate.
type PathSkip struct {
	Skipped bool
	ByRule  bool
}

// PathIndexability answers the walk's admission question for one repo-relative
// file, through the same admitWalkEntry verdict both walks use plus the corpus
// gates layered above it, so the probe cannot drift from what the walk does.
//
// The second return is false when this indexer cannot answer at all (blank
// root, absolute path, path outside the root, or a path it cannot stat).
// Callers must not read the zero PathSkip as "indexable" then: a repo that
// cannot answer has no vote.
func (idx *Indexer) PathIndexability(relPath string) (PathSkip, bool) {
	root := idx.RootPath()
	abs, ok := absWithinRoot(root, relPath)
	if !ok {
		return PathSkip{}, false
	}
	// Lstat to mirror WalkDir, whose DirEntry.Info() describes the link, never
	// its target. A path that cannot be stat'd is an abstention, not a verdict:
	// the walk never meets it, so there is nothing to agree with. Reporting it
	// as skipped rendered as "no symbols are indexed and none ever will be" for
	// a path that is merely absent.
	info, err := os.Lstat(abs)
	if err != nil {
		return PathSkip{}, false
	}
	if info.IsDir() {
		// The walk never puts a directory through the file gates, so running
		// one through them here would report "unclaimed language, and never
		// will be" for a path that is not a file at all.
		return PathSkip{}, false
	}
	adm := idx.admitWalkFileKnownType(root, abs, info.Size(), info.Mode())
	if !adm.admit {
		return PathSkip{Skipped: true, ByRule: adm.excluded}, true
	}
	// Only the gates the incremental watcher keeps. SkipUntrackedAssets
	// governs the cold walk alone, so applying it here would report "never
	// indexable" for a file the watcher indexes on the next save.
	if _, skip := idx.newContentAdmissionGate().skip(adm.lang, info.Size()); skip {
		return PathSkip{Skipped: true}, true
	}
	return PathSkip{}, true
}

// errScopeWalkBudget stops a scope walk that ran out of its budget.
var errScopeWalkBudget = errors.New("scope walk budget exhausted")

// ScopeIndexability reports whether the index walk would claim any file under
// relDir, the directory counterpart of PathIndexability. relDir is empty for
// the repository root.
//
// walked is false when the walk did not finish (unreadable directory, path
// outside the root, exhausted budget). Callers must not read
// (!indexable && !walked) as "no source here".
//
// The corpus-admission gates are deliberately not applied: they only skip more
// files, so honouring them could turn "the walk would claim something" into a
// proof that it would not.
func (idx *Indexer) ScopeIndexability(relDir string, budget time.Duration) (indexable, walked bool) {
	root := idx.RootPath()
	if root == "" {
		return false, false
	}
	abs := root
	if relDir != "" {
		var ok bool
		if abs, ok = absWithinRoot(root, relDir); !ok {
			return false, false
		}
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() {
		return false, false
	}

	deadline := time.Now().Add(budget)
	complete := true
	_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unread subtree may hold source, so the scope is unsettled.
			complete = false
			if path == abs {
				return walkErr
			}
			return nil
		}
		if time.Now().After(deadline) {
			complete = false
			return errScopeWalkBudget
		}
		if d.IsDir() {
			if idx.admitWalkEntry(root, path, -1, true).pruneDir {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !idx.admitWalkFileKnownType(root, path, -1, d.Type()).admit {
			return nil
		}
		indexable = true
		return fs.SkipAll
	})
	if indexable {
		// One file the walk would claim settles the scope however it ended.
		return true, true
	}
	return false, complete
}

// absWithinRoot resolves a repo-relative path against root and refuses
// anything that does not land inside it. filepath.Join swallows a leading
// separator (Join("/repo", "/etc/passwd") == "/repo/etc/passwd") and Cleans
// "../" away against the root, so without this an absolute or escaping caller
// path is silently rewritten into a plausible in-root path and answered for.
// Callers do hand over unvalidated strings: the MCP graph-path helpers
// deliberately echo their input back when resolution fails.
func absWithinRoot(root, relPath string) (string, bool) {
	if root == "" || relPath == "" || filepath.IsAbs(relPath) {
		return "", false
	}
	abs := filepath.Join(root, relPath)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}
