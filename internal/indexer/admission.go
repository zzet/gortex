package indexer

import (
	"context"
	"os"
	"sync"
	"time"
)

// Skip reasons for the gates that don't live in content_admission.go.
const (
	skipReasonNoLanguage  = "no_language"   // no registered extractor claims it
	skipReasonExcluded    = "excluded"      // an exclude / ignore RULE drops it
	skipReasonMaxFileSize = "max_file_size" // over index.max_file_size
)

// admissionGates carries the two gates that hold per-run state. The zero value
// is inert; both gates are nil-safe.
type admissionGates struct {
	untracked *untrackedAssetGate
	content   *contentAdmissionGate
}

// admitFile is the one admission decision, shared by the cold walk
// (indexCtxRaw), the dry-run manifest (DryRunIntake) and the single-path probe
// (PathIndexability) so the three cannot drift. reason is "" when admitted.
//
// Exclude rules run before language detection on purpose: effectiveLanguage
// falls through to readSniffPrefix, which os.Opens the file. Language-first
// read files inside vendored trees, and read them before shouldExclude's
// symlink-confinement guard refused links pointing out of the repo.
func (idx *Indexer) admitFile(path, absRoot string, size int64, gates admissionGates) (lang, reason string) {
	if idx.shouldExclude(path, absRoot, false) {
		return "", skipReasonExcluded
	}
	lang, claimed := idx.effectiveLanguage(path, nil)
	if !claimed {
		return "", skipReasonNoLanguage
	}
	if maxSize := idx.config.MaxFileSize; maxSize > 0 && size > maxSize {
		return lang, skipReasonMaxFileSize
	}
	if r, skip := gates.untracked.skip(lang, path); skip {
		return lang, r
	}
	if r, skip := gates.content.skip(lang, size); skip {
		return lang, r
	}
	return lang, ""
}

// PathSkip is how the walk would treat one file. ByRule narrows Skipped to "an
// exclude or ignore rule says so", as opposed to an unclaimed language, the
// size cap, or a corpus-admission gate.
type PathSkip struct {
	Skipped bool
	ByRule  bool
}

// PathIndexability answers the walk's admission question for one repo-relative
// file, via admitFile.
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
	// a path that is merely absent — a typo, a file about to be created, or a
	// path form that did not round-trip to this root.
	info, err := os.Lstat(abs)
	if err != nil {
		return PathSkip{}, false
	}
	_, reason := idx.admitFile(abs, root, info.Size(), idx.probeAdmissionGates(root))
	return PathSkip{Skipped: reason != "", ByRule: reason == skipReasonExcluded}, true
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
