package indexer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/indexer/source"
)

// maxSourceReadHint bounds the buffer a source-backed read preallocates
// from the metadata size. The size is whatever the snapshot reports, so a
// corrupt or hostile tree must not be able to turn it into an allocation.
const maxSourceReadHint = 64 << 20

// contentSourceRef boxes the installed content source so the swap is one
// atomic pointer store: reindex paths read the source off the hot path
// without taking a lock, the same way they read rootPath.
type contentSourceRef struct{ src source.ContentSource }

// SetContentSource routes every content read this Indexer makes through
// src instead of the os package. Passing nil restores the default, where
// reads go straight to the working tree.
//
// A source is an immutable snapshot of one revision, which is what makes
// it safe to drop the concurrent-write detection the os path needs — see
// readFileWithVersion. Installing a source does not by itself change
// where a walk enumerates from: walkSource does that, and its caller
// picks it explicitly.
func (idx *Indexer) SetContentSource(src source.ContentSource) {
	if src == nil {
		idx.contentSrc.Store(nil)
		return
	}
	idx.contentSrc.Store(&contentSourceRef{src: src})
}

// contentSource returns the installed content source, or nil when reads
// go to the filesystem.
func (idx *Indexer) contentSource() source.ContentSource {
	if ref := idx.contentSrc.Load(); ref != nil {
		return ref.src
	}
	return nil
}

// sourceRelPath maps an absolute path under the repository root to the
// repo-relative, slash-separated key a content source is addressed by.
//
// Unlike relKey it does not NFC-fold. A source answers for the exact byte
// form its snapshot recorded — a git tree serves the name that was
// committed — so folding here would address a path the source does not
// hold.
func (idx *Indexer) sourceRelPath(absPath string) (string, bool) {
	if idx.rootPath == "" || !filepath.IsAbs(absPath) {
		return "", false
	}
	rel, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// readFileWithVersion returns a file's bytes together with the read
// version they came from: through the installed content source when
// there is one, and through the os package otherwise.
//
// The receipt degrades under a source, deliberately. A snapshot cannot be
// rewritten while it is open, so the question the os receipt exists to
// answer — did the bytes change between the stat and the parse? — has one
// answer. The version comes back marked snapshot: valid by construction,
// carrying no os.FileInfo, and accepted by every restat check without a
// syscall.
func (idx *Indexer) readFileWithVersion(absPath string) ([]byte, fileReadVersion, error) {
	src := idx.contentSource()
	if src == nil {
		return readOSFileWithVersion(absPath)
	}
	rel, ok := idx.sourceRelPath(absPath)
	if !ok {
		return nil, fileReadVersion{}, fmt.Errorf(
			"read %q: not under the content source root %q: %w", absPath, idx.rootPath, source.ErrOutsideRoot)
	}
	return readSourceFile(src, rel)
}

// readFileContent returns a file's bytes through the installed content
// source when there is one, and through os.ReadFile otherwise. It is
// readFileWithVersion for the callers that want bytes and no receipt —
// the parse pool, whose staleness bookkeeping is settled by the walk that
// staged the file.
func (idx *Indexer) readFileContent(absPath string) ([]byte, error) {
	if idx.contentSource() == nil {
		return os.ReadFile(absPath)
	}
	src, _, err := idx.readFileWithVersion(absPath)
	return src, err
}

// contentFileVersion reports the version a per-file cache should key
// absPath by, together with whether the content the index reads there is
// there at all.
//
// Under a content source both answers come from the snapshot rather than
// from the working tree, which may sit at an entirely different state. A
// snapshot cannot be rewritten while it is open, so every entry in it
// shares one version: mtimeNano is zero, and a cache keyed by it stays
// coherent for the life of the pass.
//
// The two negative answers are kept apart because callers act on them
// differently. exists is false only when the content is genuinely absent
// — a file a caller may evict; every other failure, including a path the
// source cannot be addressed by, returns ok false with exists true, so an
// unreadable file is never mistaken for a deleted one.
func (idx *Indexer) contentFileVersion(absPath string) (mtimeNano int64, exists, ok bool) {
	if src := idx.contentSource(); src != nil {
		rel, inRoot := idx.sourceRelPath(absPath)
		if !inRoot {
			return 0, true, false
		}
		if _, err := src.Stat(rel); err != nil {
			return 0, !errors.Is(err, source.ErrNotInSource), false
		}
		return 0, true, true
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return 0, !errors.Is(err, os.ErrNotExist), false
	}
	return info.ModTime().UnixNano(), true, true
}

// readSourceFile reads one whole entry out of a content source. A short
// or streamed reader (a git cat-file pipe) is drained to completion, so
// the bytes are the entry's full content or the read fails.
func readSourceFile(src source.ContentSource, rel string) ([]byte, fileReadVersion, error) {
	rc, meta, err := src.Open(rel)
	if err != nil {
		return nil, fileReadVersion{}, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if meta.Size > 0 && meta.Size <= maxSourceReadHint {
		buf.Grow(int(meta.Size))
	}
	if _, err := buf.ReadFrom(rc); err != nil {
		return nil, fileReadVersion{}, err
	}
	b := buf.Bytes()
	return b, fileReadVersion{size: int64(len(b)), valid: true, snapshot: true}, nil
}
