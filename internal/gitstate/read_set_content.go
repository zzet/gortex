package gitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// sampleEvidenceHistory is how many recent fenced samples LatestSampleOf can
// answer from. It is a cache: a sample pushed out of it is still confirmed by
// a build that holds it (DirtySnapshot.origin), and a refresh ticket the
// coordinator completes against a publication's own sample does not ask it.
const sampleEvidenceHistory = 8

// sampleOrigin is what a fenced sample keeps of itself on the snapshot it
// returns: the sampler that took it, the fingerprint it fenced, when its
// status command began and the identity of the files that decided HEAD just
// before it did.
type sampleOrigin struct {
	sampler     *DirtySampler
	fingerprint string
	started     time.Time
	head        headEvidence
}

// sampleEvidence is one entry of the recent-sample cache: the sample itself
// and when its status command began.
type sampleEvidence struct {
	fingerprint string
	started     time.Time
	snap        DirtySnapshot
}

// noteSampleEvidenceLocked records a fenced sample in the recent-sample
// cache; s.mu is held.
func (s *DirtySampler) noteSampleEvidenceLocked(snap DirtySnapshot, started time.Time) {
	if snap.Fingerprint == "" {
		return
	}
	if len(s.recent) >= sampleEvidenceHistory {
		s.recent = append(s.recent[:0], s.recent[len(s.recent)-sampleEvidenceHistory+1:]...)
	}
	s.recent = append(s.recent, sampleEvidence{fingerprint: snap.Fingerprint, started: started, snap: snap})
}

// ownOrigin is the evidence of the sample before is, when this sampler took
// it and it still names the fingerprint it fenced.
func (s *DirtySampler) ownOrigin(before DirtySnapshot) (*sampleOrigin, bool) {
	o := before.origin
	if o == nil || o.sampler != s || o.fingerprint == "" || o.fingerprint != before.Fingerprint {
		return nil, false
	}
	return o, true
}

// LatestSampleOf returns the latest-begun of the sampler's recent fenced
// samples that carried fingerprint and began at or after since, and when it
// began; ok is false when there is none. It never samples. It answers a
// question LatestSampleSince cannot once a newer sample of another state has
// replaced the latest: was the working copy in this state at some instant
// after since? A publication of that state then answers a waiter admitted at
// or before since, whatever the working copy did afterwards. It is a cache
// of the last sampleEvidenceHistory samples: a caller that holds the sample
// it published (DirtySnapshot.SampleStarted) asks that instead.
//
// The returned snapshot's slices are shared and must be treated as read-only.
func (s *DirtySampler) LatestSampleOf(fingerprint string, since time.Time) (DirtySnapshot, time.Time, bool) {
	if s == nil || fingerprint == "" {
		return DirtySnapshot{}, time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out sampleEvidence
	found := false
	for _, e := range s.recent {
		if e.fingerprint == fingerprint && (!found || e.started.After(out.started)) {
			out, found = e, true
		}
	}
	if !found || out.started.Before(since) {
		return DirtySnapshot{}, time.Time{}, false
	}
	return out.snap, out.started, true
}

// BlobSHA256 is the content identity DirtyContent.SHA256 carries for a present
// regular file: the sha256 of the git blob encoding of its bytes. A build
// that holds the bytes it parsed names them with it (ConfirmReadSetContent).
func BlobSHA256(data []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ConfirmReadSetContent is ConfirmReadSet for a build that also reports the
// bytes it parsed. parsed maps a repository-relative path to BlobSHA256 of
// the bytes every reader of the build took for it; a path whose readers did
// not agree, or whose bytes are unknown, is left out.
//
// It proves the payload describes before — a state the checkout really was
// in — even when the working copy has moved on since:
//
//   - before must be a sample this sampler took and fenced, which the build
//     holds itself (DirtySnapshot.origin), not necessarily the latest: its
//     start bounds every change stamp below, and the HEAD evidence taken with
//     it must still hold. Every read of the build came after that start, so a
//     path whose stamp is older than it held the same bytes at every read; a
//     later sample of the same fingerprint never stands in for it (a file
//     read while it held other bytes and restored before that later sample
//     would pass against it);
//   - a read file the sample holds as a regular file's bytes and the build
//     parsed is confirmed by its parsed bytes alone: equal to the sample's
//     content, it is confirmed however its stamp moved since (ContentProven
//     counts those); other bytes refuse;
//   - every other read path, every read directory, a deleted or renamed path
//     and HEAD's files are confirmed as ConfirmReadSet confirms them, by
//     change stamps — except that a dirty file the build holds no bytes for
//     (parsed lacks it) whose stamp moved is refused, not re-hashed: the
//     build may have read it unrecorded while it held other bytes, and a
//     re-hash equal to the sample's would hide that.
//
// Confirmed with ContentProven > 0 means the build is a proof of a past
// sample: the caller publishes it under before.Fingerprint and builds the
// newer state next. Not confirmed sends the caller to a full sample; when
// that sample equals before, the full sample proves only the paths it
// reports, and the caller proves the build's reads of every other path with
// ConfirmReadsHold.
func (s *DirtySampler) ConfirmReadSetContent(ctx context.Context, before DirtySnapshot, files, dirs []string, parsed map[string]string) (ReadSetConfirmation, error) {
	if parsed == nil {
		parsed = map[string]string{}
	}
	return s.confirmReadSet(ctx, before, files, dirs, parsed)
}

// ReadAbsent is the content identity a build records for a read that found
// no file at a path (ConfirmReadsHold).
const ReadAbsent = "absent"

// ConfirmReadsHold proves that reads a build recorded took what the working
// copy holds now. reads maps a repository-relative path to BlobSHA256 of the
// bytes every reader of the build took from it, or ReadAbsent for a read that
// found no file; the caller leaves out the paths its sample decides (it
// judges those against the sample's content identities).
//
// The caller asks it after a full sample found the working copy back in the
// build's sampled state. The fingerprint equality says nothing about a path
// the sample holds as clean (or does not report at all) that was read while
// it held other bytes and restored before that full sample: the payload then
// describes a state that never existed, under the sample's fingerprint, and
// the next sample, of the same state, never rebuilds it. A read whose bytes
// (or absence) differ from what the path holds now is such a read.
//
// A path whose change stamp is older than the build's own sample start (by
// readSetChangeMargin) held the same bytes since before any read of the
// build, and is not read again; every other path is re-hashed. It returns the
// first path, in path order, whose recorded read the working copy refutes,
// and "" when every read holds. A path that moves after the full sample can
// refute a read that was in fact the sample's: the caller tears a build it
// could have published, never the converse. err is the context's, or a
// sampler without a root.
func (s *DirtySampler) ConfirmReadsHold(ctx context.Context, before DirtySnapshot, reads map[string]string) (string, error) {
	if s == nil || s.root == "" {
		return "", errors.New("gitstate: no sampler to confirm reads against")
	}
	var cutoff time.Time
	if own, owned := s.ownOrigin(before); owned && s.changeStampsTrusted() &&
		dirtyClockContinuous(time.Duration(time.Now().UnixNano()-own.started.UnixNano()), time.Since(own.started)) {
		cutoff = own.started.Add(-readSetChangeMargin)
	}
	return confirmReadsHold(ctx, s.root, cutoff, reads)
}

// ConfirmReadSetSettled proves the reads a build took without recording what
// they found — a per-directory ignore file the walk gate read, a manifest read
// by name, a directory listed, a read whose bytes went unrecorded — by the
// one thing that covers them all: the paths did not change while the build
// read them. Every path of the read set (files, and dirs, with their direct
// file entries when entries is set) other than the paths judged names must
// carry a change stamp older than the build's own sample start (by
// readSetChangeMargin); a path absent now must be absent in the sample too,
// or lie under a settled ancestor. judged are the paths whose reads the
// caller proves itself, by their recorded bytes, absence or existence answer
// (ConfirmReadsHold, its sample).
//
// The caller asks it, with ConfirmReadsHold, after a full sample found the
// working copy back in the build's sampled state: an unrecorded read of a
// path moved and restored since the sample (an ignore file rewritten to
// exclude a changed file, then put back) built a state that never existed,
// and only the path's stamp still shows the move. It returns the first such
// path in path order, "" when every path held. proven is false when stamps
// cannot decide — before is not this sampler's own sample, the filesystem
// gives no change stamps, the realtime clock jumped — and the caller has
// only the full sample. A path written with the same bytes, or inside the
// margin before the sample, is reported as moved too: the caller tears a
// build it could have published, never the converse. err is the context's.
func (s *DirtySampler) ConfirmReadSetSettled(ctx context.Context, before DirtySnapshot, files, dirs []string, entries bool, judged map[string]bool) (moved string, proven bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if s == nil || s.root == "" {
		return "", false, nil
	}
	own, owned := s.ownOrigin(before)
	if !owned || !s.changeStampsTrusted() ||
		!dirtyClockContinuous(time.Duration(time.Now().UnixNano()-own.started.UnixNano()), time.Since(own.started)) {
		return "", false, nil
	}
	cutoff := own.started.Add(-readSetChangeMargin)
	settled := func(v dirtyFileVersion) bool {
		return v.cacheable && time.Unix(v.changeSec, v.changeNsec).Before(cutoff)
	}
	absent := make(map[string]bool)
	for _, c := range before.Contents {
		if c.State == DirtyContentAbsent {
			absent[c.Path] = true
		}
	}
	check := make(map[string]struct{}, len(files))
	for _, f := range files {
		if clean, ok := cleanRepoPath(f); ok && clean != "." && !judged[clean] {
			check[clean] = struct{}{}
		}
	}
	orderedDirs := make([]string, 0, len(dirs))
	for _, d := range dirs {
		dir, ok := cleanRepoPath(d)
		if !ok {
			return d, true, nil
		}
		orderedDirs = append(orderedDirs, dir)
	}
	sort.Strings(orderedDirs)
	for _, dir := range orderedDirs {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		abs := filepath.Join(s.root, filepath.FromSlash(dir))
		info, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			if !s.ancestorSettled(dir, settled) {
				return dir, true, nil
			}
			continue
		}
		if err != nil || !settled(dirtyVersion(info)) {
			return dir, true, nil
		}
		if !entries || !info.IsDir() {
			continue
		}
		listed, err := os.ReadDir(abs)
		if err != nil {
			return dir, true, nil
		}
		for _, entry := range listed {
			if p := path.Join(dir, entry.Name()); !entry.IsDir() && entry.Name() != ".git" && !judged[p] {
				check[p] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(check))
	for p := range check {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	for _, p := range ordered {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if !absent[p] && !s.ancestorSettled(p, settled) {
				return p, true, nil
			}
		case err != nil, !settled(dirtyVersion(info)), absent[p]:
			return p, true, nil
		}
	}
	return "", true, nil
}

// ConfirmReadsHoldAt is ConfirmReadsHold for a build with no sampler of its
// own (a sample taken by gitstate.SampleDirty): it has no sample start to
// trust a change stamp against, so every read path is re-hashed.
func ConfirmReadsHoldAt(ctx context.Context, root string, reads map[string]string) (string, error) {
	return confirmReadsHold(ctx, root, time.Time{}, reads)
}

// confirmReadsHold is ConfirmReadsHold under root, skipping the re-hash of a
// path stamped before cutoff when it is set.
func confirmReadsHold(ctx context.Context, root string, cutoff time.Time, reads map[string]string) (string, error) {
	if err := ctx.Err(); err != nil || len(reads) == 0 {
		return "", err
	}
	if root == "" {
		return "", errors.New("gitstate: no checkout root to confirm reads against")
	}
	ordered := make([]string, 0, len(reads))
	for p := range reads {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	for _, p := range ordered {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		want := reads[p]
		clean, ok := cleanRepoPath(p)
		if !ok || clean == "." || want == "" {
			return p, nil
		}
		abs := filepath.Join(root, filepath.FromSlash(clean))
		// Followed, as the readers' whole-file reads follow a symlink.
		info, err := os.Stat(abs)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if want != ReadAbsent {
				return p, nil
			}
			continue
		case err != nil, info.IsDir(), want == ReadAbsent:
			return p, nil
		}
		if version := dirtyVersion(info); !cutoff.IsZero() && version.cacheable &&
			time.Unix(version.changeSec, version.changeNsec).Before(cutoff) {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil || BlobSHA256(data) != want {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			return p, nil
		}
	}
	return "", nil
}
