package gitstate

import (
	"context"
	"errors"
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
