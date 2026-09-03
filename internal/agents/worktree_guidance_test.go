package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func worktreeGuidanceGit(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
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
