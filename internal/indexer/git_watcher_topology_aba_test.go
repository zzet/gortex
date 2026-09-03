package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTopologyIdentityFile(t testing.TB, path, contents string, modified time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

func worktreeHeadIdentity(t testing.TB, adminDir, commonDir string) string {
	t.Helper()
	var builder strings.Builder
	if err := appendWorktreeHeadIdentity(&builder, adminDir, commonDir); err != nil {
		t.Fatalf("appendWorktreeHeadIdentity: %v", err)
	}
	return builder.String()
}

func TestWorktreeHeadIdentityDetectsHeadABA(t *testing.T) {
	commonDir := t.TempDir()
	adminDir := filepath.Join(commonDir, "worktrees", "linked")
	headPath := filepath.Join(adminDir, "HEAD")
	branchAPath := filepath.Join(commonDir, "refs", "heads", "branch-a")
	firstRefPath := filepath.Join(commonDir, "refs", "heads", "first-ref")
	initial := time.Unix(1_700_000_000, 100)

	writeTopologyIdentityFile(t, headPath, "ref: refs/heads/branch-a\n", initial)
	writeTopologyIdentityFile(t, branchAPath, strings.Repeat("a", 40)+"\n", initial)
	before := worktreeHeadIdentity(t, adminDir, commonDir)

	// Both writes happen between observations and the final HEAD contents are
	// byte-for-byte identical to the initial state. The revision metadata must
	// still make the stable probe notice the transition and reconcile the
	// checkout back from the transient first-ref state.
	writeTopologyIdentityFile(t, firstRefPath, strings.Repeat("a", 40)+"\n", initial.Add(time.Second))
	writeTopologyIdentityFile(t, headPath, "ref: refs/heads/first-ref\n", initial.Add(time.Second))
	writeTopologyIdentityFile(t, headPath, "ref: refs/heads/branch-a\n", initial.Add(2*time.Second))
	after := worktreeHeadIdentity(t, adminDir, commonDir)
	if before == after {
		t.Fatal("A -> B -> A HEAD transition produced the original probe identity")
	}
	if stable := worktreeHeadIdentity(t, adminDir, commonDir); stable != after {
		t.Fatal("unchanged HEAD metadata did not produce a stable probe identity")
	}
}

func TestWorktreeHeadIdentityDetectsActiveRefABA(t *testing.T) {
	commonDir := t.TempDir()
	adminDir := filepath.Join(commonDir, "worktrees", "linked")
	headPath := filepath.Join(adminDir, "HEAD")
	branchAPath := filepath.Join(commonDir, "refs", "heads", "branch-a")
	initial := time.Unix(1_700_000_000, 100)
	oidA := strings.Repeat("a", 40) + "\n"

	writeTopologyIdentityFile(t, headPath, "ref: refs/heads/branch-a\n", initial)
	writeTopologyIdentityFile(t, branchAPath, oidA, initial)
	before := worktreeHeadIdentity(t, adminDir, commonDir)

	writeTopologyIdentityFile(t, branchAPath, strings.Repeat("b", 40)+"\n", initial.Add(time.Second))
	writeTopologyIdentityFile(t, branchAPath, oidA, initial.Add(2*time.Second))
	after := worktreeHeadIdentity(t, adminDir, commonDir)
	if before == after {
		t.Fatal("A -> B -> A active-ref transition produced the original probe identity")
	}
	if stable := worktreeHeadIdentity(t, adminDir, commonDir); stable != after {
		t.Fatal("unchanged active-ref metadata did not produce a stable probe identity")
	}
}

func BenchmarkAppendWorktreeHeadIdentityStable(b *testing.B) {
	commonDir := b.TempDir()
	adminDir := filepath.Join(commonDir, "worktrees", "linked")
	modified := time.Unix(1_700_000_000, 100)
	writeTopologyIdentityFile(b, filepath.Join(adminDir, "HEAD"), "ref: refs/heads/branch-a\n", modified)
	writeTopologyIdentityFile(b, filepath.Join(commonDir, "refs", "heads", "branch-a"), strings.Repeat("a", 40)+"\n", modified)

	var builder strings.Builder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builder.Reset()
		if err := appendWorktreeHeadIdentity(&builder, adminDir, commonDir); err != nil {
			b.Fatalf("appendWorktreeHeadIdentity: %v", err)
		}
	}
}
