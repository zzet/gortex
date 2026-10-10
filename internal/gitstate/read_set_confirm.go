package gitstate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// readSetChangeMargin is how far before a sample's start a change stamp must
// lie to count as "before the sample". A kernel may stamp inodes from a coarse
// clock (a jiffy behind the realtime clock time.Now reads), so a write made
// just after a sample started can carry a stamp just before it. Anything
// inside the margin is treated as changed: a dirty path is re-hashed, any
// other path sends the caller to a full sample.
const readSetChangeMargin = 50 * time.Millisecond

// ReadSetConfirmation is the verdict of ConfirmReadSet. Confirmed means every
// path the build read still holds the content the sample fingerprinted; when
// it is false, Reason says what could not be proven and the caller takes a
// full sample instead. It never means the checkout changed.
type ReadSetConfirmation struct {
	Confirmed bool
	Reason    string
	// Files and Dirs are how many paths were checked; Rehashed how many
	// dirty files were read again because their change stamp moved.
	Files    int
	Dirs     int
	Rehashed int
	// ContentProven is how many read files moved since the sample but were
	// confirmed by the bytes the build parsed (ConfirmReadSetContent): the
	// payload describes the sample for them, and the working copy has moved
	// past it.
	ContentProven int
}

// headEvidence is the change identity of the files that decide what HEAD
// resolves to — the worktree's HEAD, the loose ref it names, packed-refs —
// taken just before a sample's status command started. Equal identities at
// confirmation prove HEAD resolves the way the sample saw it.
type headEvidence struct {
	paths    []string
	versions []dirtyFileVersion
	present  []bool
	ok       bool
}

// captureHeadEvidence stats the HEAD-deciding files. It is best effort: a
// layout it cannot read (a reftable store, an unreadable .git) yields
// evidence that never confirms, never an error.
func (s *DirtySampler) captureHeadEvidence() headEvidence {
	gitDir, commonDir, ok := s.gitDirs()
	if !ok {
		return headEvidence{}
	}
	if _, err := os.Lstat(filepath.Join(commonDir, "reftable")); err == nil {
		return headEvidence{}
	}
	paths := []string{filepath.Join(gitDir, "HEAD"), filepath.Join(commonDir, "packed-refs")}
	head, err := os.ReadFile(paths[0])
	if err != nil {
		return headEvidence{}
	}
	if ref, symbolic := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: "); symbolic {
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
			return headEvidence{}
		}
		paths = append(paths, filepath.Join(commonDir, filepath.FromSlash(ref)))
	}
	e := headEvidence{paths: paths, versions: make([]dirtyFileVersion, len(paths)), present: make([]bool, len(paths)), ok: true}
	for i, p := range paths {
		info, err := os.Lstat(p)
		switch {
		case err == nil:
			e.versions[i], e.present[i] = dirtyVersion(info), true
			if !e.versions[i].cacheable {
				return headEvidence{}
			}
		case errors.Is(err, fs.ErrNotExist):
		default:
			return headEvidence{}
		}
	}
	return e
}

// unchanged reports whether every HEAD-deciding file still has the identity
// captured.
func (e headEvidence) unchanged() bool {
	if !e.ok {
		return false
	}
	for i, p := range e.paths {
		info, err := os.Lstat(p)
		switch {
		case err == nil:
			if !e.present[i] || dirtyVersion(info) != e.versions[i] {
				return false
			}
		case errors.Is(err, fs.ErrNotExist):
			if e.present[i] {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// gitDirs resolves the worktree's git directory and common directory from the
// .git entry at the root, without running git, and caches them.
func (s *DirtySampler) gitDirs() (gitDir, commonDir string, ok bool) {
	s.mu.Lock()
	if s.gitDirsResolved {
		gitDir, commonDir, ok = s.gitDir, s.commonDir, s.gitDir != ""
		s.mu.Unlock()
		return gitDir, commonDir, ok
	}
	s.mu.Unlock()
	gitDir, commonDir = resolveGitDirsFromDotGit(s.root)
	s.mu.Lock()
	s.gitDir, s.commonDir, s.gitDirsResolved = gitDir, commonDir, true
	s.mu.Unlock()
	return gitDir, commonDir, gitDir != ""
}

func resolveGitDirsFromDotGit(root string) (gitDir, commonDir string) {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return "", ""
	}
	switch {
	case info.IsDir():
		gitDir = dotGit
	case info.Mode().IsRegular():
		raw, err := os.ReadFile(dotGit)
		if err != nil {
			return "", ""
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
		if !ok || target == "" {
			return "", ""
		}
		gitDir = resolveAgainst(root, target)
	default:
		return "", ""
	}
	commonDir = gitDir
	if raw, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		if rel := strings.TrimSpace(string(raw)); rel != "" {
			commonDir = resolveAgainst(gitDir, rel)
		}
	}
	return gitDir, commonDir
}

// ConfirmReadSet proves, without running git, that the files a build read
// still hold what the before sample fingerprinted, so the build's payload
// describes before.Fingerprint. files are repository-relative paths the build
// read (or claims deleted); dirs are repository-relative directories whose
// every direct file entry the build may have read ("" is the root). A path
// the build did not read is not checked: a change there does not make the
// payload wrong for the fingerprint it names, and the next sample sees it.
//
// The evidence is the inode change stamp, which every write, truncate,
// rename, link and mode change moves and no caller can set:
//
//   - the latest sample this sampler took must carry before's fingerprint, and
//     the realtime clock must not have jumped since the build's own sample
//     (before, when this sampler took it; else that latest sample) started;
//   - the files that decide HEAD must be exactly as they were when it started;
//   - every read file, and every listed directory, must carry a change stamp
//     older than the sample's start (by readSetChangeMargin). A directory's
//     stamp moves when an entry is added, removed or renamed in it, which also
//     covers a read path deleted since. A dirty file whose stamp moved is
//     re-hashed and must match the sample's content identity.
//
// Anything that cannot be proven this way returns Confirmed=false with a
// reason, never an error: the caller falls back to a full sample, which is
// what decides. err is only the context's.
func (s *DirtySampler) ConfirmReadSet(ctx context.Context, before DirtySnapshot, files, dirs []string) (ReadSetConfirmation, error) {
	return s.confirmReadSet(ctx, before, files, dirs, nil)
}

// confirmReadSet is ConfirmReadSet, and ConfirmReadSetContent when parsed is
// not nil.
func (s *DirtySampler) confirmReadSet(ctx context.Context, before DirtySnapshot, files, dirs []string, parsed map[string]string) (ReadSetConfirmation, error) {
	var out ReadSetConfirmation
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if s == nil || s.root == "" {
		out.Reason = "no sampler"
		return out, nil
	}
	s.mu.Lock()
	last, started, evidence := s.last, s.lastStarted, s.lastHead
	s.mu.Unlock()
	own, owned := s.ownOrigin(before)
	switch {
	case started.IsZero():
		out.Reason = "no sample to confirm against"
		return out, nil
	case parsed != nil && !owned:
		out.Reason = "the build's sample was not taken by this sampler"
		return out, nil
	case parsed == nil && (before.Fingerprint == "" || last.Fingerprint != before.Fingerprint):
		out.Reason = "the latest sample does not carry the build's fingerprint"
		return out, nil
	}
	if owned {
		// The build's own sample decides, whatever was sampled after it: it
		// began before every read of the build, so its instant bounds every
		// change stamp, and its HEAD evidence is what the payload's HEAD
		// was. A later sample of the same fingerprint must not stand in for
		// it: a file read while it held other bytes and restored before that
		// sample began carries a stamp older than it.
		started, evidence = own.started, own.head
	}
	switch {
	case !dirtyClockContinuous(time.Duration(time.Now().UnixNano()-started.UnixNano()), time.Since(started)):
		out.Reason = "the realtime clock moved since the sample"
		return out, nil
	case !s.changeStampsTrusted():
		out.Reason = "the checkout's filesystem gives no change stamps"
		return out, nil
	case !evidence.unchanged():
		out.Reason = "HEAD, its ref or packed-refs changed since the sample"
		return out, nil
	}
	cutoff := started.Add(-readSetChangeMargin)
	// The stamps are read with one lstat per path; only a re-hash opens
	// content, and it does so beneath the checkout root.
	var root *os.Root
	defer func() {
		if root != nil {
			_ = root.Close()
		}
	}()

	contents := make(map[string]DirtyContent, len(before.Contents))
	for _, c := range before.Contents {
		contents[c.Path] = c
	}
	settled := func(v dirtyFileVersion) bool {
		return v.cacheable && time.Unix(v.changeSec, v.changeNsec).Before(cutoff)
	}

	check := make(map[string]struct{}, len(files))
	for _, f := range files {
		if clean, ok := cleanRepoPath(f); ok && clean != "." {
			check[clean] = struct{}{}
		}
	}
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		dir, ok := cleanRepoPath(d)
		if !ok {
			out.Reason = "a read directory is not repository-relative"
			return out, nil
		}
		out.Dirs++
		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(dir)))
		if errors.Is(err, fs.ErrNotExist) {
			// Gone since the sample, or never there: its parent's stamp
			// says which (checked with the missing-file rule below).
			check[dir] = struct{}{}
			continue
		}
		if err != nil || !info.IsDir() || !settled(dirtyVersion(info)) {
			out.Reason = "a read directory changed since the sample: " + dir
			return out, nil
		}
		entries, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(dir)))
		if err != nil {
			out.Reason = "cannot list a read directory: " + dir
			return out, nil
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == ".git" {
				continue
			}
			check[path.Join(dir, entry.Name())] = struct{}{}
		}
	}

	ordered := make([]string, 0, len(check))
	for p := range check {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	for _, p := range ordered {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.Files++
		content, dirty := contents[p]
		// A file the build parsed, which the sample holds as a regular
		// file's bytes, is judged by those bytes: other bytes are a payload
		// of another state whatever the stamps say, and the sample's bytes
		// are this payload's however the file moved since.
		parsedSum, wasParsed := parsed[p]
		contentPinned := wasParsed && dirty && content.State == DirtyContentPresent && content.Mode != symlinkMode
		if contentPinned && parsedSum != content.SHA256 {
			out.Reason = "a read file was parsed from bytes other than the sample's: " + p
			return out, nil
		}
		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(p)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if dirty && content.State == DirtyContentAbsent {
				continue
			}
			// Absent now and not known absent: it either never existed or
			// was removed since. The nearest existing ancestor's stamp
			// decides — a removal moves it.
			if !s.ancestorSettled(p, settled) {
				out.Reason = "a read path may have been removed since the sample: " + p
				return out, nil
			}
			continue
		case err != nil:
			out.Reason = "cannot stat a read path: " + p
			return out, nil
		case info.IsDir():
			out.Reason = "a read path is a directory: " + p
			return out, nil
		}
		version := dirtyVersion(info)
		if !version.cacheable {
			out.Reason = "no change stamp for a read path: " + p
			return out, nil
		}
		if settled(version) {
			if dirty && content.State == DirtyContentAbsent {
				out.Reason = "a read path the sample saw absent exists: " + p
				return out, nil
			}
			continue
		}
		// A parsed file stamped after the sample began has moved past it;
		// one stamped just before (inside the margin) may be exactly what the
		// sample hashed, and the re-hash below says which.
		if contentPinned && !time.Unix(version.changeSec, version.changeNsec).Before(started) {
			out.ContentProven++
			continue
		}
		if !dirty || content.State != DirtyContentPresent {
			out.Reason = "a read file changed since the sample: " + p
			return out, nil
		}
		if parsed != nil && !wasParsed {
			// The build holds no bytes of its own for this path (a gate read
			// it unrecorded, or it was not read at all): a re-hash equal to
			// the sample's says the working copy is back, not that a read
			// made while it moved took the sample's bytes.
			out.Reason = "a read file the build holds no bytes for changed since the sample: " + p
			return out, nil
		}
		if root == nil {
			if root, err = os.OpenRoot(s.root); err != nil {
				root = nil
				out.Reason = "cannot open the checkout root"
				return out, nil
			}
		}
		memo, err := readDirtyContent(ctx, root, p, info)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, ctxErr
			}
			if contentPinned {
				out.ContentProven++
				continue
			}
			out.Reason = "a read file changed while it was re-hashed: " + p
			return out, nil
		}
		out.Rehashed++
		if memo.sha256 != content.SHA256 || memo.mode != content.Mode {
			if contentPinned {
				out.ContentProven++
				continue
			}
			out.Reason = "a read file's content changed since the sample: " + p
			return out, nil
		}
	}
	out.Confirmed = true
	return out, nil
}

// ancestorSettled walks up from a missing path to its nearest existing
// ancestor directory and reports whether that directory's entries are
// unchanged since the cutoff.
func (s *DirtySampler) ancestorSettled(p string, settled func(dirtyFileVersion) bool) bool {
	for dir := path.Dir(p); ; dir = path.Dir(dir) {
		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(dir)))
		if err == nil {
			return info.IsDir() && settled(dirtyVersion(info))
		}
		if !errors.Is(err, fs.ErrNotExist) || dir == "." {
			return false
		}
	}
}

// changeStampsTrusted reports whether the checkout lives on a filesystem
// whose change stamps the digest cache already trusts (dirtyLocalFilesystem),
// caching the answer.
func (s *DirtySampler) changeStampsTrusted() bool {
	s.mu.Lock()
	if s.stampsChecked {
		trusted := s.stampsTrusted
		s.mu.Unlock()
		return trusted
	}
	s.mu.Unlock()
	trusted := false
	if dir, err := os.Open(s.root); err == nil {
		trusted = dirtyLocalFilesystem(dir)
		_ = dir.Close()
	}
	s.mu.Lock()
	s.stampsChecked, s.stampsTrusted = true, trusted
	s.mu.Unlock()
	return trusted
}

// cleanRepoPath normalizes a repository-relative slash path; "" is the root.
// Absolute paths and paths escaping the root are refused.
func cleanRepoPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	clean := path.Clean("./" + p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
