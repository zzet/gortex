package indexer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Choosing likely files is observation: even stat metadata that an ordinary
// status would refresh must not take the index lock or rewrite the index.
func TestLikelyEditedFilesDoesNotRefreshTheGitIndex(t *testing.T) {
	builderIsolateGit(t)
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	repo := builderTempDir(t, "prewarm-git")
	builderGit(t, repo, "init", "-q")
	builderGit(t, repo, "config", "core.fsmonitor", "false")
	builderGit(t, repo, "config", "core.hooksPath", os.DevNull)
	path := filepath.Join(repo, "tracked.go")
	content := []byte("package fixture\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	builderGit(t, repo, "add", "tracked.go")
	builderGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "tracked fixture")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(repo, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	files := likelyEditedFiles(t.Context(), repo)
	if len(files) != 1 || files[0] != "tracked.go" {
		t.Fatalf("likely files = %v, want tracked.go from committed history", files)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only prewarm status refreshed the Git index")
	}
	if status := builderGit(t, repo, "status", "--porcelain", "--untracked-files=no"); status != "" {
		t.Fatalf("unchanged fixture reported dirty: %q", status)
	}
	refreshed, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, refreshed) {
		t.Fatal("fixture did not witness an ordinary status refreshing index metadata")
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, actual) {
		t.Fatal("Git observation changed source bytes")
	}
}
