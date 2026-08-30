package gitstate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSampleHEADAttached(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))

	state, err := SampleHEAD(context.Background(), repo)
	if err != nil {
		t.Fatalf("SampleHEAD: %v", err)
	}
	if state.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q, want refs/heads/main", state.Ref)
	}
	if state.Detached || state.Unborn {
		t.Errorf("unexpected flags: %+v", state)
	}
	if !isOID(state.CommitOID) {
		t.Errorf("CommitOID = %q, want an object id", state.CommitOID)
	}
	if !isOID(state.TreeOID) {
		t.Errorf("TreeOID = %q, want an object id", state.TreeOID)
	}
	if state.CommitOID == state.TreeOID {
		t.Error("commit and tree should be different objects")
	}

	head := git(t, repo, "rev-parse", "HEAD")
	if state.CommitOID != head {
		t.Errorf("CommitOID = %q, want %q", state.CommitOID, head)
	}
}

func TestSampleHEADDetached(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	detached := addWorktree(t, repo, filepath.Join(root, "detached"), "--detach")

	state, err := SampleHEAD(context.Background(), detached)
	if err != nil {
		t.Fatalf("SampleHEAD: %v", err)
	}
	if !state.Detached {
		t.Fatalf("expected Detached, got %+v", state)
	}
	if state.Ref != "" {
		t.Errorf("Ref = %q, want empty for a detached HEAD", state.Ref)
	}
	if state.Unborn {
		t.Error("a detached HEAD at a commit is not unborn")
	}
	if !isOID(state.CommitOID) || !isOID(state.TreeOID) {
		t.Errorf("unresolved objects: %+v", state)
	}
}

func TestSampleHEADUnborn(t *testing.T) {
	root := tempRoot(t)
	repo := initBareRepo(t, filepath.Join(root, "fresh"), false)

	state, err := SampleHEAD(context.Background(), repo)
	if err != nil {
		t.Fatalf("SampleHEAD: %v", err)
	}
	if !state.Unborn {
		t.Fatalf("expected Unborn, got %+v", state)
	}
	if state.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q, want refs/heads/main", state.Ref)
	}
	if state.Detached {
		t.Error("an unborn HEAD is attached to a ref")
	}
	if state.CommitOID != "" || state.TreeOID != "" {
		t.Errorf("unborn HEAD resolved to objects: %+v", state)
	}
}

func TestCanonicalHeadTreeOIDUsesRepositoryObjectFormatWithoutWritingObjects(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{format: "sha1", want: emptyTreeOIDSHA1},
		{format: "sha256", want: emptyTreeOIDSHA256},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			root := tempRoot(t)
			repo := filepath.Join(root, "fresh")
			if out, err := tryGit(t, "", "init", "-q", "--object-format="+tt.format, "-b", "main", "--", repo); err != nil {
				if tt.format == "sha256" {
					t.Skipf("git does not support sha256 repositories: %v\n%s", err, out)
				}
				t.Fatalf("init %s repository: %v\n%s", tt.format, err, out)
			}

			before := objectFileCount(t, filepath.Join(repo, ".git", "objects"))
			got, err := CanonicalHeadTreeOID(context.Background(), repo, "", "")
			if err != nil {
				t.Fatalf("CanonicalHeadTreeOID: %v", err)
			}
			if got != tt.want {
				t.Fatalf("empty tree = %q, want %q", got, tt.want)
			}
			if out, err := tryGit(t, repo, "ls-tree", "-r", "--full-tree", got); err != nil || out != "" {
				t.Fatalf("git did not accept the synthetic empty tree: output=%q err=%v", out, err)
			}
			if after := objectFileCount(t, filepath.Join(repo, ".git", "objects")); after != before {
				t.Fatalf("resolving the canonical empty tree wrote objects: %d -> %d", before, after)
			}

			if err := os.WriteFile(filepath.Join(repo, "first.go"), []byte("package first\n"), 0o644); err != nil {
				t.Fatalf("write first source: %v", err)
			}
			git(t, repo, "add", "first.go")
			git(t, repo, "commit", "-q", "-m", "first")
			firstTree := git(t, repo, "rev-parse", "HEAD^{tree}")
			if diff := git(t, repo, "diff", "--name-status", "--no-renames", got, firstTree); diff != "A\tfirst.go" {
				t.Fatalf("empty-tree -> first-tree diff = %q, want an added first.go", diff)
			}
		})
	}
}

func TestCanonicalHeadTreeOIDPreservesCommittedTreeAndRejectsUnresolvedCommit(t *testing.T) {
	const (
		commit = "1111111111111111111111111111111111111111"
		tree   = "2222222222222222222222222222222222222222"
	)
	got, err := CanonicalHeadTreeOID(context.Background(), "does-not-need-to-exist", commit, tree)
	if err != nil || got != tree {
		t.Fatalf("committed tree = %q, %v; want %q", got, err, tree)
	}
	if _, err := CanonicalHeadTreeOID(context.Background(), "repo", commit, ""); !errors.Is(err, ErrHEADUnavailable) {
		t.Fatalf("unresolved committed tree error = %v, want ErrHEADUnavailable", err)
	}
}

func BenchmarkCanonicalHeadTreeOIDUnborn(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skipf("git not on PATH: %v", err)
	}
	repo := filepath.Join(b.TempDir(), "fresh")
	cmd := exec.Command("git", "init", "-q", "-b", "main", "--", repo)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git init: %v\n%s", err, out)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := CanonicalHeadTreeOID(ctx, repo, "", ""); err != nil {
			b.Fatal(err)
		}
	}
}

func objectFileCount(t *testing.T, root string) int {
	t.Helper()
	count := 0
	if err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk objects: %v", err)
	}
	return count
}

func TestSampleHEADUnavailable(t *testing.T) {
	isolateGit(t)
	dir := realPath(t, t.TempDir())
	if out, err := tryGit(t, dir, "rev-parse", "--git-dir"); err == nil {
		t.Skipf("temp dir is inside a git repository (%s)", out)
	}

	if _, err := SampleHEAD(context.Background(), dir); !errors.Is(err, ErrHEADUnavailable) {
		t.Fatalf("err = %v, want ErrHEADUnavailable", err)
	}
	if _, err := SampleHEAD(context.Background(), ""); !errors.Is(err, ErrHEADUnavailable) {
		t.Fatalf("err on empty dir = %v, want ErrHEADUnavailable", err)
	}
	missing := filepath.Join(dir, "does", "not", "exist")
	if _, err := SampleHEAD(context.Background(), missing); !errors.Is(err, ErrHEADUnavailable) {
		t.Fatalf("err on missing dir = %v, want ErrHEADUnavailable", err)
	}
}

func TestExitCode(t *testing.T) {
	isolateGit(t)
	dir := realPath(t, t.TempDir())
	if out, err := tryGit(t, dir, "rev-parse", "--git-dir"); err == nil {
		t.Skipf("temp dir is inside a git repository (%s)", out)
	}
	// `symbolic-ref -q` exits 1 only for a detached HEAD; outside a
	// repository git fails fatally with a different status.
	_, err := SampleHEAD(context.Background(), dir)
	if err == nil {
		t.Fatal("expected an error outside a repository")
	}
	if code := exitCode(err); code == 1 {
		t.Errorf("exit code %d would be misread as a detached HEAD", code)
	}
	if code := exitCode(errors.New("not a process error")); code != -1 {
		t.Errorf("exitCode of a plain error = %d, want -1", code)
	}
}
