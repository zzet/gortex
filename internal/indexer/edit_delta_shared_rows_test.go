package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Rows no single file owns, through the per-file delta path: a registry node
// several files emit (a shared string literal) must be served as the copy of
// the smallest file that still emits it, and a builtin sentinel must exist
// exactly while something refers to it — as in a whole index of the tree.

const sharedRowsModule = "example.com/shared"

func sharedRowsTree() map[string]string {
	return map[string]string{
		"go.mod": "module " + sharedRowsModule + "\n\ngo 1.22\n",
		"a/a.go": `package a

import "errors"

func A() error {
	return errors.New("boom")
}

func Size(xs []int) int {
	return len(xs)
}
`,
		"b/b.go": `package b

import "errors"

func B() error {
	return errors.New("boom")
}
`,
		"c/c.go": `package c

func C() int {
	return 3
}
`,
	}
}

// sharedRowsStep is one working-tree edit of the shared-rows fixture.
type sharedRowsStep struct {
	name  string
	apply func(t *testing.T, root string)
}

// sharedRowsSteps edits the tree so the kept copy of the shared literal moves
// between files and the only referrer of a builtin comes and goes.
func sharedRowsSteps(original map[string]string) []sharedRowsStep {
	edit := func(name, rel, from, to string) sharedRowsStep {
		return sharedRowsStep{name: name, apply: func(t *testing.T, root string) {
			full := filepath.Join(root, filepath.FromSlash(rel))
			body, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("%s: read %s: %v", name, rel, err)
			}
			if !strings.Contains(string(body), from) {
				t.Fatalf("%s: %s does not contain %q", name, rel, from)
			}
			if err := os.WriteFile(full, []byte(strings.Replace(string(body), from, to, 1)), 0o644); err != nil {
				t.Fatalf("%s: write %s: %v", name, rel, err)
			}
		}}
	}
	restore := func(name string, rels ...string) sharedRowsStep {
		return sharedRowsStep{name: name, apply: func(t *testing.T, root string) {
			for _, rel := range rels {
				if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(original[rel]), 0o644); err != nil {
					t.Fatalf("%s: restore %s: %v", name, rel, err)
				}
			}
		}}
	}
	return []sharedRowsStep{
		// The larger file gains a second site: the smaller file's copy stays.
		edit("larger file re-emits", "b/b.go", "func B() error {\n", "func B() error {\n\t_ = errors.New(\"boom\")\n"),
		// The kept file stops emitting it: the next smallest file's copy is served.
		edit("kept file drops it", "a/a.go", `return errors.New("boom")`, `return errors.New("bang")`),
		// A smaller file than every emitter starts emitting it.
		edit("smaller file emits", "c/c.go", "func C() int {\n", "func C() int {\n\t_ = fmt.Errorf(\"boom\")\n"),
		edit("smaller file imports", "c/c.go", "package c\n", "package c\n\nimport \"fmt\"\n"),
		// The only referrer of a builtin goes away, then comes back.
		edit("builtin loses its referrer", "a/a.go", "return len(xs)", "return 0"),
		restore("undo a/a.go", "a/a.go"),
		restore("undo all", "a/a.go", "b/b.go", "c/c.go"),
	}
}

// sharedRowsWatched is the set of identities the test compares: the shared
// literal and every builtin sentinel, in either the served view or the clean
// index.
func sharedRowsWatched(readers ...graph.Reader) []string {
	seen := map[string]struct{}{}
	for _, r := range readers {
		for n := range r.NodesByKind(graph.KindString) {
			seen[n.ID] = struct{}{}
		}
		for n := range r.NodesByKind(graph.KindBuiltin) {
			seen[n.ID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sharedRowsRender(r graph.Reader, id string) string {
	n := r.GetNode(id)
	if n == nil {
		return "<absent>"
	}
	var in []string
	for _, e := range r.GetInEdges(id) {
		in = append(in, fmt.Sprintf("%s-%s@%s:%d", e.From, e.Kind, e.FilePath, e.Line))
	}
	sort.Strings(in)
	return fmt.Sprintf("%s kind=%s file=%s lines=%d-%d ws=%q meta=%s in=%v",
		n.ID, n.Kind, n.FilePath, n.StartLine, n.EndLine, n.WorkspaceID, sharedRowsMeta(n.Meta), in)
}

func TestEditDeltaServesSharedRowsLikeAWholeIndex(t *testing.T) {
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	for i, step := range sharedRowsSteps(sharedRowsTree()) {
		step.apply(t, f.worktree)
		out := coordinatorReconcile(t, c)
		view := chainMaterialize(t, f)
		clean := builderOpenStore(t, fmt.Sprintf("shared-clean-%d", i))
		builderIndex(t, clean, f.worktree)
		for _, id := range sharedRowsWatched(view.Reader, clean) {
			served, want := sharedRowsRender(view.Reader, id), sharedRowsRender(clean, id)
			if served != want {
				t.Errorf("step %d %q (gen %d, parent %d): %s\n served %s\n  clean %s",
					i, step.name, out.DirtyGenerationID, out.DirtyParentGenerationID, id, served, want)
			}
		}
		view.Close()
	}
}

func sharedRowsMeta(meta map[string]any) string {
	if len(meta) == 0 {
		return "{}"
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Sprint(meta)
	}
	return string(data)
}

// A builtin sentinel the delta re-stamps is claimed by identity only: the
// value flows out of it that other files record (len's result flowing into a
// function elsewhere) stay served. A legacy tombstone would take over the
// builtin's whole outgoing set and hide them.
func TestEditDeltaKeepsABuiltinsEdgesRecordedInOtherFiles(t *testing.T) {
	tree := map[string]string{
		"go.mod": "module example.com/builtins\n\ngo 1.22\n",
		"a/a.go": "package a\n\nfunc Size(xs []int) int {\n\treturn len(xs)\n}\n",
		"c/c.go": "package c\n\nfunc Count(s string) int {\n\tn := len(s)\n\treturn n\n}\n\nfunc Empty(s string) bool {\n\treturn len(s) == 0\n}\n\nfunc Grow(xs []int) []int {\n\txs = append(xs, len(xs))\n\treturn xs\n}\n",
	}
	f := newCoordinatorFixtureWithTree(t, tree)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	full := filepath.Join(f.worktree, "a", "a.go")
	for i, body := range []string{
		// a.go keeps its use of len: the builtin is re-stamped.
		"package a\n\n// Size counts.\nfunc Size(xs []int) int {\n\treturn len(xs)\n}\n",
		// a.go stops using len, which c.go still uses: it must stay.
		"package a\n\n// Size counts.\nfunc Size(xs []int) int {\n\treturn 0\n}\n",
	} {
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		coordinatorReconcile(t, c)
		editDeltaAssertBuiltinAdjacency(t, f, fmt.Sprintf("builtins-clean-%d", i))
	}
}

func editDeltaAssertBuiltinAdjacency(t *testing.T, f *coordinatorFixture, label string) {
	t.Helper()
	view := chainMaterialize(t, f)
	defer view.Close()
	clean := builderOpenStore(t, label)
	builderIndex(t, clean, f.worktree)
	render := func(r graph.Reader) string {
		var rows []string
		for n := range r.NodesByKind(graph.KindBuiltin) {
			for _, e := range r.GetOutEdges(n.ID) {
				rows = append(rows, fmt.Sprintf("out %s-%s->%s@%s:%d", e.From, e.Kind, e.To, e.FilePath, e.Line))
			}
			for _, e := range r.GetInEdges(n.ID) {
				rows = append(rows, fmt.Sprintf("in %s-%s->%s@%s:%d", e.From, e.Kind, e.To, e.FilePath, e.Line))
			}
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n")
	}
	served, want := render(view.Reader), render(clean)
	if !strings.Contains(want, "out repo::builtin::go::") || !strings.Contains(want, "@repo/c/c.go") {
		t.Fatalf("the fixture records no builtin out-edge in c/c.go:\n%s", want)
	}
	if served != want {
		t.Fatalf("builtin adjacency differs from a whole index\n served:\n%s\n clean:\n%s", served, want)
	}
}

// A row a file emits under an identity no file mask reaches — the contract
// node of an environment variable the file reads — goes with the file: when
// the file is deleted and nothing else refers to it, the delta removes it by
// name, as a whole index of the tree has no such row.
func TestEditDeltaRemovesADeletedFilesForeignRows(t *testing.T) {
	tree := map[string]string{
		"go.mod": "module example.com/foreign\n\ngo 1.22\n",
		"a/a.go": "package a\n\nimport \"os\"\n\nfunc Mode() string {\n\treturn os.Getenv(\"FOREIGN_MODE\")\n}\n",
		"b/b.go": "package b\n\nfunc B() int {\n\treturn 1\n}\n",
	}
	f := newCoordinatorFixtureWithTree(t, tree)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	base := chainMaterialize(t, f)
	found := base.Reader.GetNode("env::FOREIGN_MODE") != nil
	base.Close()
	if !found {
		t.Skip("the extractor emits no environment contract for os.Getenv here")
	}
	if err := os.Remove(filepath.Join(f.worktree, "a", "a.go")); err != nil {
		t.Fatal(err)
	}
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	clean := builderOpenStore(t, "foreign-clean")
	builderIndex(t, clean, f.worktree)
	if clean.GetNode("env::FOREIGN_MODE") != nil {
		t.Fatalf("the fixture's clean index still holds the contract")
	}
	if n := view.Reader.GetNode("env::FOREIGN_MODE"); n != nil {
		t.Fatalf("the deleted file's contract is still served: %+v", *n)
	}
}
