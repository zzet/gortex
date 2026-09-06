package gitstate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func familyDirsGit(t testing.TB, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "commit.gpgsign=false", "-c", "core.hooksPath="}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func familyDirsFixture(t testing.TB) (primary, linked string) {
	t.Helper()
	parent := t.TempDir()
	primary, linked = filepath.Join(parent, "primary root"), filepath.Join(parent, "linked root")
	familyDirsGit(t, parent, "init", primary)
	familyDirsGit(t, primary, "config", "user.name", "Gortex Test")
	familyDirsGit(t, primary, "config", "user.email", "test@gortex.invalid")
	familyDirsGit(t, primary, "commit", "--allow-empty", "-m", "baseline")
	familyDirsGit(t, primary, "worktree", "add", "--detach", linked, "HEAD")
	return primary, linked
}

func familyDirsSameDirectory(t testing.TB, actual, expected string) {
	t.Helper()
	if !filepath.IsAbs(actual) {
		t.Fatalf("Git directory is not absolute: %q", actual)
	}
	a, err := os.Stat(actual)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsDir() || !b.IsDir() || !os.SameFile(a, b) {
		t.Fatalf("Git directory %q does not identify %q", actual, expected)
	}
}

func TestResolveFamilyDirsMatchesMainLinkedAndSubdirectory(t *testing.T) {
	primary, linked := familyDirsFixture(t)
	// CI may keep the repository on D: and temporary directories on C:.
	// Stay on the fixture's volume so relative-path coverage runs on Windows.
	t.Chdir(filepath.Dir(primary))
	for _, root := range []string{primary, linked} {
		inv, err := Inventory(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		subdir := filepath.Join(root, "nested", "source")
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			t.Fatal(err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(cwd, subdir)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{root, subdir, relative} {
			gitDir, commonDir, err := ResolveFamilyDirs(t.Context(), path)
			if err != nil {
				t.Fatalf("resolve %q: %v", path, err)
			}
			familyDirsSameDirectory(t, gitDir, inv.GitDir)
			familyDirsSameDirectory(t, commonDir, filepath.Join(primary, ".git"))
		}
	}
}

func TestResolveFamilyDirsUnavailableHasNoBinding(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"", root, filepath.Join(root, "missing")} {
		gitDir, commonDir, err := ResolveFamilyDirs(t.Context(), path)
		if !errors.Is(err, ErrInventoryUnavailable) || gitDir != "" || commonDir != "" {
			t.Fatalf("unavailable path %q returned %q, %q, %v", path, gitDir, commonDir, err)
		}
	}
}

func TestResolveFamilyDirsPreservesContextFailure(t *testing.T) {
	root := t.TempDir()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{canceled, expired} {
		gitDir, commonDir, err := ResolveFamilyDirs(ctx, root)
		if !errors.Is(err, ctx.Err()) || !errors.Is(err, ErrInventoryUnavailable) || gitDir != "" || commonDir != "" {
			t.Fatalf("context failure returned %q, %q, %v", gitDir, commonDir, err)
		}
	}
}

func BenchmarkResolveFamilyDirs(b *testing.B) {
	_, linked := familyDirsFixture(b)
	for _, mode := range []string{"inventory", "directories_only"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if mode == "inventory" {
					if _, err := Inventory(b.Context(), linked); err != nil {
						b.Fatal(err)
					}
				} else if _, _, err := ResolveFamilyDirs(b.Context(), linked); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
