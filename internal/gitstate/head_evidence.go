package gitstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// HeadEvidence is the change identity of the files that decide what a
// checkout's HEAD resolves to (the worktree's HEAD, the loose ref it names,
// packed-refs), read without running git. A caller that must refuse work when
// HEAD moves between two instants captures it at the first and asks Unchanged
// at the second; any write to those files — a branch switch, a commit, a ref
// rewrite — reads as changed.
type HeadEvidence struct {
	e headEvidence
}

// CaptureHeadEvidence stats the HEAD-deciding files now. It is best effort:
// a layout it cannot read (a reftable store, an unreadable .git) yields
// evidence whose Usable is false and which never confirms.
func (s *DirtySampler) CaptureHeadEvidence() HeadEvidence {
	if s == nil || s.root == "" {
		return HeadEvidence{}
	}
	return HeadEvidence{e: s.captureHeadEvidence()}
}

// Usable reports whether the evidence can confirm anything.
func (h HeadEvidence) Usable() bool { return h.e.ok }

// Unchanged reports whether every HEAD-deciding file still has the identity
// captured. Unusable evidence is never unchanged.
func (h HeadEvidence) Unchanged() bool { return h.e.unchanged() }

// headFileDetached reads the worktree's HEAD file without running git:
// detached is true when it holds an object id, false when it is a symbolic
// ref; known is false when the file cannot be read or holds neither (a
// reftable store, an unreadable .git), and the caller then asks git.
func (s *DirtySampler) headFileDetached() (detached, known bool) {
	gitDir, _, ok := s.gitDirs()
	if !ok {
		return false, false
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return false, false
	}
	head := strings.TrimSpace(string(raw))
	if strings.HasPrefix(head, "ref: ") {
		return false, true
	}
	if isOID(head) && !isZeroOID(head) {
		return true, true
	}
	return false, false
}

// Hold takes the sampling lease without sampling, as a background holder, and
// returns its release. A caller that must keep samples out for a moment uses
// it; so do tests that need a held lease.
func (s *DirtySampler) Hold(ctx context.Context) (func(), error) {
	return s.acquire(ctx)
}

// UrgentWaiting reports how many urgent samples are waiting for the lease.
func (s *DirtySampler) UrgentWaiting() int {
	if s == nil {
		return 0
	}
	return int(s.urgentWaiting.Load())
}
