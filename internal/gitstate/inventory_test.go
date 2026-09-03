package gitstate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryMainOnlyRepo(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))

	inv, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(inv.Records))
	}
	wantGitDir := filepath.Join(repo, ".git")
	if inv.GitDir != wantGitDir {
		t.Errorf("GitDir = %q, want %q", inv.GitDir, wantGitDir)
	}
	if inv.CommonDir != wantGitDir {
		t.Errorf("CommonDir = %q, want %q", inv.CommonDir, wantGitDir)
	}

	r := inv.Records[0]
	if !samePath(r.Path, repo) {
		t.Errorf("Path = %q, want %q", r.Path, repo)
	}
	if !r.IsMain {
		t.Error("expected the first record to be the main worktree")
	}
	if r.AdminName != MainAdminName {
		t.Errorf("AdminName = %q, want %q", r.AdminName, MainAdminName)
	}
	if r.Branch != "main" {
		t.Errorf("Branch = %q, want %q", r.Branch, "main")
	}
	if r.HEADRef != "refs/heads/main" {
		t.Errorf("HEADRef = %q, want refs/heads/main", r.HEADRef)
	}
	if !isOID(r.HEADOID) {
		t.Errorf("HEADOID = %q, want an object id", r.HEADOID)
	}
	if r.Detached || r.Unborn || r.Bare || r.Locked || r.Prunable {
		t.Errorf("unexpected flags on a plain checkout: %+v", r)
	}
	if r.LockReason != "" || r.PruneReason != "" {
		t.Errorf("unexpected reasons: lock=%q prune=%q", r.LockReason, r.PruneReason)
	}
	if !r.RootAccessible || r.RootErr != nil {
		t.Errorf("RootAccessible = %v, RootErr = %v, want true/nil", r.RootAccessible, r.RootErr)
	}
}

func TestInventoryLinkedWorktreesWithAwkwardPaths(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))

	spaced := addWorktree(t, repo, filepath.Join(root, "wt with space"), "-b", "feature/foo")

	// A newline in a directory name is what makes the NUL-delimited
	// format necessary. Some filesystems reject it; the rest of the
	// test still runs when they do.
	newlined := filepath.Join(root, "wt\nnewline")
	_, nlErr := tryGit(t, repo, "worktree", "add", "-q", "--detach", "--", newlined)

	inv, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if !inv.Records[0].IsMain || !samePath(inv.Records[0].Path, repo) {
		t.Fatalf("first record is not the main worktree: %+v", inv.Records[0])
	}
	for _, r := range inv.Records[1:] {
		if r.IsMain {
			t.Errorf("record %q marked as main", r.Path)
		}
	}

	sp := recordFor(t, inv, spaced)
	if sp.Branch != "feature/foo" || sp.HEADRef != "refs/heads/feature/foo" {
		t.Errorf("spaced worktree branch = %q / %q, want feature/foo", sp.Branch, sp.HEADRef)
	}
	if sp.AdminName == "" || sp.AdminName == MainAdminName {
		t.Fatalf("spaced worktree AdminName = %q, want a linked admin name", sp.AdminName)
	}
	// git sanitizes the admin directory name, so it is not simply the
	// basename of the worktree path.
	if sp.AdminName == filepath.Base(spaced) {
		t.Errorf("AdminName %q should be the sanitized admin directory, not the path basename", sp.AdminName)
	}
	adminDir := filepath.Join(inv.CommonDir, "worktrees", sp.AdminName)
	if _, statErr := os.Stat(adminDir); statErr != nil {
		t.Errorf("admin directory %q does not exist: %v", adminDir, statErr)
	}
	if !sp.RootAccessible {
		t.Errorf("spaced worktree root should be accessible, RootErr = %v", sp.RootErr)
	}

	t.Run("newline in path", func(t *testing.T) {
		if nlErr != nil {
			t.Skipf("filesystem rejects a newline in a directory name: %v", nlErr)
		}
		nl := recordFor(t, inv, newlined)
		if !strings.Contains(nl.Path, "\n") {
			t.Fatalf("path %q lost its newline", nl.Path)
		}
		if !nl.Detached {
			t.Errorf("expected the newline worktree to be detached: %+v", nl)
		}
		if nl.AdminName == "" {
			t.Error("newline worktree has no AdminName")
		}
	})
}

func TestInventoryLockedWorktree(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	locked := addWorktree(t, repo, filepath.Join(root, "locked"), "--detach")
	git(t, repo, "worktree", "lock", "--reason", "held by tests", "--", locked)

	inv, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	r := recordFor(t, inv, locked)
	if !r.Locked {
		t.Fatalf("expected Locked, got %+v", r)
	}
	if r.LockReason != "held by tests" {
		t.Errorf("LockReason = %q, want %q", r.LockReason, "held by tests")
	}
	if main := inv.Records[0]; main.Locked {
		t.Errorf("main worktree should not be locked: %+v", main)
	}
}

func TestInventoryMissingRootSurvivesThenPruneRemovesIt(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	gone := addWorktree(t, repo, filepath.Join(root, "gone"), "--detach")

	// Delete the checkout behind git's back. `git worktree remove`
	// would also drop the administrative record, which is not the case
	// under test.
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}

	inv, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	r := recordFor(t, inv, gone)
	if r.RootAccessible {
		t.Errorf("expected RootAccessible=false for a deleted checkout")
	}
	if !errors.Is(r.RootErr, fs.ErrNotExist) {
		t.Errorf("RootErr = %v, want a not-exist error", r.RootErr)
	}
	if r.Prunable && r.PruneReason == "" {
		t.Error("a prunable record should carry git's reason")
	}
	// The administrative directory outlives the checkout, so the admin
	// name is still recoverable without the root.
	if r.AdminName == "" {
		t.Error("AdminName should be recoverable from the administrative directory")
	}
	if _, statErr := os.Stat(filepath.Join(inv.CommonDir, "worktrees", r.AdminName)); statErr != nil {
		t.Errorf("admin directory for %q missing: %v", r.AdminName, statErr)
	}

	git(t, repo, "worktree", "prune")

	after, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory after prune: %v", err)
	}
	if hasRecord(after, gone) {
		t.Errorf("pruned worktree still listed: %+v", after.Records)
	}
	if !hasRecord(after, repo) {
		t.Errorf("main worktree disappeared after prune: %+v", after.Records)
	}
}

func TestInventoryDetachedHEAD(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	detached := addWorktree(t, repo, filepath.Join(root, "detached"), "--detach")

	inv, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	r := recordFor(t, inv, detached)
	if !r.Detached {
		t.Fatalf("expected Detached, got %+v", r)
	}
	if r.Branch != "" || r.HEADRef != "" {
		t.Errorf("detached worktree should carry no branch, got %q / %q", r.Branch, r.HEADRef)
	}
	if !isOID(r.HEADOID) {
		t.Errorf("HEADOID = %q, want an object id", r.HEADOID)
	}
	if r.Unborn {
		t.Error("a detached worktree at a commit is not unborn")
	}
}

func TestInventoryUnbornHEAD(t *testing.T) {
	root := tempRoot(t)
	repo := initBareRepo(t, filepath.Join(root, "fresh"), false)

	inv, err := Inventory(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(inv.Records))
	}
	r := inv.Records[0]
	if !r.Unborn {
		t.Fatalf("expected Unborn, got %+v", r)
	}
	if r.HEADOID != "" {
		t.Errorf("HEADOID = %q, want empty for an unborn branch", r.HEADOID)
	}
	if r.Branch != "main" || r.HEADRef != "refs/heads/main" {
		t.Errorf("branch = %q / %q, want main", r.Branch, r.HEADRef)
	}
	if r.Detached || r.Bare {
		t.Errorf("unexpected flags: %+v", r)
	}
}

func TestInventoryBareRepository(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	bare := filepath.Join(root, "bare.git")
	git(t, "", "clone", "-q", "--bare", "--", repo, bare)

	inv, err := Inventory(context.Background(), bare)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(inv.Records))
	}
	if inv.CommonDir != bare || inv.GitDir != bare {
		t.Errorf("dirs = %q / %q, want %q", inv.GitDir, inv.CommonDir, bare)
	}
	r := inv.Records[0]
	if !r.Bare {
		t.Fatalf("expected Bare, got %+v", r)
	}
	if !r.IsMain || r.AdminName != MainAdminName {
		t.Errorf("bare entry should be the main record: %+v", r)
	}
	if r.Branch != "" || r.HEADOID != "" {
		t.Errorf("bare entry should carry no branch or HEAD: %+v", r)
	}
	if r.Unborn {
		t.Error("a bare repository is not an unborn worktree")
	}
}

func TestInventoryFromLinkedWorktreeSeesWholeFamily(t *testing.T) {
	root := tempRoot(t)
	repo := initRepo(t, filepath.Join(root, "repo"))
	linked := addWorktree(t, repo, filepath.Join(root, "linked"), "-b", "topic/one")

	inv, err := Inventory(context.Background(), linked)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(inv.Records))
	}
	if !inv.Records[0].IsMain || !samePath(inv.Records[0].Path, repo) {
		t.Errorf("main record = %+v, want %q", inv.Records[0], repo)
	}
	if inv.CommonDir != filepath.Join(repo, ".git") {
		t.Errorf("CommonDir = %q, want the main checkout's .git", inv.CommonDir)
	}
	self := recordFor(t, inv, linked)
	if inv.GitDir != filepath.Join(inv.CommonDir, "worktrees", self.AdminName) {
		t.Errorf("GitDir = %q, want the linked admin directory for %q", inv.GitDir, self.AdminName)
	}
}

func TestInventoryUnavailableOutsideRepository(t *testing.T) {
	isolateGit(t)
	dir := realPath(t, t.TempDir())
	if out, err := tryGit(t, dir, "rev-parse", "--git-dir"); err == nil {
		t.Skipf("temp dir is inside a git repository (%s)", out)
	}

	inv, err := Inventory(context.Background(), dir)
	if inv != nil {
		t.Errorf("expected a nil inventory, got %+v", inv)
	}
	if !errors.Is(err, ErrInventoryUnavailable) {
		t.Fatalf("err = %v, want ErrInventoryUnavailable", err)
	}
}

func TestInventoryUnavailableOnEmptyDirectory(t *testing.T) {
	inv, err := Inventory(context.Background(), "")
	if inv != nil {
		t.Errorf("expected a nil inventory, got %+v", inv)
	}
	if !errors.Is(err, ErrInventoryUnavailable) {
		t.Fatalf("err = %v, want ErrInventoryUnavailable", err)
	}
}
