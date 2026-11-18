package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// worktreeFixture lays out a primary checkout with a linked worktree nested
// under it, the shape `git worktree add .claude/worktrees/x` produces, and
// returns (primary root, worktree root).
func worktreeFixture(t *testing.T) (string, string) {
	t.Helper()
	primary := filepath.Join(t.TempDir(), "primary")
	worktree := filepath.Join(primary, ".claude", "worktrees", "x")
	for _, dir := range []string{
		filepath.Join(primary, ".git", "worktrees", "x"),
		filepath.Join(primary, "src"),
		filepath.Join(worktree, "src"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pointer := "gitdir: " + filepath.Join(primary, ".git", "worktrees", "x") + "\n"
	writeFixtureFile(t, filepath.Join(worktree, ".git"), pointer)
	writeFixtureFile(t, filepath.Join(primary, "src", "app.py"), "def app():\n    pass\n")
	writeFixtureFile(t, filepath.Join(worktree, "src", "new.py"), "def new():\n    pass\n")
	return primary, worktree
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// standardReadRemedy is the header of the unconditional read-side remedy —
// advice that resolves against the session's checkout.
const standardReadRemedy = "Call `explore` first, then use `read` instead:\n"

func TestReadDenyInForeignWorktreeNamesItsView(t *testing.T) {
	primary, worktree := worktreeFixture(t)
	stubIndexedFile(t, true, 2)
	stubFileOutline(t, nil, false)

	file := filepath.Join(worktree, "src", "new.py")
	result := enrichRead(map[string]any{"file_path": file}, primary)
	if !result.deny {
		t.Fatalf("indexed worktree Read was not denied: %#v", result)
	}
	if strings.Contains(result.reason, standardReadRemedy) {
		t.Fatalf("worktree deny prescribes the session-view remedy: %q", result.reason)
	}
	sel := worktreeViewSelector(worktree)
	if !strings.Contains(result.reason, sel) {
		t.Fatalf("worktree deny does not name the worktree view %s: %q", sel, result.reason)
	}
	if strings.Count(result.reason, sel) < 4 {
		t.Fatalf("every redirected call should carry the view selector: %q", result.reason)
	}
}

func TestReadAdvisoryForBranchNewWorktreeFileNamesItsView(t *testing.T) {
	primary, worktree := worktreeFixture(t)
	// Tracked, indexable, not in the graph: a file the branch just added.
	stubIndexedFile(t, false, 0)

	file := filepath.Join(worktree, "src", "new.py")
	result := enrichRead(map[string]any{"file_path": file}, primary)
	if result.deny {
		t.Fatalf("not-yet-indexed worktree Read was denied: %#v", result)
	}
	if strings.Contains(result.context, "Use `explore` first") {
		t.Fatalf("branch-new worktree file got the session-view advisory: %q", result.context)
	}
	if !strings.Contains(result.context, worktreeViewSelector(worktree)) {
		t.Fatalf("advisory does not name the worktree view: %q", result.context)
	}
}

func TestReadDenyInForeignWorktreeReportsFallbackView(t *testing.T) {
	primary, worktree := worktreeFixture(t)
	st := indexedStatus(2)
	st.View = &daemon.ProbeView{Kind: daemon.ProbeViewBase, FallbackReason: "unavailable"}
	stubFileIndexScope(t, st)
	stubFileOutline(t, nil, false)

	result := enrichRead(map[string]any{"file_path": filepath.Join(worktree, "src", "new.py")}, primary)
	if !result.deny {
		t.Fatalf("indexed worktree Read was not denied: %#v", result)
	}
	if !strings.Contains(result.reason, "(unavailable)") || !strings.Contains(result.reason, "require_exact:true") {
		t.Fatalf("fallback view is not surfaced in the deny: %q", result.reason)
	}
}

func TestReadDenyKeepsStandardRemedyOutsideForeignWorktrees(t *testing.T) {
	primary, worktree := worktreeFixture(t)
	stubIndexedFile(t, true, 7)
	stubFileOutline(t, nil, false)

	cases := []struct {
		name, file, cwd string
	}{
		{"primary file from primary", filepath.Join(primary, "src", "app.py"), primary},
		{"worktree file from inside that worktree", filepath.Join(worktree, "src", "new.py"), worktree},
		{"relative primary file", "src/app.py", primary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := enrichRead(map[string]any{"file_path": tc.file}, tc.cwd)
			if want := baselineReadDeny(tc.file, 7); result.reason != want {
				t.Fatalf("deny = %q, want exactly %q", result.reason, want)
			}
		})
	}
}

func TestGrepDenyScopedIntoForeignWorktreeNamesItsView(t *testing.T) {
	primary, worktree := worktreeFixture(t)
	stubIndexedFile(t, true, 2)

	file := filepath.Join(worktree, "src", "new.py")
	result := enrichGrep(map[string]any{"pattern": "new", "path": file}, 0, primary)
	if !result.deny {
		t.Fatalf("indexed worktree Grep was not denied: %#v", result)
	}
	if !strings.Contains(result.reason, worktreeViewSelector(worktree)) {
		t.Fatalf("worktree Grep deny does not name the worktree view: %q", result.reason)
	}

	primaryFile := filepath.Join(primary, "src", "app.py")
	result = enrichGrep(map[string]any{"pattern": "app", "path": primaryFile}, 0, primary)
	if want := formatTrackedSearchDeny("Grep", "app"); result.reason != want {
		t.Fatalf("primary Grep deny = %q, want exactly %q", result.reason, want)
	}
}

func TestLinkedWorktreeRootIgnoresCheckoutsAndSubmodules(t *testing.T) {
	primary, worktree := worktreeFixture(t)
	if got := linkedWorktreeRoot(filepath.Join(worktree, "src", "new.py")); got != worktree {
		t.Fatalf("linkedWorktreeRoot(worktree file) = %q, want %q", got, worktree)
	}
	if got := linkedWorktreeRoot(filepath.Join(primary, "src", "app.py")); got != "" {
		t.Fatalf("linkedWorktreeRoot(primary file) = %q, want \"\"", got)
	}
	sub := filepath.Join(primary, "vendor", "lib")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")
	if got := linkedWorktreeRoot(filepath.Join(sub, "lib.go")); got != "" {
		t.Fatalf("linkedWorktreeRoot(submodule file) = %q, want \"\"", got)
	}
}
