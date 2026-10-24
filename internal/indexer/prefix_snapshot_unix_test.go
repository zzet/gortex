//go:build unix

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"golang.org/x/sys/unix"
)

func TestResolveTrackPrefixUsesOneWorkspaceSnapshot(t *testing.T) {
	root := setupRepoDir(t, "src")
	path := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	next := filepath.Join(root, "next.yaml")
	require.NoError(t, os.WriteFile(next, []byte("workspace: second\n"), 0o644))
	_, mi, cm := newWorktreeTestIndexer(t)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	done := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		if _, err = writer.Write([]byte("workspace: first\n")); err == nil {
			// Replace the pathname before the first reader reaches EOF.
			// Any second read sees a different workspace snapshot.
			err = os.Rename(next, path)
		}
		closeErr := writer.Close()
		if err == nil {
			err = closeErr
		}
		done <- err
	}()
	entry := config.RepoEntry{Path: root}
	prefix, cfg := mi.resolveTrackPrefix(&entry, root, nil, nil)
	require.NoError(t, <-done)
	assert.Equal(t, "src", prefix)
	assert.Equal(t, "first", cfg.Workspace)
	assert.Equal(t, "first", cm.GetRepoConfig(prefix).Workspace)
}
