package analysis

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// sampleDiff is a synthetic unified diff (context width 3) covering two files
// with adds, deletes, and context lines so both the hunk parser and the
// line-carrying parser have something to chew on.
const sampleDiff = `diff --git a/pkg/foo.go b/pkg/foo.go
index 1111111..2222222 100644
--- a/pkg/foo.go
+++ b/pkg/foo.go
@@ -1,6 +1,7 @@
 package foo

 func Foo() int {
-	return 1
+	x := compute()
+	return x
 }

@@ -20,3 +21,3 @@ func Bar() {
 	a := 1
-	b := 2
+	b := 3
 	_ = a
diff --git a/pkg/baz.go b/pkg/baz.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/pkg/baz.go
@@ -0,0 +1,3 @@
+package baz
+
+func Baz() {}
`

func TestParseDiffHunksEqualsInternal(t *testing.T) {
	got := ParseDiffHunks(sampleDiff)
	want := parseDiffHunks(sampleDiff)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseDiffHunks != parseDiffHunks\n got: %#v\nwant: %#v", got, want)
	}
	if len(got) == 0 {
		t.Fatalf("expected some hunks from the sample diff, got none")
	}
}

// diffKey spells a repo-relative path the way parseDiffLines keys its map and
// DiffHunk.FilePath is cleaned: filepath.Clean, so the key carries the running
// platform's separators. Writing the '/' form directly reads fine on POSIX and
// misses on Windows, where the map key is `pkg\foo.go`.
func diffKey(p string) string { return filepath.Clean(p) }

func TestParseDiffLinesNewSide(t *testing.T) {
	lines := parseDiffLines(sampleDiff)

	foo := lines[diffKey("pkg/foo.go")]
	if len(foo) == 0 {
		t.Fatalf("expected new-side lines for pkg/foo.go")
	}
	// The first hunk starts at new line 1; the added "x := compute()" lands on
	// line 4 (after package/blank/func-sig context).
	var sawCompute bool
	for _, hl := range foo {
		// No removed lines must ever appear.
		if hl.Side != "+" && hl.Side != " " {
			t.Fatalf("unexpected side %q on %#v", hl.Side, hl)
		}
		if hl.Side == "+" && hl.Text == "\tx := compute()" {
			sawCompute = true
			if hl.NewLine != 4 {
				t.Fatalf("expected 'x := compute()' on new line 4, got %d", hl.NewLine)
			}
		}
		// The removed "return 1" / "b := 2" must not surface.
		if hl.Text == "\treturn 1" && hl.Side != " " {
			t.Fatalf("removed line leaked into new-side lines: %#v", hl)
		}
	}
	if !sawCompute {
		t.Fatalf("added line 'x := compute()' missing from new-side lines: %#v", foo)
	}

	// New-file lines all carry "+", numbered 1..3.
	baz := lines[diffKey("pkg/baz.go")]
	if len(baz) != 3 {
		t.Fatalf("expected 3 new-side lines for pkg/baz.go, got %d (%#v)", len(baz), baz)
	}
	for i, hl := range baz {
		if hl.Side != "+" {
			t.Fatalf("new file line %d should be an add, got side %q", i, hl.Side)
		}
		if hl.NewLine != i+1 {
			t.Fatalf("new file line %d numbered %d, want %d", i, hl.NewLine, i+1)
		}
	}
}

// newTestRepo creates a throwaway git repo with one committed file, mutates it,
// and returns the repo root. The base commit is on branch the caller diffs
// against via scope "all" (working tree vs HEAD).
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	// Force standard a/ b/ diff prefixes regardless of the developer's global
	// git config (mnemonic/noprefix would otherwise emit c/ w/ and defeat the
	// +++ b/ header match shared by MapGitDiff and parseDiffLines).
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")

	src := "package foo\n\nfunc Foo() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")

	// Modify: add a line inside Foo.
	mutated := "package foo\n\nfunc Foo() int {\n\tx := 1\n\treturn x\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMapGitDiffWithLinesReturnsNewSideLines(t *testing.T) {
	dir := newTestRepo(t)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, lines, err := MapGitDiffWithLines(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiffWithLines: %v", err)
	}
	if res == nil {
		t.Fatal("nil DiffResult")
	}
	foo := lines["foo.go"]
	if len(foo) == 0 {
		t.Fatalf("expected new-side lines for foo.go, got none (%#v)", lines)
	}
	var sawAdd bool
	for _, hl := range foo {
		if hl.Side == "+" {
			sawAdd = true
		}
		if hl.NewLine <= 0 {
			t.Fatalf("non-positive new line: %#v", hl)
		}
	}
	if !sawAdd {
		t.Fatalf("expected at least one added new-side line: %#v", foo)
	}

	// The changed symbol Foo should be detected (overlap logic unchanged).
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected Foo among changed symbols: %#v", res.ChangedSymbols)
	}
}

// TestMapGitDiffRepoPrefixJoin covers the multi-repo daemon shape: indexed
// file paths carry the repo prefix ("myrepo/foo.go") while git emits
// repo-relative hunk paths ("foo.go"). The prefix-aware join must find the
// symbol; ChangedFiles must stay diff-relative so git pathspec re-joins keep
// working.
func TestMapGitDiffRepoPrefixJoin(t *testing.T) {
	dir := newTestRepo(t)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "myrepo/foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "myrepo/foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, err := MapGitDiff(g, dir, "myrepo", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "myrepo/foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected prefixed Foo among changed symbols: %#v", res.ChangedSymbols)
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "foo.go" {
		t.Fatalf("ChangedFiles must keep diff-relative paths, got %#v", res.ChangedFiles)
	}

	// Without the prefix the join misses — the pre-fix behavior, kept for
	// single-repo graphs whose paths are unprefixed.
	res, err = MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff (no prefix): %v", err)
	}
	if len(res.ChangedSymbols) != 0 {
		t.Fatalf("unprefixed join against a prefixed graph should miss, got %#v", res.ChangedSymbols)
	}
}

// TestMapGitDiffMnemonicPrefixConfig pins the diff header prefixes against
// hostile git config: with diff.mnemonicPrefix=true a worktree diff emits
// "+++ w/..." headers, which the "+++ b/" parser anchor would zero out —
// every diff-driven tool would silently report an empty changeset. The -c
// overrides in GitDiffArgs must win over repo and global config.
func TestMapGitDiffMnemonicPrefixConfig(t *testing.T) {
	dir := newTestRepo(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Hostile repo-local config (newTestRepo sets both to false; flip them).
	run("config", "diff.mnemonicPrefix", "true")

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, err := MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if len(res.Hunks) == 0 {
		t.Fatalf("expected hunks despite diff.mnemonicPrefix=true, got none")
	}
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected Foo among changed symbols: %#v", res.ChangedSymbols)
	}

	run("config", "diff.noprefix", "true")
	res, err = MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff (noprefix): %v", err)
	}
	if len(res.Hunks) == 0 {
		t.Fatalf("expected hunks despite diff.noprefix=true, got none")
	}
}

func TestJoinFileNodes(t *testing.T) {
	// Graph keys are "<prefix>/" + the remainder in native separators, so the
	// fixture is built the same way the indexer would build it. Writing the
	// '/'-joined form here would pass on POSIX and describe a key that does not
	// exist on Windows.
	key := func(rel string) string { return "myrepo/" + filepath.FromSlash(rel) }

	g := graph.New()
	g.AddNode(&graph.Node{ID: key("a.go") + "::A", Kind: graph.KindFunction, Name: "A", FilePath: key("a.go")})
	// The shadow: the repo's own tree carries a top-level directory named like
	// the repo prefix, so "myrepo/a.go" is a well-formed path in BOTH domains —
	// the git-relative spelling of this nested file, and the graph key of the
	// top-level a.go above.
	g.AddNode(&graph.Node{ID: key("myrepo/a.go") + "::Nested", Kind: graph.KindFunction, Name: "Nested", FilePath: key("myrepo/a.go")})

	// A repo-relative path is prefixed unconditionally.
	if nodes := JoinFileNodes(g, "myrepo", "a.go", RepoRelativePath); len(nodes) != 1 || nodes[0].ID != key("a.go")+"::A" {
		t.Fatalf("repo-relative path must resolve through the prefix: %#v", nodes)
	}
	// The shadow case: a repo-relative path that merely LOOKS prefixed still
	// gets prefixed, so it resolves to the nested file it names — not to the
	// same-named top-level file it would collide with.
	if nodes := JoinFileNodes(g, "myrepo", "myrepo/a.go", RepoRelativePath); len(nodes) != 1 || nodes[0].ID != key("myrepo/a.go")+"::Nested" {
		t.Fatalf("prefix-shadowed repo-relative path must resolve to the nested file: %#v", nodes)
	}
	// A caller already holding a graph key says so, and it is used as-is.
	if nodes := JoinFileNodes(g, "myrepo", key("a.go"), GraphKeyedPath); len(nodes) != 1 || nodes[0].ID != key("a.go")+"::A" {
		t.Fatalf("graph-keyed path must be used verbatim: %#v", nodes)
	}
	// A graph key is used verbatim whether or not a prefix is supplied.
	if nodes := JoinFileNodes(g, "", key("a.go"), GraphKeyedPath); len(nodes) != 1 || nodes[0].ID != key("a.go")+"::A" {
		t.Fatalf("a graph key must be used verbatim with no prefix: %#v", nodes)
	}
	if nodes := JoinFileNodes(g, "myrepo", "missing.go", RepoRelativePath); len(nodes) != 0 {
		t.Fatalf("a miss must stay a miss: %#v", nodes)
	}

	// Standalone indexer: no repo prefix, but the keys are still native. A
	// forge or git path arrives '/'-spelled and must still resolve.
	standalone := graph.New()
	nested := filepath.FromSlash("pkg/auth/x.go")
	standalone.AddNode(&graph.Node{ID: nested + "::Login", Kind: graph.KindFunction, Name: "Login", FilePath: nested})
	if nodes := JoinFileNodes(standalone, "", "pkg/auth/x.go", RepoRelativePath); len(nodes) != 1 || nodes[0].ID != nested+"::Login" {
		t.Fatalf("empty prefix must still convert separators: %#v", nodes)
	}
}

func TestGraphKey(t *testing.T) {
	native := func(rel string) string { return "myrepo/" + filepath.FromSlash(rel) }
	for _, tc := range []struct {
		name         string
		prefix, path string
		domain       PathDomain
		want         string
	}{
		{"repo-relative gains the prefix", "myrepo", "a.go", RepoRelativePath, native("a.go")},
		{"prefix-shadowed path still gains it", "myrepo", "myrepo/a.go", RepoRelativePath, native("myrepo/a.go")},
		{"graph-keyed path is untouched", "myrepo", "myrepo/a.go", GraphKeyedPath, "myrepo/a.go"},
		{"no prefix is a no-op", "", "a.go", RepoRelativePath, "a.go"},
		// Multi-segment with no prefix: the standalone indexer's keys still
		// carry native separators, so a '/'-spelled forge path must be
		// converted even though there is nothing to prefix. A single-segment
		// fixture cannot show this — there is no separator to get wrong.
		{"no prefix still converts separators", "", "pkg/auth/x.go", RepoRelativePath, filepath.FromSlash("pkg/auth/x.go")},
		{"no prefix, graph-keyed stays verbatim", "", "pkg/auth/x.go", GraphKeyedPath, "pkg/auth/x.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GraphKey(tc.prefix, tc.path, tc.domain); got != tc.want {
				t.Fatalf("GraphKey(%q, %q, %v) = %q, want %q", tc.prefix, tc.path, tc.domain, got, tc.want)
			}
		})
	}
}

// TestMapGitDiffUnchanged asserts the existing --unified=0 path still yields the
// same DiffResult shape (hunks + changed symbols + changed files) it always did,
// independent of the new sibling.
func TestMapGitDiffUnchanged(t *testing.T) {
	dir := newTestRepo(t)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID:        "foo.go::Foo",
		Kind:      graph.KindFunction,
		Name:      "Foo",
		FilePath:  "foo.go",
		StartLine: 3,
		EndLine:   6,
		Language:  "go",
	})

	res, err := MapGitDiff(g, dir, "", "all", "")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if len(res.Hunks) == 0 {
		t.Fatalf("expected hunks, got none")
	}
	for _, h := range res.Hunks {
		if h.FilePath != "foo.go" {
			t.Fatalf("unexpected hunk file %q", h.FilePath)
		}
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "foo.go" {
		t.Fatalf("expected changed files [foo.go], got %#v", res.ChangedFiles)
	}
	var sawFoo bool
	for _, cs := range res.ChangedSymbols {
		if cs.ID == "foo.go::Foo" {
			sawFoo = true
		}
	}
	if !sawFoo {
		t.Fatalf("expected Foo among changed symbols: %#v", res.ChangedSymbols)
	}
}

// --- file-level change detection (issue #546) ---------------------------------

// newDeleteRepo commits gone.go (two symbols) plus a non-symbol README so the
// delete/rename cases have both an indexed and an unindexed file to move.
func newDeleteRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")

	src := "package foo\n\nfunc Gone() int {\n\treturn 2\n}\n\nfunc AlsoGone() int {\n\treturn 3\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "gone.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")
	return dir
}

// deleteRepoGraph indexes the two symbols gone.go holds before the change.
func deleteRepoGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "gone.go::Gone", Kind: graph.KindFunction, Name: "Gone",
		FilePath: "gone.go", StartLine: 3, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "gone.go::AlsoGone", Kind: graph.KindFunction, Name: "AlsoGone",
		FilePath: "gone.go", StartLine: 7, EndLine: 9, Language: "go",
	})
	return g
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func hasFile(files []string, want string) bool {
	for _, f := range files {
		if f == want {
			return true
		}
	}
	return false
}

func symbolIDs(syms []ChangedSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.ID)
	}
	sort.Strings(out)
	return out
}

func findFileChange(changes []FileChange, path string) (FileChange, bool) {
	for _, c := range changes {
		if c.Path == path {
			return c, true
		}
	}
	return FileChange{}, false
}

// TestMapGitDiffDeletedFile pins the headline defect of issue #546: a deleted
// file's new side is /dev/null, so the pre-fix parser cleared the current path
// and skipped the @@ header that followed. The delete produced no hunk, no
// changed file and no changed symbol — a delete-only change was reported as a
// clean tree in every scope.
func TestMapGitDiffDeletedFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  bool
		scopes []string
	}{
		{name: "worktree", scopes: []string{"unstaged", "all"}},
		{name: "staged", stage: true, scopes: []string{"staged", "all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDeleteRepo(t)
			if tc.stage {
				gitIn(t, dir, "rm", "-q", "gone.go")
			} else if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
				t.Fatal(err)
			}

			for _, scope := range tc.scopes {
				res, err := MapGitDiff(deleteRepoGraph(), dir, "", scope, "main")
				if err != nil {
					t.Fatalf("MapGitDiff(%s): %v", scope, err)
				}
				if !hasFile(res.ChangedFiles, "gone.go") {
					t.Fatalf("scope %s: deleted file missing from ChangedFiles: %#v", scope, res.ChangedFiles)
				}
				fc, ok := findFileChange(res.FileChanges, "gone.go")
				if !ok || fc.Kind != FileDeleted {
					t.Fatalf("scope %s: want a deleted FileChange for gone.go, got %#v", scope, res.FileChanges)
				}
				// Every symbol the graph still holds for the file is gone with it.
				if got := symbolIDs(res.ChangedSymbols); len(got) != 2 ||
					got[0] != "gone.go::AlsoGone" || got[1] != "gone.go::Gone" {
					t.Fatalf("scope %s: want both deleted symbols, got %#v", scope, got)
				}
			}
		})
	}
}

// TestMapGitDiffDeletedFileHunkIsOldSide pins the line numbers a deleted file
// reports. There is no new side to number, so the range must be the old-side
// span the file occupied — that is what overlaps the still-indexed symbols —
// and it must be flagged so a consumer never anchors it as a new-side line.
func TestMapGitDiffDeletedFileHunkIsOldSide(t *testing.T) {
	dir := newDeleteRepo(t)
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	res, err := MapGitDiff(deleteRepoGraph(), dir, "", "all", "main")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	var found bool
	for _, h := range res.Hunks {
		if h.FilePath != "gone.go" {
			continue
		}
		found = true
		if !h.Deleted {
			t.Fatalf("deleted-file hunk must be flagged Deleted: %#v", h)
		}
		if h.StartLine != 1 || h.EndLine != 9 {
			t.Fatalf("want the old-side span 1..9 the file occupied, got %d..%d", h.StartLine, h.EndLine)
		}
	}
	if !found {
		t.Fatalf("no hunk for the deleted file: %#v", res.Hunks)
	}
}

// TestMapGitDiffDeletedEmptyFile covers the delete that carries no @@ header at
// all: git emits only the "deleted file mode" header for an empty file. The
// file-level record is the only thing that can keep it visible.
func TestMapGitDiffDeletedEmptyFile(t *testing.T) {
	dir := newDeleteRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "empty")
	gitIn(t, dir, "rm", "-q", "empty.txt")

	res, err := MapGitDiff(deleteRepoGraph(), dir, "", "all", "main")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if !hasFile(res.ChangedFiles, "empty.txt") {
		t.Fatalf("deleted empty file missing from ChangedFiles: %#v", res.ChangedFiles)
	}
	if fc, ok := findFileChange(res.FileChanges, "empty.txt"); !ok || fc.Kind != FileDeleted {
		t.Fatalf("want a deleted FileChange for empty.txt, got %#v", res.FileChanges)
	}
}

// TestMapGitDiffRename pins the second half of issue #546: a 100%-similar
// rename carries rename from/to headers and no @@ header whatsoever, so a
// hunk-driven parser reported a pure `git mv` as an empty diff. Both paths must
// surface, linked, and the symbols indexed under the old path must come with it.
func TestMapGitDiffRename(t *testing.T) {
	for _, tc := range []struct{ name, edit string }{
		{name: "pure"},
		{name: "with_edits", edit: "package foo\n\nfunc Gone() int {\n\treturn 99\n}\n\nfunc AlsoGone() int {\n\treturn 3\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDeleteRepo(t)
			gitIn(t, dir, "mv", "gone.go", "moved.go")
			if tc.edit != "" {
				if err := os.WriteFile(filepath.Join(dir, "moved.go"), []byte(tc.edit), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			res, err := MapGitDiff(deleteRepoGraph(), dir, "", "all", "main")
			if err != nil {
				t.Fatalf("MapGitDiff: %v", err)
			}
			if !hasFile(res.ChangedFiles, "gone.go") || !hasFile(res.ChangedFiles, "moved.go") {
				t.Fatalf("a rename changes both paths, got ChangedFiles %#v", res.ChangedFiles)
			}
			fc, ok := findFileChange(res.FileChanges, "moved.go")
			if !ok || fc.Kind != FileRenamed || fc.PreviousPath != "gone.go" {
				t.Fatalf("want moved.go renamed from gone.go, got %#v", res.FileChanges)
			}
			// The graph still indexes the symbols under the old path; moving
			// the file affects every one of them, and no line range says so.
			if got := symbolIDs(res.ChangedSymbols); len(got) != 2 ||
				got[0] != "gone.go::AlsoGone" || got[1] != "gone.go::Gone" {
				t.Fatalf("want both moved symbols, got %#v", got)
			}
		})
	}
}

// TestMapGitDiffFileChangeKinds pins the remaining kinds, including the
// non-symbol files issue #546 calls out: a changed manifest or doc has no
// indexed symbol, so the file-level record is the only evidence it changed.
func TestMapGitDiffFileChangeKinds(t *testing.T) {
	dir := newDeleteRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# doc\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "added.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")

	res, err := MapGitDiff(deleteRepoGraph(), dir, "", "all", "main")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	for path, want := range map[string]FileChangeKind{
		"README.md":  FileModified,
		"added.json": FileAdded,
	} {
		fc, ok := findFileChange(res.FileChanges, path)
		if !ok || fc.Kind != want {
			t.Fatalf("want %s classified %q, got %#v", path, want, res.FileChanges)
		}
		if !hasFile(res.ChangedFiles, path) {
			t.Fatalf("%s missing from ChangedFiles: %#v", path, res.ChangedFiles)
		}
	}
	if len(res.ChangedSymbols) != 0 {
		t.Fatalf("neither file holds an indexed symbol, got %#v", res.ChangedSymbols)
	}
}

// TestMapGitDiffModeOnlyChange covers the entry that carries no ---/+++ and no
// rename header: chmod. The "diff --git a/P b/P" line is its only path source.
func TestMapGitDiffModeOnlyChange(t *testing.T) {
	dir := newDeleteRepo(t)
	if err := os.Chmod(filepath.Join(dir, "README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Skip on a filesystem git cannot see the mode bit through — but decide
	// that from git itself, so the skip can never stand in for a real miss.
	cmd := exec.Command("git", "diff", "--name-only", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff --name-only: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "README.md") {
		t.Skipf("git does not report the mode change on this filesystem: %q", out)
	}

	res, err := MapGitDiff(deleteRepoGraph(), dir, "", "all", "main")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if !hasFile(res.ChangedFiles, "README.md") {
		t.Fatalf("mode-only change missing from ChangedFiles: %#v", res.ChangedFiles)
	}
	if fc, ok := findFileChange(res.FileChanges, "README.md"); !ok || fc.Kind != FileModified {
		t.Fatalf("want README.md modified, got %#v", res.FileChanges)
	}
}

// TestParseDiffGitPaths pins the same-path recovery, including a path with a
// space, and the deliberate refusal to guess when the two sides differ.
func TestParseDiffGitPaths(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"diff --git a/pkg/foo.go b/pkg/foo.go", "pkg/foo.go"},
		{"diff --git a/my dir/foo.go b/my dir/foo.go", "my dir/foo.go"},
		// Differing sides are a rename or a copy; the rename headers below
		// carry the truth, so guessing a split here would only be wrong.
		{"diff --git a/old.go b/new.go", ""},
		{`diff --git "a/od\td.go" "b/od\td.go"`, ""},
		{"diff --git nonsense", ""},
	} {
		// The recovered path is cleaned, so it carries native separators;
		// the '/' spelling above is the diff's, not the result's.
		want := tc.want
		if want != "" {
			want = filepath.Clean(want)
		}
		if got := parseDiffGitPaths(tc.line); got != want {
			t.Fatalf("parseDiffGitPaths(%q) = %q, want %q", tc.line, got, want)
		}
	}
}

// TestMapGitDiffSurfacesGitFailure pins the third defect: MapGitDiff used to
// discard the error whenever stdout happened to be empty, so an unknown base
// ref or a root that is not a work tree returned a clean, confident "no
// changes" result. Both must now fail loudly.
func TestMapGitDiffSurfacesGitFailure(t *testing.T) {
	t.Run("unknown_base_ref", func(t *testing.T) {
		dir := newDeleteRepo(t)
		if _, err := MapGitDiff(deleteRepoGraph(), dir, "", "compare", "no-such-branch"); err == nil {
			t.Fatal("an unknown base ref must not be reported as an empty diff")
		}
	})
	t.Run("not_a_work_tree", func(t *testing.T) {
		if _, err := MapGitDiff(deleteRepoGraph(), t.TempDir(), "", "unstaged", "main"); err == nil {
			t.Fatal("a root that is not a git work tree must not be reported as an empty diff")
		}
	})
	t.Run("with_lines", func(t *testing.T) {
		if _, _, err := MapGitDiffWithLines(deleteRepoGraph(), t.TempDir(), "", "unstaged", "main"); err == nil {
			t.Fatal("MapGitDiffWithLines must surface the same failure")
		}
	})
}

// TestMapGitDiffUntrackedStaysUnobserved pins the documented limitation rather
// than a defect: `git diff` never lists untracked files, so no scope can see
// them. detect_changes says so in its response note; this keeps the two honest
// with each other, and keeps the fix above from silently growing an
// ignored-tree walk (issue #324).
func TestMapGitDiffUntrackedStaysUnobserved(t *testing.T) {
	dir := newDeleteRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package foo\n\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MapGitDiff(deleteRepoGraph(), dir, "", "all", "main")
	if err != nil {
		t.Fatalf("MapGitDiff: %v", err)
	}
	if hasFile(res.ChangedFiles, "new.go") {
		t.Fatalf("git diff cannot observe untracked files; ChangedFiles = %#v", res.ChangedFiles)
	}
}

// TestJoinHunksToSymbolsPrefixedDeleteAndRename binds the old-side join for the
// two change kinds that carry no usable hunk: a delete (no new side) and a
// rename (PreviousPath). Both resolve through the same RepoRelativePath
// contract as a hunk path, and both were previously covered only with an empty
// prefix, where the contract cannot be observed.
//
// The fixture is prefix-shadowed: the repo's own tree carries a top-level
// directory named like the repo prefix, so each old-side git path is also a
// well-formed graph key for a different, untouched file.
func TestJoinHunksToSymbolsPrefixedDeleteAndRename(t *testing.T) {
	const prefix = "repo-a"
	key := func(rel string) string { return prefix + "/" + filepath.FromSlash(rel) }

	g := graph.New()
	add := func(rel, sym string) {
		g.AddNode(&graph.Node{
			ID: key(rel) + "::" + sym, Kind: graph.KindFunction, Name: sym,
			FilePath: key(rel), StartLine: 1, EndLine: 10, Language: "go",
		})
	}
	// Shadows: what a repo-relative old-side path would resolve to if it were
	// mistaken for a graph key.
	add("pkg/gone.go", "ShadowGone")
	add("pkg/old.go", "ShadowOld")
	// The real old-side files, nested under the prefix-named directory.
	add("repo-a/pkg/gone.go", "Gone")
	add("repo-a/pkg/old.go", "Old")

	res := joinHunksToSymbols(g, prefix, nil, []FileChange{
		{Path: "repo-a/pkg/gone.go", Kind: FileDeleted},
		{Path: "repo-a/pkg/new.go", PreviousPath: "repo-a/pkg/old.go", Kind: FileRenamed},
	})

	got := map[string]bool{}
	for _, cs := range res.ChangedSymbols {
		got[cs.ID] = true
	}
	for _, want := range []string{key("repo-a/pkg/gone.go") + "::Gone", key("repo-a/pkg/old.go") + "::Old"} {
		if !got[want] {
			t.Fatalf("old-side symbol %q missing: %#v", want, res.ChangedSymbols)
		}
	}
	for _, shadow := range []string{key("pkg/gone.go") + "::ShadowGone", key("pkg/old.go") + "::ShadowOld"} {
		if got[shadow] {
			t.Fatalf("unchanged shadow %q was taken as changed: %#v", shadow, res.ChangedSymbols)
		}
	}
}
