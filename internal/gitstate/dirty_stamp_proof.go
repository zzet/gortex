package gitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// What a dirty sample promises, and how it is proven.
//
// A sample is a git status (HEAD, branch, index against HEAD, the dirty and
// untracked paths with their codes) plus the content identity of every
// reported path. The fingerprint must describe one state of the checkout, so
// the content hashed after the status must be the content the status saw.
//
// The git fence (fenceDirtyEvidence) proved it by running status a second
// time after hashing: equal output means git's view at the end (t2) was the
// view at the start (t0), and every dirty path re-checked at t2 against the
// identity it was hashed under (bytes re-read unless they came from a settled
// cache entry) means the hashed bytes were the bytes at t2. The sample is then
// the state at t2, and also refuses any movement of git's view during
// [t0, t2] — including of paths git reported clean.
//
// The stamp proof (stampsProveDirtyEvidence) proves the same fingerprint
// without git, as the state at t0 (the instant before HEAD was captured and
// the status began). Every inode change — write, truncate, rename over,
// chmod, link — moves the change stamp, and no caller can set it:
//
//   - every present dirty path still has the identity it was hashed under, and
//     its change stamp lies before t0 by readSetChangeMargin (a coarse kernel
//     clock can stamp a write made just after t0 just before it): its bytes did
//     not change from before t0 until after they were hashed, so the hash is
//     the t0 content;
//   - every dirty path seen absent is still absent and its nearest existing
//     ancestor directory's stamp lies before that cutoff (a removal or a
//     creation moves it), so it was absent at t0;
//   - an opaque dirty directory (gitlink, nested repository) has the identity
//     and a settled stamp;
//   - the HEAD-deciding files (HEAD, its ref, packed-refs) are exactly as they
//     were at t0, so the HEAD the status reported is HEAD at t0;
//   - the filesystem's stamps are trusted (local) and the realtime clock did
//     not jump since t0 (the stamps and the cutoff are on that clock).
//
// What it no longer refuses is movement after t0 of paths git reported clean,
// of the untracked set, or of the index: those are a later state, which the
// next sample sees, and which a build is protected from by its own read-set
// confirmation (ConfirmReadSet) against this sample's start. The exact-snapshot
// promise a refresh ticket carries is unchanged: its fingerprint is the dirty
// content of one state of the working copy, taken after the ticket's request.
// Anything the stamps cannot prove — a path changed at or after the cutoff,
// untrusted stamps, a moved HEAD, a clock jump — falls back to the git fence,
// so the proof never accepts less than the fence would.

// stampsProveDirtyEvidence reports whether the change stamps prove the hashed
// dirty evidence describes the working copy at started (see above). false
// means "not proven", never "changed": the caller runs the git fence.
func (s *DirtySampler) stampsProveDirtyEvidence(ctx context.Context, root *os.Root, paths []string, evidence map[string]dirtyContentEvidence, started time.Time, head headEvidence) bool {
	if started.IsZero() || !head.ok {
		return false
	}
	if !dirtyClockContinuous(time.Duration(time.Now().UnixNano()-started.UnixNano()), time.Since(started)) {
		return false
	}
	if !s.changeStampsTrusted() {
		return false
	}
	cutoff := started.Add(-readSetChangeMargin)
	settled := func(v dirtyFileVersion) bool {
		return v.cacheable && time.Unix(v.changeSec, v.changeNsec).Before(cutoff)
	}
	for _, path := range paths {
		observed, ok := evidence[path]
		if !ok {
			return false
		}
		info, err := root.Lstat(path)
		if observed.missing {
			if !errors.Is(err, fs.ErrNotExist) || !s.ancestorSettled(path, settled) {
				return false
			}
			continue
		}
		if err != nil || observed.info == nil {
			return false
		}
		version := dirtyVersion(info)
		if version != dirtyVersion(observed.info) {
			return false
		}
		if !settled(version) && !knownWriteProves(ctx, root, path, info, observed) {
			return false
		}
	}
	// Last: HEAD's files are checked after every path, so a HEAD move while
	// the paths were checked is seen too.
	return head.unchanged()
}

// DirtyContentProofs reports how many dirty samples had their content
// evidence proven by change stamps alone (stamped) and how many needed the git
// fence (fenced). A clean sample needs neither and is counted in neither.
func (s *DirtySampler) DirtyContentProofs() (stamped, fenced uint64) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stampProven, s.fenced
}

// A known write is one file the caller itself just wrote, with the exact bytes
// it wrote. An edit's ticket capture samples the working copy right after its
// own write, when that file's change stamp is necessarily young: the stamps
// cannot prove it, and without this the capture paid the git fence for the
// one file whose content it already knows.
//
// knownWriteProves accepts a young path only when it is the known write and
// the bytes hashed for the sample are exactly the bytes written: the file
// still has the identity it was hashed under (checked by the caller, so the
// bytes re-read are the bytes hashed), and it re-reads to the written
// SHA-256. Every other young path still falls back to the fence.
// The sample then describes the working copy at its start, as a sample the
// stamps prove on their own does: the written bytes were on disk before the
// sample began (the caller wrote them first), and nothing wrote the file
// between its hash and this check.
type knownWrite struct {
	path   string // repository-relative, slash-separated, as git reports it
	sha256 string // hex SHA-256 of the raw bytes written
}

type knownWriteKey struct{}

// WithKnownWrite tells a sample taken under ctx that the caller wrote relPath
// (relative to the checkout root) with raw bytes hashing to rawSHA256 (hex
// SHA-256) just before sampling, so the stamp proof can prove that file by
// content instead of falling back to the git fence (see knownWriteProves).
func WithKnownWrite(ctx context.Context, relPath, rawSHA256 string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	clean, ok := cleanRepoPath(filepath.ToSlash(relPath))
	if !ok || clean == "." || rawSHA256 == "" {
		return ctx
	}
	return context.WithValue(ctx, knownWriteKey{}, knownWrite{path: clean, sha256: rawSHA256})
}

func knownWriteProves(ctx context.Context, root *os.Root, path string, info os.FileInfo, observed dirtyContentEvidence) bool {
	known, ok := ctx.Value(knownWriteKey{}).(knownWrite)
	if !ok || known.path != path || observed.opaque || observed.missing || !info.Mode().IsRegular() {
		return false
	}
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || dirtyVersion(opened) != dirtyVersion(info) {
		return false
	}
	// The file has the identity it was hashed under (the caller checked,
	// and the open above re-checks), so these are the bytes the sample
	// hashed; they must be the bytes written.
	raw := sha256.New()
	n, err := io.Copy(raw, file)
	if err != nil || n != opened.Size() {
		return false
	}
	return hex.EncodeToString(raw.Sum(nil)) == known.sha256
}

// KnownWriteFrom reports the known write ctx carries (WithKnownWrite).
func KnownWriteFrom(ctx context.Context) (relPath, rawSHA256 string, ok bool) {
	if ctx == nil {
		return "", "", false
	}
	known, ok := ctx.Value(knownWriteKey{}).(knownWrite)
	return known.path, known.sha256, ok
}
