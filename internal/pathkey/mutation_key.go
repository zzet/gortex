package pathkey

import "path/filepath"

// MutationKey returns the one identity under which every writer and reader of
// a source file agrees on that file: the mutation path lock, the pending
// projection of the bytes a tool just wrote, and the watcher events that
// follow the write.
//
// A tool request names a file in the caller's spelling (/tmp/repo/a.go, a
// symlinked workspace, a case variant on a case-insensitive volume) while the
// watcher reports the root's spelling (/private/tmp/repo/a.go). A lexical key
// gives each spelling its own lock, so two edits of one file can interleave
// and one of them is lost. MutationKey resolves symlinks in the parent
// directory, keeping a missing suffix as written, then folds the result like
// IdentityKey. The leaf itself is never followed: writing a symlinked file
// replaces the link, so the link and its target are distinct files.
func MutationKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return IdentityKey(path)
	}
	dir, leaf := filepath.Split(filepath.Clean(abs))
	if leaf == "" {
		return IdentityKey(CanonicalPath(abs))
	}
	return IdentityKey(filepath.Join(CanonicalPath(dir), leaf))
}
