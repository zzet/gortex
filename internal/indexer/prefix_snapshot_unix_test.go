//go:build unix

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	writerDone := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-writerDone:
			return
		default:
		}
		// A missing reader leaves OpenFile blocked. Open a nonblocking read
		// end so the writer can return before the temporary FIFO is removed.
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			defer unix.Close(fd)
		}
		select {
		case <-writerDone:
		case <-time.After(2 * time.Second):
			t.Error("workspace snapshot writer did not stop")
		}
	})
	go func() {
		defer close(writerDone)
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
	prefix, cfg := mi.resolveTrackPrefix(&entry, root, nil, nil, mi.configuredPrefixCheck())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("workspace config reader never opened the FIFO")
	}
	assert.Equal(t, "src", prefix)
	assert.Equal(t, "first", cfg.Workspace)
	assert.Equal(t, "first", cm.GetRepoConfig(prefix).Workspace)
}
