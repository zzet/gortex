package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitcmd"
)

func TestRepoHeadAndDirtyDoesNotRefreshLinkedWorktreeIndex(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "main")
	worktreeRoot := filepath.Join(parent, "worktree")
	require.NoError(t, os.Mkdir(mainRoot, 0o755))
	runGit(t, mainRoot, "init", "-q", "-b", "main")
	runGit(t, mainRoot, "config", "user.email", "test@example.invalid")
	runGit(t, mainRoot, "config", "user.name", "Test")
	runGit(t, mainRoot, "config", "commit.gpgsign", "false")

	trackedPath := filepath.Join(mainRoot, "tracked.go")
	require.NoError(t, os.WriteFile(trackedPath, []byte("package fixture\n"), 0o644))
	runGit(t, mainRoot, "add", "tracked.go")
	runGit(t, mainRoot, "commit", "-q", "-m", "initial")
	runGit(t, mainRoot, "worktree", "add", "-q", "-b", "linked", worktreeRoot)

	ctx := context.Background()
	wantHead, err := gitcmd.Output(ctx, worktreeRoot, "rev-parse", "HEAD")
	require.NoError(t, err)
	gitDir, err := gitcmd.Output(ctx, worktreeRoot, "rev-parse", "--absolute-git-dir")
	require.NoError(t, err)
	indexPath := filepath.Join(gitDir, "index")
	indexBefore, err := os.ReadFile(indexPath)
	require.NoError(t, err)

	// Changing only the working file's timestamp makes ordinary git status
	// refresh the cached stat entry in the linked worktree's private index.
	// Gortex must still report the tree clean without performing that write.
	worktreeFile := filepath.Join(worktreeRoot, "tracked.go")
	future := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(worktreeFile, future, future))

	gotHead, dirty := repoHeadAndDirty(worktreeRoot)
	require.Equal(t, wantHead, gotHead)
	require.False(t, dirty, "an mtime-only change must remain clean")
	requireIndexBytesEqual(t, indexPath, indexBefore)

	// A real content change must be observed without turning the read-only
	// freshness probe into an index writer either.
	require.NoError(t, os.WriteFile(worktreeFile, []byte("package fixture\n\nfunc Changed() {}\n"), 0o644))
	gotHead, dirty = repoHeadAndDirty(worktreeRoot)
	require.Equal(t, wantHead, gotHead)
	require.True(t, dirty, "a content change must be reported dirty")
	requireIndexBytesEqual(t, indexPath, indexBefore)
}

func requireIndexBytesEqual(t *testing.T, indexPath string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	require.Truef(t, bytes.Equal(want, got),
		"read-only Git probe rewrote %s: before_sha256=%s after_sha256=%s",
		indexPath, sha256String(want), sha256String(got))
}

func sha256String(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}
