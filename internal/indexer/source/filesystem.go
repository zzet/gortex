package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// gitDirName is the one entry a filesystem walk never descends into.
// It is skipped at the root only: a nested "vendor/x/.git" belongs to a
// different repository and is the caller's business, while the root
// ".git" is the object store this source is meant to be an alternative
// to.
const gitDirName = ".git"

// FilesystemSource serves content from a checked-out worktree.
//
// Every file access goes through an os.Root opened on the worktree
// directory, so confinement is structural rather than validated: the
// kernel-level handle refuses to resolve a path that leaves the root,
// including one that leaves through a symlink, whatever this package
// does or forgets to do. Paths that are absolute or climb out with ".."
// are rejected before the syscall as well, so the common cases have a
// decision of our own.
//
// Symlinks are reported, not followed, by Stat and Walk: Symlink is set
// and SymlinkTarget carries the raw target text, exactly as lstat and
// readlink see it. Open is the one place a link is followed, and only
// as far as the root allows — a link pointing outside returns
// ErrOutsideRoot instead of content.
type FilesystemSource struct {
	root *os.Root
	fsys fs.FS
	// path is the cleaned absolute worktree path, used as Identity.
	path string

	closeOnce sync.Once
	closeErr  error
}

// compile-time check that the source satisfies the interface.
var _ ContentSource = (*FilesystemSource)(nil)

// NewFilesystemSource opens root as a content source. The directory
// must exist and be a directory; the returned source holds an open
// handle on it until Close.
func NewFilesystemSource(root string) (*FilesystemSource, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("filesystem source: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("filesystem source: resolve %s: %w", root, err)
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("filesystem source: open root %s: %w", abs, err)
	}
	return &FilesystemSource{root: r, fsys: r.FS(), path: filepath.Clean(abs)}, nil
}

// Identity returns a stable description of the source: the absolute
// worktree path, prefixed so it cannot be confused with a tree source.
func (s *FilesystemSource) Identity() string { return "fs:" + s.path }

// Close releases the root handle. It is idempotent; later calls return
// the first close error.
func (s *FilesystemSource) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.root.Close() })
	return s.closeErr
}

// Stat reports metadata for path with lstat semantics: a symlink is
// described, never followed. Directories and other non-content entries
// (devices, sockets, fifos) are not part of the source namespace and
// return ErrNotInSource.
func (s *FilesystemSource) Stat(p string) (FileMeta, error) {
	rel, err := normalizePath(p)
	if err != nil {
		return FileMeta{}, err
	}
	return s.stat(rel)
}

// Open returns a reader over the file's content plus the same metadata
// Stat reports. For a symlink the metadata still describes the link
// (Symlink is set, Size is the length of the target text) while the
// reader yields the target's content — but only when the target stays
// inside the root; otherwise the result is ErrOutsideRoot and no bytes.
// Opening a directory returns ErrNotInSource.
func (s *FilesystemSource) Open(p string) (io.ReadCloser, FileMeta, error) {
	rel, err := normalizePath(p)
	if err != nil {
		return nil, FileMeta{}, err
	}
	meta, err := s.stat(rel)
	if err != nil {
		return nil, FileMeta{}, err
	}
	if meta.Symlink && linkEscapes(rel, meta.SymlinkTarget) {
		return nil, FileMeta{}, fmt.Errorf("%s: symlink target %q: %w", rel, meta.SymlinkTarget, ErrOutsideRoot)
	}
	f, err := s.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, FileMeta{}, mapRootError(rel, err)
	}
	if meta.Symlink {
		// The link resolved to something; a directory is not content,
		// and the caller should learn that here rather than from a read
		// error later. The check is on the open descriptor, so it
		// describes what was actually opened.
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			_ = f.Close()
			if err != nil {
				return nil, FileMeta{}, mapRootError(rel, err)
			}
			return nil, FileMeta{}, fmt.Errorf("%s: symlink target is not a file: %w", rel, ErrNotInSource)
		}
	}
	return f, meta, nil
}

// Walk visits every regular file and symlink under the root in
// lexicographic order of the full slash-separated path, skipping the
// root ".git". Directory symlinks are reported as symlinks and not
// descended into, so the walk cannot loop or leave the root. Entries
// that vanish mid-walk are skipped rather than failing the walk.
func (s *FilesystemSource) Walk(ctx context.Context, fn func(FileMeta) error) error {
	if fn == nil {
		return errors.New("filesystem source: nil walk function")
	}
	return s.walkDir(ctx, ".", fn)
}

// walkDir emits dir's entries in the package's canonical order and
// recurses into subdirectories at the position their name takes in that
// order.
//
// The ordering trick: a directory sorts under its name plus "/", which
// is exactly the character that follows it in any full path below it.
// Sorting a directory's entries by that key and recursing in place
// yields the same sequence as sorting every full path in the tree, one
// directory read at a time and with no global buffer.
func (s *FilesystemSource) walkDir(ctx context.Context, dir string, fn func(FileMeta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := fs.ReadDir(s.fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return mapRootError(dir, err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(walkKey(a), walkKey(b))
	})
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dir == "." && e.Name() == gitDirName {
			continue
		}
		rel := e.Name()
		if dir != "." {
			rel = dir + "/" + e.Name()
		}
		if e.IsDir() {
			if err := s.walkDir(ctx, rel, fn); err != nil {
				return err
			}
			continue
		}
		meta, err := s.stat(rel)
		if err != nil {
			// A path that is not content (device, socket) or that
			// disappeared between the directory read and the lstat is
			// not a walk failure.
			if errors.Is(err, ErrNotInSource) {
				continue
			}
			return err
		}
		if err := fn(meta); err != nil {
			return err
		}
	}
	return nil
}

// walkKey is the sort key that makes a directory sort where its
// children's paths sort: "src" as "src/", so "src/a" lands after
// "src.go" exactly as full-path ordering would put it.
func walkKey(e fs.DirEntry) string {
	if e.IsDir() {
		return e.Name() + "/"
	}
	return e.Name()
}

// stat is the lstat-based metadata lookup shared by Stat, Open and
// Walk. rel must already be normalized.
func (s *FilesystemSource) stat(rel string) (FileMeta, error) {
	fi, err := s.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return FileMeta{}, mapRootError(rel, err)
	}
	mode := fi.Mode()
	meta := FileMeta{Path: rel, Size: fi.Size(), Mode: mode}
	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := s.root.Readlink(filepath.FromSlash(rel))
		if err != nil {
			return FileMeta{}, mapRootError(rel, err)
		}
		meta.Symlink = true
		meta.SymlinkTarget = filepath.ToSlash(target)
	case mode.IsRegular():
	default:
		return FileMeta{}, fmt.Errorf("%s: not a file: %w", rel, ErrNotInSource)
	}
	return meta, nil
}

// pathEscapesText is the message os.Root attaches to a path that would
// leave the root. The os package keeps the sentinel unexported and
// wraps it in a *fs.PathError, so matching the text is the only way to
// recognise the escapes the kernel-level check caught for us — the
// escapes this package can decide by itself are decided before the
// syscall, in normalizePath and linkEscapes.
const pathEscapesText = "path escapes from parent"

// mapRootError translates an os.Root failure into this package's
// sentinels: a missing path (or a component that is not a directory)
// into ErrNotInSource, a refused escape into ErrOutsideRoot, anything
// else wrapped with the path for context.
func mapRootError(rel string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("%s: %w", rel, ErrNotInSource)
	case strings.Contains(err.Error(), pathEscapesText):
		return fmt.Errorf("%s: %w", rel, ErrOutsideRoot)
	default:
		return fmt.Errorf("%s: %w", rel, err)
	}
}
