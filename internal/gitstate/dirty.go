package gitstate

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
)

// ErrDirtyUnavailable reports that a checkout's dirty state could not be
// sampled — the directory is not a working tree, or the status call
// failed. As with ErrInventoryUnavailable and ErrHEADUnavailable, the
// zero DirtySnapshot returned alongside it carries no information: a
// caller diffing against a previous snapshot MUST NOT read the empty
// entry list as "the checkout went clean".
var ErrDirtyUnavailable = errors.New("git dirty state unavailable")

// DirtyKind names what kind of difference one path carries.
type DirtyKind string

const (
	// DirtyModified is a content change to a path that exists on both
	// sides of the comparison. A conflicted (unmerged) path is reported
	// this way too.
	DirtyModified DirtyKind = "modified"
	// DirtyAdded is a path the index holds and HEAD does not.
	DirtyAdded DirtyKind = "added"
	// DirtyDeleted is a path that HEAD holds and the index or the
	// worktree does not.
	DirtyDeleted DirtyKind = "deleted"
	// DirtyUntracked is a path on disk that the index does not hold.
	// Ignored paths are never reported as untracked.
	DirtyUntracked DirtyKind = "untracked"
	// DirtyModeChanged is a path whose octal file mode changed while the
	// path stayed present on both sides — the executable bit flipping is
	// the usual case.
	DirtyModeChanged DirtyKind = "mode_changed"
	// DirtySymlinkChanged is a path whose file mode changed between
	// 120000 and anything else — a symlink replaced a file, or a file
	// replaced a symlink.
	DirtySymlinkChanged DirtyKind = "symlink_changed"
	// DirtyRenamedFrom is the destination half of a rename. OldPath
	// names where the content came from.
	DirtyRenamedFrom DirtyKind = "renamed_from"
)

// DirtyEntry is one path's difference from HEAD.
type DirtyEntry struct {
	// Path is the path relative to the worktree root, exactly as git
	// spells it. It may contain spaces and newlines.
	Path string
	// Kind is what kind of difference this is.
	Kind DirtyKind
	// Staged is true when the index differs from HEAD at this path.
	Staged bool
	// Unstaged is true when the worktree differs from the index at this
	// path. An untracked path is unstaged.
	Unstaged bool
	// OldPath is the source path of a rename, set only on a
	// DirtyRenamedFrom entry. It is diagnostic: the vanished source is
	// reported as its own DirtyDeleted entry, so a caller that only
	// diffs paths never has to read it.
	OldPath string
	// Submodule is true when the path is a submodule rather than a file.
	Submodule bool
}

// DirtySnapshot is one sample of everything a checkout differs from
// HEAD by.
type DirtySnapshot struct {
	// HeadRef is the full ref HEAD points at ("refs/heads/main"), empty
	// when HEAD is detached.
	HeadRef string
	// HeadCommit is the commit HEAD resolves to, empty when the branch
	// is unborn.
	HeadCommit string
	// HeadTree is that commit's tree, empty when the branch is unborn.
	HeadTree string
	// Entries are the differing paths, ordered by path and then kind.
	Entries []DirtyEntry
	// Fingerprint hashes reported effective content over HeadTree. HeadCommit
	// and staging remain diagnostics; see SampleDirty for admission/cache bounds.
	Fingerprint string
	// Contents is the per-path content identity the Fingerprint was built
	// from, one entry per distinct reported path, sorted by path. It is set
	// only when every sampling fence passed, so a refused (torn) sample never
	// exposes it, and it is nil for a clean checkout. The entries whose
	// HeadEqual is false are exactly the Fingerprint's per-path inputs.
	Contents []DirtyContent
}

// DirtyContentState names what the sampler observed at one reported path.
type DirtyContentState string

const (
	// DirtyContentPresent is a regular file or symlink whose bytes were
	// hashed; Mode and SHA256 are set.
	DirtyContentPresent DirtyContentState = "present"
	// DirtyContentAbsent is a reported path with nothing on disk.
	DirtyContentAbsent DirtyContentState = "absent"
	// DirtyContentOpaque is a directory reported as one path (a gitlink or
	// nested repository). It is never traversed; SHA256 carries the
	// diagnostic/stat digest the fingerprint mixes in for it.
	DirtyContentOpaque DirtyContentState = "opaque"
)

// DirtyContent is one reported path's content identity as the sampler
// fingerprinted it.
type DirtyContent struct {
	// Path is the path relative to the worktree root, as git spells it.
	Path string
	// State is what was observed on disk.
	State DirtyContentState
	// Mode is the git octal mode of a present entry ("100644", "100755",
	// "120000"); empty otherwise.
	Mode string
	// SHA256 is the sha256 of the git blob encoding of a present entry's
	// raw bytes, or an opaque entry's diagnostic digest; empty for absent.
	SHA256 string
	// HeadEqual reports that the path contributes nothing to the
	// fingerprint because it equals HEAD: a present entry whose mode and
	// raw bytes equal the HEAD blob (staged residue), or an absent entry
	// HEAD does not hold either.
	HeadEqual bool
}

// Octal file modes git prints in the porcelain mode columns. Only these
// two need naming: 000000 marks a side where the path is absent, and
// 120000 marks a symlink.
const (
	absentMode  = "000000"
	symlinkMode = "120000"
)

// dirtyCommandFunc is the command boundary behind DirtySampler. Production
// uses gitcmd.Run; the seam keeps command-count and error tests deterministic.
type dirtyCommandFunc func(context.Context, string, ...string) ([]byte, error)

// DirtySampler samples a known worktree root without rediscovering it on every
// poll. It retains the last immutable tree oid and a bounded digest-only cache,
// never file bytes. Branch names are deliberately not cached:
// two branches may point at the same commit, and porcelain is authoritative for
// which one is checked out now.
type DirtySampler struct {
	root string
	run  dirtyCommandFunc
	// refreshRun is separate from read-only sampling's immediate cancellation.
	// Test-injected samplers leave it nil and keep their existing run seam.
	refreshRun dirtyCommandFunc

	mu        sync.Mutex
	commitOID string
	treeOID   string

	// Initialized under mu; the channel lease protects HEAD and digest caches
	// while allowing canceled callers to stop waiting for another sample.
	sampling         chan struct{}
	contentCache     map[string]dirtyContentMemo
	contentCacheRoot os.FileInfo

	// last is the most recent sample that passed every fence, and lastStarted
	// the instant its git status began. SampleSince hands it to callers that
	// only need a sample taken after some instant, so concurrent freshness
	// checks share one sample instead of queueing for one each.
	last        DirtySnapshot
	lastStarted time.Time
	// lastHead is the HEAD-deciding files' identity taken just before last's
	// status command started (ConfirmReadSet).
	lastHead headEvidence
	// gitDir/commonDir resolve the checkout's .git without running git, and
	// stampsTrusted whether its filesystem's change stamps are trusted; both
	// are computed once (read_set_confirm.go).
	gitDir, commonDir            string
	gitDirsResolved              bool
	stampsChecked, stampsTrusted bool
	// taken counts the samples this sampler has taken (not the ones
	// SampleSince shared).
	taken uint64
	// stampProven and fenced count dirty samples whose content evidence was
	// proven by change stamps alone, and those that needed the git fence
	// (contentFingerprint).
	stampProven, fenced uint64
	// urgentWaiting counts urgent callers waiting for the lease; yields how
	// often a holder gave the lease up to one.
	urgentWaiting atomic.Int32
	urgentActive  atomic.Int32
	yields        uint64
}

// NewDirtySampler constructs a sampler for a path already known to be the
// worktree root. seedCommit and seedTree may come from the checkout catalog;
// they are used only when both are valid object ids, and are always checked
// against porcelain's current commit before reuse.
func NewDirtySampler(root, seedCommit, seedTree string) (*DirtySampler, error) {
	abs, err := absDir(root)
	if err != nil {
		return nil, fmt.Errorf("gitstate: resolve checkout root %q: %w: %w", root, ErrDirtyUnavailable, err)
	}
	s := newDirtySampler(abs, seedCommit, seedTree, gitcmd.Run)
	s.refreshRun = gitcmd.RunIndexRefresh
	return s, nil
}

func newDirtySampler(root, seedCommit, seedTree string, run dirtyCommandFunc) *DirtySampler {
	s := &DirtySampler{root: root, run: run}
	if isOID(seedCommit) && !isZeroOID(seedCommit) && isOID(seedTree) && !isZeroOID(seedTree) {
		s.commitOID = seedCommit
		s.treeOID = seedTree
	}
	return s
}

// Sample reports HEAD and dirty paths. A clean committed checkout costs one Git
// command; dirty samples add a final status fence. An exact-commit tree lookup
// is needed only after its OID changes, never by resolving mutable HEAD twice.
func (s *DirtySampler) Sample(ctx context.Context) (DirtySnapshot, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return DirtySnapshot{}, err
	}
	defer release()
	return s.sampleHeld(ctx)
}

// SampleSince returns a sample whose git status began at or after since: the
// most recent such sample if one exists, one that was in flight when the
// caller arrived and started at or after since, or a new one. A sample that
// started before since is never returned, so a caller that passes the instant
// its request arrived gets a sample taken after the request — the guarantee a
// freshness check needs — while N concurrent callers pay for one sample
// between them instead of N serialized ones.
//
// The returned snapshot's slices are shared with other callers of SampleSince
// and must be treated as read-only.
func (s *DirtySampler) SampleSince(ctx context.Context, since time.Time) (DirtySnapshot, error) {
	snap, _, err := s.SampleSinceStarted(ctx, since)
	return snap, err
}

// SampleSinceStarted is SampleSince that also reports when the returned
// sample's git status began. A caller that has to decide which of several
// waiters one sample can answer compares that instant with each waiter's
// admission: the sample proves the working copy at an instant after every
// waiter admitted at or before it.
func (s *DirtySampler) SampleSinceStarted(ctx context.Context, since time.Time) (DirtySnapshot, time.Time, error) {
	if snap, started, ok := s.sampleStartedSince(since); ok {
		return snap, started, nil
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return DirtySnapshot{}, time.Time{}, err
	}
	defer release()
	// A sample that was in flight while this caller waited for the lease may
	// have started after since; it is as good as a new one.
	if snap, started, ok := s.sampleStartedSince(since); ok {
		return snap, started, nil
	}
	return s.sampleHeldStarted(ctx)
}

// LatestSampleSince returns the most recent sample whose git status began at
// or after since, and when it began, without taking one: ok is false when no
// such sample exists. It never waits for the sampling lease.
func (s *DirtySampler) LatestSampleSince(since time.Time) (DirtySnapshot, time.Time, bool) {
	return s.sampleStartedSince(since)
}

// LastSampleStarted reports when the most recent successful sample's git
// status began, zero when there has been none.
func (s *DirtySampler) LastSampleStarted() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStarted
}

func (s *DirtySampler) sampleStartedSince(since time.Time) (DirtySnapshot, time.Time, bool) {
	if s == nil || since.IsZero() {
		return DirtySnapshot{}, time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastStarted.IsZero() || s.lastStarted.Before(since) {
		return DirtySnapshot{}, time.Time{}, false
	}
	return s.last, s.lastStarted, true
}

// acquire takes the sampling lease; the caller must run release. An urgent
// caller (WithUrgentSample) is served before every other waiter, and a holder
// that is not urgent gives the lease up at its next git boundary while one
// waits (yieldToUrgent).
func (s *DirtySampler) acquire(ctx context.Context) (func(), error) {
	if s == nil || s.run == nil || strings.TrimSpace(s.root) == "" {
		return nil, fmt.Errorf("gitstate: dirty sampler is not initialized: %w", ErrDirtyUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("gitstate: sample canceled: %w: %w", ErrDirtyUnavailable, err)
	}
	sampling := s.lease()
	urgent := urgentSample(ctx)
	if err := s.takeLease(ctx, sampling, urgent); err != nil {
		return nil, fmt.Errorf("gitstate: wait for sampler: %w: %w", ErrDirtyUnavailable, err)
	}
	if urgent {
		s.urgentActive.Add(1)
		return func() { s.urgentActive.Add(-1); <-sampling }, nil
	}
	return func() { <-sampling }, nil
}

func (s *DirtySampler) lease() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sampling == nil {
		s.sampling = make(chan struct{}, 1)
	}
	return s.sampling
}

// urgentLeasePoll is how often a non-urgent waiter looks again while an
// urgent sample is waiting for, or holding, the lease.
const urgentLeasePoll = time.Millisecond

// takeLease blocks until the caller holds the lease. A non-urgent caller that
// gets the lease while an urgent one waits hands it straight back and tries
// again once no urgent caller waits.
func (s *DirtySampler) takeLease(ctx context.Context, sampling chan struct{}, urgent bool) error {
	if urgent {
		s.urgentWaiting.Add(1)
		defer s.urgentWaiting.Add(-1)
	}
	for {
		select {
		case sampling <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		if urgent || s.urgentWaiting.Load() == 0 {
			return nil
		}
		<-sampling
		timer := time.NewTimer(urgentLeasePoll)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// yieldToUrgent is a non-urgent sample's git boundary: when an urgent sample
// waits for the lease, the holder gives it up, lets the urgent sample run to
// its end, and takes it back before its own next step. The wait for the lease
// back ignores cancellation (it is bounded by one urgent sample), so the
// holder's deferred release always has a lease to release; a cancellation
// that arrived meanwhile is returned then.
func (s *DirtySampler) yieldToUrgent(ctx context.Context) error {
	if urgentSample(ctx) || s.urgentWaiting.Load() == 0 {
		return ctx.Err()
	}
	sampling := s.lease()
	s.mu.Lock()
	s.yields++
	s.mu.Unlock()
	<-sampling
	_ = s.takeLease(context.WithoutCancel(ctx), sampling, false)
	return ctx.Err()
}

type urgentSampleKey struct{}

// WithUrgentSample marks ctx's samples urgent: an edit's own working-copy
// sample, on the path between the tool call and its disk commit or its
// refresh ticket. An urgent sample takes the sampling lease before any other
// waiter, and a background sample holding it gives it up at its next git
// boundary (or between two hashed files) until the urgent one is done. The
// urgent sample therefore waits for at most one git command or one file hash
// of the background sample, never for the whole of it.
func WithUrgentSample(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, urgentSampleKey{}, true)
}

func urgentSample(ctx context.Context) bool {
	urgent, _ := ctx.Value(urgentSampleKey{}).(bool)
	return urgent
}

// UrgentYields reports how many times a non-urgent sample gave the lease up
// to an urgent one.
func (s *DirtySampler) UrgentYields() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.yields
}

// sampleHeld takes one sample; the caller holds the sampling lease. A sample
// that passes every fence is remembered for SampleSince, stamped with the
// instant its status command began.
func (s *DirtySampler) sampleHeld(ctx context.Context) (DirtySnapshot, error) {
	snap, _, err := s.sampleHeldStarted(ctx)
	return snap, err
}

// sampleHeldStarted is sampleHeld that also reports when the sample's git
// status began.
func (s *DirtySampler) sampleHeldStarted(ctx context.Context) (DirtySnapshot, time.Time, error) {
	started := time.Now()
	head := s.captureHeadEvidence()
	snap, err := s.sampleHeldUnrecorded(ctx, started, head)
	if err != nil {
		return DirtySnapshot{}, time.Time{}, err
	}
	s.mu.Lock()
	s.taken++
	if !started.Before(s.lastStarted) {
		s.last, s.lastStarted, s.lastHead = snap, started, head
	}
	s.mu.Unlock()
	return snap, started, nil
}

// SamplesTaken reports how many samples this sampler has taken and fenced
// successfully; a sample SampleSince shared is not counted again. It is the
// evidence of how many working-copy samples a caller's sequence cost.
func (s *DirtySampler) SamplesTaken() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taken
}

// started is the instant before head was captured and the status command
// began; head is the HEAD-deciding files' identity at that instant. Together
// they let the content fingerprint prove its dirty evidence without a second
// status (contentFingerprint).
func (s *DirtySampler) sampleHeldUnrecorded(ctx context.Context, started time.Time, head headEvidence) (DirtySnapshot, error) {
	out, err := s.run(ctx, s.root, "--no-optional-locks", "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--renames")
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: read status in %s: %w: %w", s.root, ErrDirtyUnavailable, err)
	}
	ref, commit, err := parseBranchStatusZ(out)
	if err == nil && ref == "(detached)" && commit != "" {
		// A detached HEAD whose file says so needs no symbolic-ref call: the
		// status already named the commit, and the fence below re-runs status.
		if detached, known := s.headFileDetached(); known && detached {
			ref = ""
		}
	}
	if err == nil && (ref == "(detached)" || ref == "(unknown)") {
		if err := s.yieldToUrgent(ctx); err != nil {
			return DirtySnapshot{}, fmt.Errorf("gitstate: read HEAD in %s: %w: %w", s.root, ErrDirtyUnavailable, err)
		}
		var symbolic []byte
		symbolic, err = s.run(ctx, s.root, "symbolic-ref", "-q", "HEAD")
		switch {
		case err == nil:
			ref = strings.TrimSpace(string(symbolic))
			if ref == "" {
				err = fmt.Errorf("%w: git symbolic-ref returned an empty HEAD", ErrDirtyUnavailable)
			}
		case exitCode(err) == 1 && commit != "":
			ref = ""
			err = nil
		case exitCode(err) == 1:
			err = fmt.Errorf("%w: detached HEAD without a commit", ErrDirtyUnavailable)
		default:
			err = fmt.Errorf("%w: git symbolic-ref HEAD: %w", ErrDirtyUnavailable, err)
		}
	}
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: read HEAD in %s: %w: %w", s.root, ErrDirtyUnavailable, err)
	}

	var tree string
	if commit != "" {
		if commit == s.commitOID && s.treeOID != "" {
			tree = s.treeOID
		} else {
			if err := s.yieldToUrgent(ctx); err != nil {
				return DirtySnapshot{}, fmt.Errorf("gitstate: resolve tree for %s in %s: %w: %w", commit, s.root, ErrDirtyUnavailable, err)
			}
			treeOut, treeErr := s.run(ctx, s.root, "rev-parse", "--verify", "-q", commit+"^{tree}")
			if treeErr != nil {
				// SampleHEAD historically treats an ordinary tree-resolution
				// failure as an unresolved tree, but cancellation must remain an
				// error so shutdown cannot publish a sample after its caller left.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return DirtySnapshot{}, fmt.Errorf("gitstate: resolve tree for %s in %s: %w: %w", commit, s.root, ErrDirtyUnavailable, ctxErr)
				}
			} else if candidate := strings.TrimSpace(string(treeOut)); isOID(candidate) && !isZeroOID(candidate) {
				tree = candidate
				s.commitOID = commit
				s.treeOID = tree
			}
		}
	}

	snap := DirtySnapshot{
		HeadRef:    ref,
		HeadCommit: commit,
		HeadTree:   tree,
		Entries:    parseStatusZ(out),
	}
	slices.SortStableFunc(snap.Entries, compareDirtyEntries)
	identityTree := snap.HeadTree
	if identityTree == "" && snap.HeadCommit != "" {
		identityTree = "unresolved-commit:" + snap.HeadCommit
	}
	snap.Fingerprint, snap.Contents, err = s.contentFingerprint(ctx, identityTree, snap.Entries, out, started, head)
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: fingerprint dirty content in %s: %w: %w", s.root, ErrDirtyUnavailable, err)
	}
	return snap, nil
}

// parseBranchStatusZ extracts the two mandatory --branch headers from a
// porcelain-v2 NUL stream. branch.head is short for normal local branches, so
// it is expanded to the full symbolic ref SampleHEAD has always returned.
func parseBranchStatusZ(out []byte) (ref, commit string, err error) {
	var oid, head string
	for _, chunk := range bytes.Split(out, []byte{0}) {
		record := string(chunk)
		if !strings.HasPrefix(record, "# ") {
			break
		}
		switch {
		case strings.HasPrefix(record, "# branch.oid "):
			oid = strings.TrimPrefix(record, "# branch.oid ")
		case strings.HasPrefix(record, "# branch.head "):
			head = strings.TrimPrefix(record, "# branch.head ")
		}
	}
	if oid == "" || head == "" {
		return "", "", errors.New("porcelain v2 omitted branch.oid or branch.head")
	}
	if oid != "(initial)" {
		if !isOID(oid) || isZeroOID(oid) {
			return "", "", fmt.Errorf("porcelain v2 reported invalid branch oid %q", oid)
		}
		commit = oid
	}
	switch head {
	case "(detached)", "(unknown)":
		return head, commit, nil
	default:
		return "refs/heads/" + head, commit, nil
	}
}

// SampleDirty reports how the working tree at dir differs from HEAD.
//
// The public sampler accepts any directory inside a checkout for compatibility.
// It resolves the root once, then delegates to the root-known sampler used by
// long-lived checkout coordinators. An unborn branch remains a valid result:
// HeadCommit and HeadTree are empty and git reports every tracked path as added.
//
// The listing comes from `git status --porcelain=v2 --branch -z`, with
// untracked files expanded individually and rename detection on. -z is
// required, not preferred: a path may legally contain spaces and newlines, and
// the rename record spells its second path in its own NUL-terminated chunk.
// Ignored paths are absent because --ignored is not passed. The status call
// runs under --no-optional-locks so observation never writes to the index.
//
// Fingerprint hashes HeadTree and actual reported raw bytes, type, executable
// mode and existence. Staging, timestamp and commit-only changes do not change
// regular-file or symlink identity. Only raw HEAD-blob equality removes staged
// residue; clean filters are never applied. Git-hidden changes and filter/CRLF
// equivalence are outside this guarantee. Opaque gitlinks/directories retain
// the existing diagnostic/stat contract, without recursive traversal.
// A clean filter can hide unchanged raw bytes after staging, removing a path
// from status; identity across that admission boundary is not guaranteed.
//
// The bounded digest-only cache requires the opened file's known-local FS and
// a digest freshly hashed after a quiet change-time window. Young, future,
// unsupported or observably clock-rolled-back evidence causes rereading, not
// waiting. Root, file and status fences reject detected inconsistent samples.
// These are ordinary local-kernel assumptions, not an atomic FS snapshot or
// proof against arbitrary concurrent writers; publication guards still apply.
func SampleDirty(ctx context.Context, dir string) (DirtySnapshot, error) {
	abs, err := absDir(dir)
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: resolve %q: %w: %w", dir, ErrDirtyUnavailable, err)
	}

	// Porcelain paths are relative to the worktree root, which is not
	// necessarily the directory that was queried, so compatibility callers
	// still need this one discovery command. Coordinators already know root.
	root, err := gitcmd.Output(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return DirtySnapshot{}, fmt.Errorf("gitstate: resolve worktree root for %s: %w: %w", abs, ErrDirtyUnavailable, err)
	}
	sampler, err := NewDirtySampler(root, "", "")
	if err != nil {
		return DirtySnapshot{}, err
	}
	return sampler.Sample(ctx)
}

// Dirty content identity is deliberately separate from porcelain diagnostics.
const (
	dirtyContentCacheLimit = 4096
	// A digest hashed during the uncertain window must never become reusable
	// merely because it ages: a fresh hash after the window is required.
	dirtyContentQuietWindow    = 2 * time.Second
	dirtyContentClockTolerance = 10 * time.Millisecond
)

type dirtyFileVersion struct {
	size, mtime, changeSec, changeNsec int64
	mode                               os.FileMode
	device, inode                      uint64
	cacheable                          bool
}
type dirtyContentMemo struct {
	version            dirtyFileVersion
	mode, sha1, sha256 string
	hashedAt           time.Time
	reusable           bool
}
type dirtyContentEvidence struct {
	info            os.FileInfo
	memo            dirtyContentMemo
	missing, opaque bool
}
type dirtyHeadEntry struct{ mode, oid string }

func dirtyVersion(info os.FileInfo) dirtyFileVersion {
	v := dirtyFileVersion{size: info.Size(), mtime: info.ModTime().UnixNano(), mode: info.Mode()}
	v.device, v.inode, v.changeSec, v.changeNsec, v.cacheable = dirtyChangeIdentity(info)
	return v
}

// A staged rename's HEAD blob belongs to the source, not its new destination.
// Unmerged records do not identify HEAD reliably and remain conservative.
func dirtyHeadEntries(status []byte) map[string]dirtyHeadEntry {
	head := make(map[string]dirtyHeadEntry)
	chunks := bytes.Split(status, []byte{0})
	for i := 0; i < len(chunks); i++ {
		record := string(chunks[i])
		switch {
		case strings.HasPrefix(record, "1 "):
			if fields, path, ok := splitRecord(record, 8); ok {
				head[path] = dirtyHeadEntry{mode: fields[3], oid: fields[6]}
			}
		case strings.HasPrefix(record, "2 "):
			fields, path, ok := splitRecord(record, 9)
			if !ok || i+1 >= len(chunks) {
				continue
			}
			i++
			if strings.HasPrefix(fields[8], "R") {
				head[string(chunks[i])] = dirtyHeadEntry{mode: fields[3], oid: fields[6]}
			}
			head[path] = dirtyHeadEntry{mode: absentMode}
		}
	}
	return head
}

// dirtyContentHashed, when set, runs after the dirty paths were hashed and
// before their evidence is proven. It is a test seam: it lets a test change a
// file inside the window the proof has to notice.
var dirtyContentHashed func()

// fenceDirtyEvidence is the git fence: a second status must equal the first,
// and every dirty path must still carry the identity it was hashed under
// (bytes re-read when the hash was not taken from a settled cache entry).
func (s *DirtySampler) fenceDirtyEvidence(ctx context.Context, root *os.Root, paths []string, evidence map[string]dirtyContentEvidence, status []byte) error {
	if err := s.yieldToUrgent(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.fenced++
	s.mu.Unlock()
	after, err := s.run(ctx, s.root, "--no-optional-locks", "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--renames")
	if err != nil {
		return err
	}
	if !bytes.Equal(status, after) {
		return errors.New("git dirty status changed while sampling")
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		observed := evidence[path]
		info, err := root.Lstat(path)
		if observed.missing {
			if !os.IsNotExist(err) {
				return fmt.Errorf("dirty path %q appeared while sampling", path)
			}
			continue
		}
		if err != nil || dirtyVersion(info) != dirtyVersion(observed.info) {
			return fmt.Errorf("dirty path %q changed while sampling", path)
		}
		if !observed.opaque && !observed.memo.reusable {
			// Young, unsupported or incomplete evidence must not certify bytes.
			current, err := readDirtyContent(ctx, root, path, info)
			if err != nil {
				return err
			}
			if current.mode != observed.memo.mode || current.sha256 != observed.memo.sha256 {
				return fmt.Errorf("dirty path %q changed while sampling", path)
			}
		}
	}
	return nil
}

func (s *DirtySampler) contentFingerprint(ctx context.Context, tree string, entries []DirtyEntry, status []byte, started time.Time, headAtStart headEvidence) (string, []DirtyContent, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	var canonical dirtyCanonical
	canonical.str("gortex.gitstate.dirty.content.v2")
	canonical.str(tree)
	if len(entries) == 0 {
		s.contentCache = nil
		s.contentCacheRoot = nil
		sum := sha256.Sum256(canonical.buf)
		return hex.EncodeToString(sum[:]), nil, nil
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	pinnedRoot, err := root.Open(".")
	if err != nil {
		return "", nil, err
	}
	rootInfo, err := pinnedRoot.Stat()
	closeErr := pinnedRoot.Close()
	if err != nil || closeErr != nil {
		return "", nil, errors.Join(err, closeErr)
	}
	cacheRootMatches := s.contentCacheRoot != nil && os.SameFile(rootInfo, s.contentCacheRoot)
	head := dirtyHeadEntries(status)
	paths := make([]string, 0, len(entries))
	byPath := make(map[string][]DirtyEntry, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
		byPath[entry.Path] = append(byPath[entry.Path], entry)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	nextCache := make(map[string]dirtyContentMemo, min(len(paths), dirtyContentCacheLimit))
	evidence := make(map[string]dirtyContentEvidence, len(paths))
	// Built from the same evidence as the canonical stream, in the same
	// sorted path order; exposed only after every fence below passes.
	contents := make([]DirtyContent, 0, len(paths))
	for _, path := range paths {
		if err := s.yieldToUrgent(ctx); err != nil {
			return "", nil, err
		}
		info, err := root.Lstat(path)
		if os.IsNotExist(err) {
			evidence[path] = dirtyContentEvidence{missing: true}
			if prior, ok := head[path]; ok && prior.mode == absentMode {
				contents = append(contents, DirtyContent{Path: path, State: DirtyContentAbsent, HeadEqual: true})
				continue
			}
			contents = append(contents, DirtyContent{Path: path, State: DirtyContentAbsent})
			canonical.str(path)
			canonical.str(absentMode)
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("stat dirty path %q: %w", path, err)
		}
		if info.IsDir() {
			// Preserve the existing opaque gitlink/nested-repository contract.
			// Do not introduce recursive traversal or require initialized submodules.
			evidence[path] = dirtyContentEvidence{info: info, opaque: true}
			opaque := fingerprintDirty(s.root, "", byPath[path])
			contents = append(contents, DirtyContent{Path: path, State: DirtyContentOpaque, SHA256: opaque})
			canonical.str(path)
			canonical.str("opaque-directory")
			canonical.str(opaque)
			continue
		}
		cached, found := s.contentCache[path]
		memo, _, err := dirtyContentForPath(ctx, root, path, info, cached, found && cacheRootMatches)
		if err != nil {
			return "", nil, err
		}
		evidence[path] = dirtyContentEvidence{info: info, memo: memo}
		if len(nextCache) < dirtyContentCacheLimit {
			nextCache[path] = memo
		}
		content := DirtyContent{Path: path, State: DirtyContentPresent, Mode: memo.mode, SHA256: memo.sha256}
		if prior, ok := head[path]; ok && prior.mode == memo.mode && (prior.oid == memo.sha1 || prior.oid == memo.sha256) {
			content.HeadEqual = true
			contents = append(contents, content)
			continue // Only actual raw HEAD-blob equality removes staged residue.
		}
		contents = append(contents, content)
		canonical.str(path)
		canonical.str(memo.mode)
		canonical.str(memo.sha256)
	}
	if dirtyContentHashed != nil {
		dirtyContentHashed()
	}
	// The status describes the working copy as git saw it from started on.
	// What remains to prove is that every hashed dirty path still held, when
	// it was hashed, what it held then. The change stamps prove it without
	// running git (stampsProveDirtyEvidence); anything they cannot prove is
	// fenced by a second status and a re-validation, as before.
	if s.stampsProveDirtyEvidence(ctx, root, paths, evidence, started, headAtStart) {
		s.mu.Lock()
		s.stampProven++
		s.mu.Unlock()
	} else if err := s.fenceDirtyEvidence(ctx, root, paths, evidence, status); err != nil {
		return "", nil, err
	}
	currentRoot, err := os.OpenFile(s.root, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, err
	}
	currentRootInfo, statErr := currentRoot.Stat()
	closeErr = currentRoot.Close()
	if statErr != nil || closeErr != nil || !currentRootInfo.IsDir() || !os.SameFile(rootInfo, currentRootInfo) {
		return "", nil, errors.New("checkout root changed while sampling dirty content")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	// Commit the bounded cache and its physical-root scope only after all fences.
	s.contentCache = nextCache
	s.contentCacheRoot = rootInfo
	sum := sha256.Sum256(canonical.buf)
	return hex.EncodeToString(sum[:]), contents, nil
}

func dirtyStampQuiet(version dirtyFileVersion, now time.Time) bool {
	if !version.cacheable {
		return false
	}
	stamp := time.Unix(version.changeSec, version.changeNsec)
	return !stamp.After(now) && now.Sub(stamp) >= dirtyContentQuietWindow
}
func dirtyClockContinuous(wallElapsed, monotonicElapsed time.Duration) bool {
	if wallElapsed < 0 || monotonicElapsed < 0 {
		return false
	}
	drift := wallElapsed - monotonicElapsed
	return drift >= -dirtyContentClockTolerance && drift <= dirtyContentClockTolerance
}
func dirtyMemoReusable(memo dirtyContentMemo, version dirtyFileVersion, now time.Time) bool {
	if !memo.reusable || memo.version != version || !dirtyStampQuiet(version, now) {
		return false
	}
	// This detects observable realtime jumps, not unobservable clock excursions
	// or arbitrary concurrent writers, and does not create an FS snapshot.
	return dirtyClockContinuous(time.Duration(now.UnixNano()-memo.hashedAt.UnixNano()), now.Sub(memo.hashedAt))
}
func dirtyContentForPath(ctx context.Context, root *os.Root, path string, info os.FileInfo, cached dirtyContentMemo, found bool) (dirtyContentMemo, bool, error) {
	if err := ctx.Err(); err != nil {
		return dirtyContentMemo{}, false, err
	}
	version := dirtyVersion(info)
	if found && dirtyMemoReusable(cached, version, time.Now()) {
		// Verify the actual opened file's filesystem, not merely the root mount.
		current, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return dirtyContentMemo{}, false, err
		}
		opened, statErr := current.Stat()
		local := statErr == nil && dirtyLocalFilesystem(current)
		closeErr := current.Close()
		if statErr != nil || closeErr != nil || !opened.Mode().IsRegular() || dirtyVersion(opened) != version {
			return dirtyContentMemo{}, false, fmt.Errorf("dirty file %q changed before cache lookup", path)
		}
		if local {
			return cached, true, nil
		}
	}
	memo, err := readDirtyContent(ctx, root, path, info)
	return memo, false, err
}
func readDirtyContent(ctx context.Context, root *os.Root, path string, before os.FileInfo) (dirtyContentMemo, error) {
	memo := dirtyContentMemo{version: dirtyVersion(before), hashedAt: time.Now()}
	if err := ctx.Err(); err != nil {
		return memo, err
	}
	var reader io.Reader
	var file *os.File
	readSize := before.Size()
	switch {
	case before.Mode()&os.ModeSymlink != 0:
		target, err := root.Readlink(path)
		if err != nil {
			return memo, err
		}
		memo.mode = symlinkMode
		readSize = int64(len(target)) // Windows link stat size need not equal text bytes.
		reader = strings.NewReader(target)
	case before.Mode().IsRegular():
		var err error
		// NONBLOCK closes the regular-file to FIFO race before fstat can reject it.
		file, err = root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return memo, err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || dirtyVersion(opened) != memo.version {
			return memo, fmt.Errorf("dirty file %q changed before reading", path)
		}
		memo.reusable = memo.version.cacheable && dirtyLocalFilesystem(file) && dirtyStampQuiet(memo.version, memo.hashedAt)
		memo.mode = "100644"
		if before.Mode()&0o100 != 0 {
			memo.mode = "100755"
		}
		reader = file
	default:
		return memo, fmt.Errorf("unsupported dirty file type at %q: %s", path, before.Mode())
	}
	var err error
	memo.sha1, memo.sha256, err = hashDirtyReader(ctx, reader, readSize)
	if err != nil {
		return memo, fmt.Errorf("read dirty path %q: %w", path, err)
	}
	after, err := root.Lstat(path)
	if err != nil || dirtyVersion(after) != memo.version {
		return memo, fmt.Errorf("dirty file %q changed while reading", path)
	}
	if file != nil {
		current, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return memo, err
		}
		currentInfo, statErr := current.Stat()
		closeErr := current.Close()
		originalInfo, originalErr := file.Stat()
		if statErr != nil || closeErr != nil || originalErr != nil || !currentInfo.Mode().IsRegular() || !os.SameFile(originalInfo, currentInfo) || dirtyVersion(currentInfo) != memo.version || dirtyVersion(originalInfo) != memo.version {
			return memo, fmt.Errorf("dirty file %q was replaced while reading", path)
		}
	}
	return memo, nil
}
func hashDirtyReader(ctx context.Context, reader io.Reader, size int64) (string, string, error) {
	if size < 0 {
		return "", "", errors.New("invalid dirty file size")
	}
	one, two := sha1.New(), sha256.New()
	header := "blob " + strconv.FormatInt(size, 10) + "\x00"
	_, _ = io.WriteString(one, header)
	_, _ = io.WriteString(two, header)
	writer := io.MultiWriter(one, two)
	var buffer [32 * 1024]byte
	var count int64
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		n, err := reader.Read(buffer[:])
		if n > 0 {
			empty = 0
			if int64(n) > size-count {
				return "", "", errors.New("dirty file grew while reading")
			}
			count += int64(n)
			_, _ = writer.Write(buffer[:n])
		} else if err == nil {
			empty++
			if empty >= 100 {
				return "", "", io.ErrNoProgress
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
	}
	if count != size {
		return "", "", errors.New("dirty file changed length while reading")
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(one.Sum(nil)), hex.EncodeToString(two.Sum(nil)), nil
}

// compareDirtyEntries orders entries by path, breaking ties on kind so
// the order stays total even when one path carries more than one entry.
func compareDirtyEntries(a, b DirtyEntry) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	return strings.Compare(string(a.Kind), string(b.Kind))
}

// parseStatusZ parses `git status --porcelain=v2 -z`.
//
// The stream is a flat sequence of NUL-terminated records, each opened
// by a one-character type: '1' a changed path, '2' a rename or copy, 'u'
// an unmerged path, '?' an untracked path. A '2' record is the only one
// that spans two chunks — its source path follows in the next one.
// Records of any other type (an ignored path, a header a newer git
// adds) are skipped rather than guessed at.
func parseStatusZ(out []byte) []DirtyEntry {
	chunks := bytes.Split(out, []byte{0})
	var entries []DirtyEntry
	for i := 0; i < len(chunks); i++ {
		rec := string(chunks[i])
		switch {
		case strings.HasPrefix(rec, "1 "):
			if e, ok := parseChangedRecord(rec); ok {
				entries = append(entries, e)
			}
		case strings.HasPrefix(rec, "2 "):
			var source string
			if i+1 < len(chunks) {
				source = string(chunks[i+1])
				i++
			}
			entries = append(entries, parseRenameRecord(rec, source)...)
		case strings.HasPrefix(rec, "u "):
			if e, ok := parseUnmergedRecord(rec); ok {
				entries = append(entries, e)
			}
		case strings.HasPrefix(rec, "? "):
			if path := rec[2:]; path != "" {
				entries = append(entries, DirtyEntry{Path: path, Kind: DirtyUntracked, Unstaged: true})
			}
		}
	}
	return entries
}

// splitRecord splits a porcelain v2 record into its n leading
// space-separated fields plus the path, which is everything left over
// and may itself contain spaces.
func splitRecord(rec string, n int) (fields []string, path string, ok bool) {
	parts := strings.SplitN(rec, " ", n+1)
	if len(parts) != n+1 || parts[n] == "" {
		return nil, "", false
	}
	return parts[:n], parts[n], true
}

// parseChangedRecord parses a '1' record:
//
//	1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
func parseChangedRecord(rec string) (DirtyEntry, bool) {
	fields, path, ok := splitRecord(rec, 8)
	if !ok || len(fields[1]) != 2 {
		return DirtyEntry{}, false
	}
	staged, unstaged := fields[1][0], fields[1][1]
	kind := kindFromModes(fields[3], fields[4], fields[5])
	if kind == "" {
		kind = kindFromStatus(staged, unstaged)
	}
	return DirtyEntry{
		Path:      path,
		Kind:      kind,
		Staged:    staged != '.',
		Unstaged:  unstaged != '.',
		Submodule: isSubmoduleField(fields[2]),
	}, true
}

// parseRenameRecord parses a '2' record plus the source path that
// follows it:
//
//	2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\0<source>
//
// git reports a rename as one record naming two paths. Callers compare
// snapshots path by path, so it is split back into the two path facts it
// stands for: the source is gone, and the destination is here and
// remembers where its content came from. A copy leaves the source in
// place, so only a rename contributes the deletion half.
func parseRenameRecord(rec, source string) []DirtyEntry {
	fields, path, ok := splitRecord(rec, 9)
	if !ok || len(fields[1]) != 2 || source == "" {
		return nil
	}
	staged, unstaged := fields[1][0], fields[1][1]
	submodule := isSubmoduleField(fields[2])

	var entries []DirtyEntry
	if strings.HasPrefix(fields[8], "R") {
		// The source only vanishes on the side that reported the rename.
		// A worktree modification reported alongside a staged rename
		// ("RM") belongs to the destination, not to the vanished source.
		entries = append(entries, DirtyEntry{
			Path:      source,
			Kind:      DirtyDeleted,
			Staged:    staged == 'R',
			Unstaged:  unstaged == 'R',
			Submodule: submodule,
		})
	}
	return append(entries, DirtyEntry{
		Path:      path,
		Kind:      DirtyRenamedFrom,
		Staged:    staged != '.',
		Unstaged:  unstaged != '.',
		OldPath:   source,
		Submodule: submodule,
	})
}

// parseUnmergedRecord parses a 'u' record:
//
//	u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
//
// A conflicted path differs from HEAD on both sides at once — the index
// holds three stages of it and the worktree holds whatever the merge
// left behind — so both flags are set regardless of which stage codes
// git printed.
func parseUnmergedRecord(rec string) (DirtyEntry, bool) {
	fields, path, ok := splitRecord(rec, 10)
	if !ok {
		return DirtyEntry{}, false
	}
	return DirtyEntry{
		Path:      path,
		Kind:      DirtyModified,
		Staged:    true,
		Unstaged:  true,
		Submodule: isSubmoduleField(fields[2]),
	}, true
}

// isSubmoduleField reports whether a porcelain <sub> field describes a
// submodule. The field is "N..." for anything else and "S<c><m><u>" for
// a submodule.
func isSubmoduleField(sub string) bool { return strings.HasPrefix(sub, "S") }

// kindFromModes classifies an entry from git's three octal mode columns:
// head to index is the staged transition, index to worktree the unstaged
// one. It returns "" when the modes explain nothing, leaving the status
// codes to decide.
//
// The modes are the only place a mode flip shows up. git reports
// `chmod +x` on otherwise untouched content as a plain 'M', with
// identical blob hashes on both sides — the changed octal column is the
// whole difference.
func kindFromModes(head, index, worktree string) DirtyKind {
	staged := modeTransition(head, index)
	unstaged := modeTransition(index, worktree)
	if staged == DirtySymlinkChanged || unstaged == DirtySymlinkChanged {
		return DirtySymlinkChanged
	}
	if staged == DirtyModeChanged || unstaged == DirtyModeChanged {
		return DirtyModeChanged
	}
	return ""
}

// modeTransition classifies one before/after pair of octal modes. An
// absent side is a creation or a deletion, which the status codes
// already describe, so only a change between two present modes counts.
func modeTransition(before, after string) DirtyKind {
	switch {
	case before == after, before == "", after == "":
		return ""
	case before == absentMode, after == absentMode:
		return ""
	case before == symlinkMode, after == symlinkMode:
		return DirtySymlinkChanged
	default:
		return DirtyModeChanged
	}
}

// kindFromStatus maps a porcelain XY status pair to a kind. The staged
// column wins when it is set, because it says what the index records;
// '.' means that side is unchanged.
//
// Codes describing a type change ('T') never decide anything here: a
// type change always moves an octal mode column, so kindFromModes has
// already classified it. Everything that is neither an add nor a delete
// is therefore a content modification.
func kindFromStatus(staged, unstaged byte) DirtyKind {
	code := staged
	if code == '.' {
		code = unstaged
	}
	switch code {
	case 'A':
		return DirtyAdded
	case 'D':
		return DirtyDeleted
	default:
		return DirtyModified
	}
}

// Canonical encoding.
//
// A fingerprint must be injective: no two distinct snapshots may encode
// to the same bytes. Joining fields with a separator cannot promise
// that, because a path may contain any byte but NUL — including whatever
// separator was picked. So every string is written length-prefixed,
// every flag as a fixed byte, every integer as a fixed 8 bytes, and the
// entry list behind its own count, all under a domain tag. Field
// boundaries are recoverable from the byte stream alone.
const dirtySnapshotTag = "gortex.gitstate.dirty.v1"

// dirtyCanonical accumulates the canonical byte stream.
type dirtyCanonical struct {
	buf []byte
}

// str writes len(s) as a uvarint followed by the raw bytes of s.
func (c *dirtyCanonical) str(s string) {
	c.buf = binary.AppendUvarint(c.buf, uint64(len(s)))
	c.buf = append(c.buf, s...)
}

// count writes the length of a list as a uvarint.
func (c *dirtyCanonical) count(n int) {
	c.buf = binary.AppendUvarint(c.buf, uint64(n))
}

// i64 writes n as 8 big-endian bytes — fixed width, so no value of one
// field can be mistaken for the start of the next.
func (c *dirtyCanonical) i64(n int64) {
	c.buf = binary.BigEndian.AppendUint64(c.buf, uint64(n))
}

// flag writes a bool as one fixed byte.
func (c *dirtyCanonical) flag(b bool) {
	var v byte
	if b {
		v = 1
	}
	c.buf = append(c.buf, v)
}

// fingerprintDirty reduces a sorted entry list to a lowercase hex
// sha256, mixing in the stat evidence behind each entry.
func fingerprintDirty(root, headCommit string, entries []DirtyEntry) string {
	var c dirtyCanonical
	c.str(dirtySnapshotTag)
	c.str(headCommit)
	c.count(len(entries))
	for _, e := range entries {
		c.str(e.Path)
		c.str(string(e.Kind))
		c.flag(e.Staged)
		c.flag(e.Unstaged)
		c.str(e.OldPath)
		size, mtime := statEvidence(root, e)
		c.i64(size)
		c.i64(mtime)
	}
	sum := sha256.Sum256(c.buf)
	return hex.EncodeToString(sum[:])
}

// statEvidence samples the size and modification time behind an entry.
// A deletion has nothing on disk to stat, and a path that vanished
// between the status call and the stat is treated the same way: it still
// fingerprints, it just contributes no stat evidence of its own.
//
// The link is statted, never its target, so swapping a symlink for one
// pointing somewhere else is visible.
func statEvidence(root string, e DirtyEntry) (size, mtimeNanos int64) {
	if e.Kind == DirtyDeleted {
		return 0, 0
	}
	fi, err := os.Lstat(filepath.Join(root, e.Path))
	if err != nil {
		return 0, 0
	}
	return fi.Size(), fi.ModTime().UnixNano()
}

// treeHoldsChunk bounds how many pathspecs one ls-tree invocation carries.
const treeHoldsChunk = 128

// TreeHoldsPaths reports, for each path, whether the committed tree holds it
// as a blob or symlink. It is the exact HEAD-membership question a working-tree
// sample cannot answer for a path it no longer reports (a path back at its
// committed state reports nothing, whether HEAD holds it or not). Paths are
// matched literally, never as pathspec patterns. An empty tree or path list
// asks git nothing.
func TreeHoldsPaths(ctx context.Context, root, tree string, paths []string) (map[string]bool, error) {
	out := make(map[string]bool, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	if !isOID(tree) || isZeroOID(tree) {
		return nil, fmt.Errorf("gitstate: tree membership needs a tree oid, got %q: %w", tree, ErrDirtyUnavailable)
	}
	for _, p := range paths {
		out[p] = false
	}
	for start := 0; start < len(paths); start += treeHoldsChunk {
		end := min(start+treeHoldsChunk, len(paths))
		args := append([]string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", tree, "--"}, paths[start:end]...)
		listed, err := gitcmd.Run(ctx, root, args...)
		if err != nil {
			return nil, fmt.Errorf("gitstate: list %s in %s: %w: %w", tree, root, ErrDirtyUnavailable, err)
		}
		for _, record := range bytes.Split(listed, []byte{0}) {
			// "<mode> SP <type> SP <oid> TAB <path>"
			tab := bytes.IndexByte(record, '\t')
			if tab < 0 {
				continue
			}
			fields := strings.Fields(string(record[:tab]))
			if len(fields) < 2 || fields[1] != "blob" {
				continue
			}
			if name := string(record[tab+1:]); name != "" {
				if _, asked := out[name]; asked {
					out[name] = true
				}
			}
		}
	}
	return out, nil
}
