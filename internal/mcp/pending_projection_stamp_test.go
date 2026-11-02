package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiskStampKeepsIdentityBeforeReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.go")
	const content = "package example\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	before, err := statDiskStamp(path)
	require.NoError(t, err)
	// Allocate the replacement before unlinking the original so its file ID
	// cannot be a reuse of the original's ID. Do not compare before the rename:
	// that would accidentally initialize a lazy Windows FileInfo identity.
	replacement := path + ".tmp"
	require.NoError(t, os.WriteFile(replacement, []byte(content), 0o600))
	mtime := before.info.ModTime()
	require.NoError(t, os.Chtimes(replacement, mtime, mtime))
	require.NoError(t, os.Rename(replacement, path))
	after, err := statDiskStamp(path)
	require.NoError(t, err)
	require.Equal(t, before.info.Size(), after.info.Size())
	require.True(t, before.info.ModTime().Equal(after.info.ModTime()))
	require.False(t, before.matches(after), "replacement must invalidate a stamp even before its first comparison")
}

func TestDiskStampKeepsIdentityAfterOriginalPathDisappears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.go")
	alias := path + ".link"
	require.NoError(t, os.WriteFile(path, []byte("package example\n"), 0o600))
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	before, err := statDiskStamp(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	after, err := statDiskStamp(alias)
	require.NoError(t, err)
	require.True(t, before.matches(after), "identity belongs to the stamped object, not a later lookup of its old path")
}

func TestDiskStampPreservesLeafType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.go")
	require.NoError(t, os.WriteFile(path, []byte("package example\n"), 0o600))
	for _, path := range []string{path, dir} {
		stamp, err := statDiskStamp(path)
		require.NoError(t, err)
		info, err := os.Lstat(path)
		require.NoError(t, err)
		require.Equal(t, info.Mode().Type(), stamp.info.Mode().Type())
	}
	link := filepath.Join(dir, "symlink.go")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	stamp, err := statDiskStamp(link)
	require.NoError(t, err)
	require.NotZero(t, stamp.info.Mode()&os.ModeSymlink, "a disk stamp must not follow the leaf")
	missing, err := statDiskStamp(filepath.Join(dir, "absent.go"))
	require.NoError(t, err)
	require.Nil(t, missing.info)
}
