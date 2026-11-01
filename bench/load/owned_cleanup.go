package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// removeOwnedState is called only for the fixed children of an owned run.
// Go module caches make directories read-only; unlinking their files requires
// writable parents. WalkDir never follows symlinks, and a symlink itself is
// left untouched until RemoveAll unlinks it rather than changing its target.
func removeOwnedState(root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()|0o700)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}
