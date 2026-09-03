package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// admissionGates carries the two corpus-admission gates that hold per-run
// state. The zero value is inert; both gates are nil-safe.
type admissionGates struct {
	untracked *untrackedAssetGate
	content   *contentAdmissionGate
}

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
	adm := idx.admitWalkEntry(root, abs, info.Size(), false)
	if !adm.admit {
		return PathSkip{Skipped: true, ByRule: adm.excluded}, true
	}
	gates := idx.probeAdmissionGates(root)
	if _, skip := gates.untracked.skip(adm.lang, abs); skip {
		return PathSkip{Skipped: true}, true
	}
	if _, skip := gates.content.skip(adm.lang, info.Size()); skip {
		return PathSkip{Skipped: true}, true
	}
	return PathSkip{}, true
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

const (
	// probeGatesTTL bounds gate reuse across probes: the untracked gate shells
	// out to `git ls-files` for the whole repo, and the tracked set moves at
	// commit speed, not per-call speed.
	probeGatesTTL = 30 * time.Second
	// probeGitTimeout keeps a wedged repo from stalling the triggering call.
	probeGitTimeout = 2 * time.Second
)

// probeGateCache memoises the gates for single-path probes, keyed on root so a
// re-rooted indexer never answers with another checkout's tracked set.
type probeGateCache struct {
	mu    sync.Mutex
	at    time.Time
	root  string
	gates admissionGates
}

func (idx *Indexer) probeAdmissionGates(root string) admissionGates {
	c := &idx.probeGates
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root == root && !c.at.IsZero() && time.Since(c.at) < probeGatesTTL {
		return c.gates
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeGitTimeout)
	defer cancel()
	c.gates = admissionGates{
		untracked: idx.newUntrackedAssetGate(ctx, root),
		content:   idx.newContentAdmissionGate(),
	}
	c.root = root
	c.at = time.Now()
	return c.gates
}
