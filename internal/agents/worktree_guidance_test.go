package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFamilyRepoRootsAndPendingText(t *testing.T) {
	registered := []string{"/registered", "/shared"}
	configured := []string{"/unloaded", "/shared", " "}
	got := FamilyRepoRoots(registered, configured)
	want := []string{"/registered", "/shared", "/unloaded"}
	if !reflect.DeepEqual(got, want) || registered[0] != "/registered" || configured[0] != "/unloaded" {
		t.Fatalf("root union changed inputs or lost declaration: %v", got)
	}
	text := strings.ReplaceAll(PendingAutomaticCheckoutGuidance, "no reconnect is needed", "")
	if strings.Contains(strings.ToLower(text), "reconnect") || !strings.Contains(text, "view_building") || strings.Contains(text, "repo_not_tracked") {
		t.Fatalf("pending guidance gave a reconnect/untracked remedy: %s", text)
	}
}

func TestNearestGitRootAbstainsAtUnknownNestedBoundary(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	var inspected []string
	got := nearestGitRootWithLstat(nested, func(path string) (os.FileInfo, error) {
		inspected = append(inspected, path)
		if path == filepath.Join(nested, ".git") {
			return nil, &os.PathError{Op: "lstat", Path: path, Err: os.ErrPermission}
		}
		return os.Lstat(path)
	})
	if got != "" || len(inspected) != 1 {
		t.Fatalf("unknown nested boundary fell through to known ancestor: root=%q probes=%v", got, inspected)
	}
	if got := nearestGitRoot(nested); got != root {
		t.Fatalf("a genuinely absent nested .git should still find ancestor: %q", got)
	}
}

func worktreeGuidanceGit(t testing.TB, dir string, args ...string) {
	t.Helper()
	gitArgs := append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func BenchmarkPendingAutomaticCheckout(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git not available")
	}
	main := filepath.Join(b.TempDir(), "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		b.Fatal(err)
	}
	worktreeGuidanceGit(b, main, "init", "-q", "-b", "main")
	worktreeGuidanceGit(b, main, "config", "user.email", "test@example.com")
	worktreeGuidanceGit(b, main, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(main, "main.go"), []byte("package main\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	worktreeGuidanceGit(b, main, "add", ".")
	worktreeGuidanceGit(b, main, "commit", "-q", "-m", "initial")

	linked := filepath.Join(b.TempDir(), "linked")
	worktreeGuidanceGit(b, main, "worktree", "add", "-q", "-b", "feature", linked)
	linkedNested := filepath.Join(linked, "internal", "pkg")
	if err := os.MkdirAll(linkedNested, 0o755); err != nil {
		b.Fatal(err)
	}

	unrelated := filepath.Join(b.TempDir(), "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		b.Fatal(err)
	}
	worktreeGuidanceGit(b, unrelated, "init", "-q")
	unrelatedNested := filepath.Join(unrelated, "nested")
	if err := os.MkdirAll(unrelatedNested, 0o755); err != nil {
		b.Fatal(err)
	}

	for _, count := range []int{1, 32, 256} {
		roots := make([]string, count)
		for i := range roots {
			roots[i] = unrelated
		}
		matching := append([]string(nil), roots...)
		matching[len(matching)-1] = main
		noMatch := make([]string, count)
		for i := range noMatch {
			noMatch[i] = main
		}

		b.Run(fmt.Sprintf("match_last/tracked_roots_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !PendingAutomaticCheckout(linkedNested, matching) {
					b.Fatal("same-family checkout was not recognized")
				}
			}
		})
		b.Run(fmt.Sprintf("unrelated/tracked_roots_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if PendingAutomaticCheckout(unrelatedNested, noMatch) {
					b.Fatal("unrelated checkout was recognized")
				}
			}
		})
	}
}

func TestPendingAutomaticCheckoutMatchesBothFamilyDirections(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeGuidanceGit(t, main, "init", "-q", "-b", "main")
	worktreeGuidanceGit(t, main, "config", "user.email", "test@example.com")
	worktreeGuidanceGit(t, main, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(main, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreeGuidanceGit(t, main, "add", ".")
	worktreeGuidanceGit(t, main, "commit", "-q", "-m", "initial")

	linked := filepath.Join(t.TempDir(), "linked")
	worktreeGuidanceGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	nested := filepath.Join(linked, "internal", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	if !PendingAutomaticCheckout(nested, []string{main}) {
		t.Fatal("linked worktree of a tracked family was not recognized as pending automatic discovery")
	}
	checkout, represented := AutomaticCheckoutFamily(nested, []string{"", main})
	if checkout != linked || represented != main {
		t.Fatalf("family lookup lost checkout or configured representative: %q %q", checkout, represented)
	}
	if !PendingAutomaticCheckout(main, []string{linked}) {
		t.Fatal("primary checkout was not recognized when a linked checkout is the tracked family member")
	}

	unrelated := filepath.Join(t.TempDir(), "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeGuidanceGit(t, unrelated, "init", "-q")
	if PendingAutomaticCheckout(unrelated, []string{main}) {
		t.Fatal("an unrelated repository was mistaken for an automatic worktree")
	}
}
