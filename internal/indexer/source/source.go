// Package source hands the indexer file content without caring where
// the bytes live. A checked-out worktree and a committed git tree are
// both content sources: one reads through the filesystem, the other
// reads blobs straight out of the object database, and both present the
// same repo-relative, slash-separated namespace of files and symlinks.
//
// The namespace is deliberately narrow. A source enumerates and serves
// regular files and symlinks only — directories, devices, sockets and
// git submodule pointers are not part of it, because a committed tree
// cannot serve them and a caller that switched sources would otherwise
// change behaviour. Nothing here filters paths by ignore rules or size:
// admission is the caller's decision, and a source answers for every
// path it holds.
//
// Three sentinel errors carry the outcomes a caller must distinguish:
// ErrNotInSource (the source simply has no such path), ErrObjectMissing
// (git knows the object but the local repository no longer holds its
// bytes — a capability loss, never an empty file) and ErrOutsideRoot (a
// path or symlink that tried to leave the source root).
package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// Sentinel errors returned by every ContentSource implementation.
var (
	// ErrNotInSource reports that the source holds no such path. It is
	// the ordinary "not found": the file was never committed, was
	// deleted, or names a directory rather than content.
	ErrNotInSource = errors.New("path is not in the content source")

	// ErrObjectMissing reports that the source knows about the object
	// but the local repository cannot produce its bytes — pruned,
	// corrupted, or absent from a partial clone that is not allowed to
	// fetch. Callers must treat it as a capability loss and never as an
	// empty file: indexing a pruned blob as "" would silently erase
	// real content from the graph.
	ErrObjectMissing = errors.New("git object is missing from the local repository")

	// ErrOutsideRoot reports a path that resolves outside the source
	// root: an absolute path, one that climbs out with "..", or a
	// symlink whose target leaves the root.
	ErrOutsideRoot = errors.New("path resolves outside the source root")
)

// FileMeta describes one entry of a content source.
//
// Path is repo-relative and slash-separated, cleaned of "." and ".."
// components, and never starts with a slash. Size is the content length
// in bytes — for a symlink, the length of its target text — and is
// negative only when the source cannot determine it (a git tree whose
// blob is missing locally).
//
// Mode carries the permission bits plus fs.ModeSymlink for a link.
// Permission bits are exact for regular files (a git tree reports 0644
// or 0755, the two modes git records); for symlinks only the
// fs.ModeSymlink bit is meaningful, since the permission bits of a link
// differ by platform and are ignored by git. Symlink is the flag to
// branch on, and SymlinkTarget is the raw, unresolved target text.
type FileMeta struct {
	Path          string
	Size          int64
	Mode          fs.FileMode
	Symlink       bool
	SymlinkTarget string
}

// ContentSource serves file content and metadata for one snapshot of a
// repository — a checkout, a committed tree, or a composition of both.
//
// Implementations are safe for concurrent use by multiple goroutines.
// Open returns a reader the caller must close; the FileMeta it returns
// equals what Stat reports for the same path, so a caller never needs a
// second lookup. Walk visits every entry in one deterministic order:
// lexicographic byte order of the full slash-separated path, identical
// across implementations so two sources can be merged by path. Walk
// stops and returns the error when fn returns one, and it honours ctx
// cancellation. Close releases whatever the source holds — a directory
// handle, a git child process — and is safe to call more than once.
type ContentSource interface {
	Open(path string) (io.ReadCloser, FileMeta, error)
	Stat(path string) (FileMeta, error)
	Walk(ctx context.Context, fn func(FileMeta) error) error
	Identity() string
	Close() error
}

// normalizePath turns a caller-supplied path into the canonical
// repo-relative form used as a key everywhere in this package, or
// returns why it cannot be one.
//
// Absolute paths (including a Windows volume-relative "C:x") and paths
// that climb out of the root with ".." are ErrOutsideRoot: they name
// something the source cannot own. The root itself (".", "") and paths
// carrying a NUL byte are ErrNotInSource — they are not content.
//
// This is a lexical check only. It rejects the paths that are provably
// outside the root before any syscall; the filesystem source still
// depends on os.Root to stop what only the kernel can see, such as a
// symlink chain that leaves the root.
func normalizePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path: %w", ErrNotInSource)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("path contains NUL: %w", ErrNotInSource)
	}
	slashed := filepath.ToSlash(p)
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("%s: absolute path: %w", p, ErrOutsideRoot)
	}
	clean := path.Clean(slashed)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s: climbs above the root: %w", p, ErrOutsideRoot)
	}
	if clean == "." {
		return "", fmt.Errorf("%s: not a file: %w", p, ErrNotInSource)
	}
	return clean, nil
}

// linkEscapes reports whether the raw target of the symlink at rel
// leaves the source root on its own: an absolute target, or a relative
// one with more ".." steps than the link's directory has depth.
//
// It catches the last hop only. A chain of links that stays lexically
// inside the root at every step but escapes through a directory symlink
// is caught by os.Root when the file is actually opened; this check
// exists so the common case has a decision of our own rather than one
// read out of an operating-system error message.
func linkEscapes(rel, target string) bool {
	if target == "" {
		return false
	}
	slashed := filepath.ToSlash(target)
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return true
	}
	joined := path.Join(path.Dir(rel), slashed)
	return joined == ".." || strings.HasPrefix(joined, "../")
}
