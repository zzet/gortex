package gitstate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A racily clean index is one whose entries git cannot trust by their stat
// data: an entry recorded in the same second as (or after) the index file was
// written might have been modified again within that second, so git re-reads
// and hashes its content on every status. When git writes such an index it
// also "smudges" those entries (records size 0) so the next reader hashes
// them. A checkout created by `git worktree add` or a checkout of many files
// in one second is born in that state.
//
// A reader that writes the index back (plain `git status` takes the index
// lock opportunistically) heals it once. This daemon never does: every
// working-copy sample runs `git --no-optional-locks status`, which refreshes
// in memory and discards the result, so a racily clean checkout paid a full
// content hash of every tracked file on every sample, forever — about 0.4 s
// per status on this repository, four statuses per edit.
//
// RacyIndex detects the state from the index file alone (no git), and
// RefreshRacyIndex heals it with one `git update-index -q --refresh`, which
// takes the index lock and writes the refreshed stat data back.

// RacyIndexReport is what a read of the checkout's index found.
type RacyIndexReport struct {
	// Known is false when the index could not be read or is in a form this
	// reader does not parse (a split index, an unknown version, a hash other
	// than SHA-1 or SHA-256); the other fields are then zero.
	Known bool
	// Entries is the number of index entries.
	Entries int
	// Smudged counts non-empty regular-file or symlink entries recorded with
	// size 0: git will hash their content on every status.
	Smudged int
	// Racy counts entries whose recorded modification time is not earlier
	// than the index file's own: git treats them as possibly changed.
	Racy int
}

// NeedsRefresh reports whether a refresh would save content hashing on every
// status.
func (r RacyIndexReport) NeedsRefresh() bool { return r.Known && (r.Smudged > 0 || r.Racy > 0) }

// racyIndexMaxBytes bounds the index this reader parses; a larger index is
// reported unknown rather than read.
const racyIndexMaxBytes = 256 << 20

// emptyBlobSHA1 and emptyBlobSHA256 name the empty blob: an entry of an empty
// file legitimately records size 0.
var (
	emptyBlobSHA1   = []byte{0xe6, 0x9d, 0xe2, 0x9b, 0xb2, 0xd1, 0xd6, 0x43, 0x4b, 0x8b, 0x29, 0xae, 0x77, 0x5a, 0xd8, 0xc2, 0xe4, 0x8c, 0x53, 0x91}
	emptyBlobSHA256 = []byte{0x47, 0x3a, 0x0f, 0x4c, 0x3b, 0xe8, 0xa9, 0x36, 0x81, 0xa2, 0x67, 0xe3, 0xb1, 0xe9, 0xa7, 0xdc, 0xda, 0x11, 0x85, 0x43, 0x6f, 0xe1, 0x41, 0xf7, 0x74, 0x91, 0x20, 0xa3, 0x03, 0x72, 0x18, 0x13}
)

// RacyIndex reads the checkout's index and reports its racily clean entries.
func (s *DirtySampler) RacyIndex() RacyIndexReport {
	if s == nil || s.root == "" {
		return RacyIndexReport{}
	}
	gitDir, commonDir, ok := s.gitDirs()
	if !ok {
		return RacyIndexReport{}
	}
	return readRacyIndex(filepath.Join(gitDir, "index"), repositoryHashSize(commonDir))
}

// repositoryHashSize is the object-id width the repository's config names.
func repositoryHashSize(commonDir string) int {
	raw, err := os.ReadFile(filepath.Join(commonDir, "config"))
	if err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, found := strings.Cut(strings.TrimSpace(line), "=")
			if found && strings.EqualFold(strings.TrimSpace(key), "objectformat") &&
				strings.EqualFold(strings.TrimSpace(value), "sha256") {
				return 32
			}
		}
	}
	return 20
}

func readRacyIndex(path string, hashSize int) RacyIndexReport {
	info, err := os.Stat(path)
	if err != nil || info.Size() < 12 || info.Size() > racyIndexMaxBytes {
		return RacyIndexReport{}
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 12 || !bytes.Equal(data[:4], []byte("DIRC")) {
		return RacyIndexReport{}
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version < 2 || version > 4 {
		return RacyIndexReport{}
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	indexSec := info.ModTime().Unix()
	emptyBlob := emptyBlobSHA1
	if hashSize == 32 {
		emptyBlob = emptyBlobSHA256
	}
	fixed := 40 + hashSize + 2 // stat fields, object id, flags
	report := RacyIndexReport{Entries: count}
	offset := 12
	var previousPath []byte
	for i := 0; i < count; i++ {
		if offset+fixed > len(data) {
			return RacyIndexReport{}
		}
		entry := data[offset:]
		mtimeSec := int64(binary.BigEndian.Uint32(entry[8:12]))
		mode := binary.BigEndian.Uint32(entry[24:28])
		size := binary.BigEndian.Uint32(entry[36:40])
		oid := entry[40 : 40+hashSize]
		flags := binary.BigEndian.Uint16(entry[40+hashSize : fixed])
		header := fixed
		if flags&0x4000 != 0 {
			if version < 3 {
				return RacyIndexReport{}
			}
			header += 2
		}
		if offset+header > len(data) {
			return RacyIndexReport{}
		}
		rest := data[offset+header:]
		var consumed int
		if version == 4 {
			strip, n := binary.Uvarint(rest)
			if n <= 0 || int(strip) > len(previousPath) {
				return RacyIndexReport{}
			}
			end := bytes.IndexByte(rest[n:], 0)
			if end < 0 {
				return RacyIndexReport{}
			}
			current := append(append([]byte(nil), previousPath[:len(previousPath)-int(strip)]...), rest[n:n+end]...)
			previousPath = current
			consumed = header + n + end + 1
		} else {
			end := bytes.IndexByte(rest, 0)
			if end < 0 {
				return RacyIndexReport{}
			}
			consumed = (header + end + 8) &^ 7 // NUL-padded to a multiple of eight
		}
		offset += consumed
		kind := mode & 0o170000
		if kind != 0o100000 && kind != 0o120000 {
			continue // gitlinks carry no content stat worth refreshing
		}
		if size == 0 && !bytes.Equal(oid, emptyBlob) {
			report.Smudged++
		}
		if mtimeSec >= indexSec {
			report.Racy++
		}
	}
	// Extensions follow the entries, then the trailing checksum. A split index
	// ("link") keeps its entries in a shared index this reader does not
	// follow, so its report would be incomplete.
	for offset+8 <= len(data)-hashSize {
		signature := data[offset : offset+4]
		length := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if bytes.Equal(signature, []byte("link")) {
			return RacyIndexReport{}
		}
		if length < 0 || offset+8+length > len(data) {
			return RacyIndexReport{}
		}
		offset += 8 + length
	}
	report.Known = true
	return report
}

// ErrIndexLocked is a refresh skipped because another process holds the
// checkout's index lock; the caller retries at its next activation.
var ErrIndexLocked = errors.New("gitstate: the checkout's index is locked by another process")

// ErrRefreshYielded is a refresh skipped because an urgent sample (an
// edit's) was waiting for, or holding, the sampler.
var ErrRefreshYielded = errors.New("gitstate: index refresh yielded to an urgent sample")

// UrgentBusy reports whether an urgent sample (an edit's) is waiting for or
// holding this sampler's lease.
func (s *DirtySampler) UrgentBusy() bool {
	return s != nil && (s.urgentWaiting.Load() > 0 || s.urgentActive.Load() > 0)
}

// racyRefreshTimeout bounds one refresh.
const racyRefreshTimeout = 30 * time.Second

// RefreshRacyIndex heals a racily clean index: when RacyIndex reports that a
// refresh would help, it runs `git update-index -q --refresh`, which takes
// the index lock and writes the refreshed stat data back. A locked index is
// not waited for (ErrIndexLocked); the caller retries at its next activation.
// It reports the index before and after, and whether it ran.
//
// It does not take the sampling lease: git writes the refreshed index by
// renaming a new file over the old one under its own lock, so a status (which
// reads the index without writing it) running beside it reads one index or
// the other, never a torn one, and the refresh never makes a sample wait. It
// does not start while an edit's (urgent) sample waits for or holds the
// sampler (ErrRefreshYielded).
func (s *DirtySampler) RefreshRacyIndex(ctx context.Context) (before, after RacyIndexReport, ran bool, err error) {
	before = s.RacyIndex()
	if !before.NeedsRefresh() {
		return before, before, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, racyRefreshTimeout)
	defer cancel()
	if s.UrgentBusy() {
		return before, before, false, ErrRefreshYielded
	}
	gitDir, _, ok := s.gitDirs()
	if ok {
		if _, statErr := os.Lstat(filepath.Join(gitDir, "index.lock")); statErr == nil {
			return before, before, false, ErrIndexLocked
		}
	}
	run := s.refreshRun
	if run == nil {
		run = s.run
	}
	out, runErr := run(ctx, s.root, "update-index", "-q", "--refresh")
	after = s.RacyIndex()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return before, after, true, fmt.Errorf("gitstate: refresh index in %s: %w", s.root, ctxErr)
	}
	switch {
	case runErr == nil:
	case strings.Contains(runErr.Error(), "index.lock") || indexLocked(gitDir, ok):
		// Taken by another process between the check above and the run
		// (git's -q refresh reports it by exit status alone).
		return before, after, true, ErrIndexLocked
	case exitCode(runErr) == 1:
		// update-index exits 1 when some paths need an update (they are
		// modified); the stat data of every other entry was written back.
	default:
		return before, after, true, fmt.Errorf("gitstate: refresh index in %s: %w (%s)", s.root, runErr, bytes.TrimSpace(out))
	}
	return before, after, true, nil
}

func indexLocked(gitDir string, resolved bool) bool {
	if !resolved {
		return false
	}
	_, err := os.Lstat(filepath.Join(gitDir, "index.lock"))
	return err == nil
}
