package indexer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Opt-in (GX_DECL_DIFF_REPLAY_REPO=<a git checkout>, GX_DECL_DIFF_REPLAY_REV,
// default HEAD): replays ordinary edits on the four reference files and
// reports, for each, the declaration diff of the file's rows before and
// after. Each side indexes the file's package at the revision (extraction
// and in-package resolution, no enrichment), so the diff says which
// declarations an edit changes and whether it would qualify for a row-level
// form. It measures; it asserts nothing about the answer.
func TestDeclarationDiffReplay(t *testing.T) {
	repo := os.Getenv("GX_DECL_DIFF_REPLAY_REPO")
	if repo == "" {
		t.Skip("GX_DECL_DIFF_REPLAY_REPO not set")
	}
	rev := os.Getenv("GX_DECL_DIFF_REPLAY_REV")
	if rev == "" {
		rev = "HEAD"
	}
	targets := map[string]string{ // the harness's renamed function per file
		"internal/gitstate/dirty.go":           "newDirtySampler",
		"internal/indexer/checkout_refresh.go": "retryableCheckoutRefreshError",
		"internal/mcp/checkout_binding.go":     "requestRequiresExactCheckoutView",
		"internal/config/config.go":            "matchesSkipRule",
	}
	files := make([]string, 0, len(targets))
	for f := range targets {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, rel := range files {
		src := gitShow(t, repo, rev, rel)
		base := replayRows(t, repo, rev, rel, src)
		for _, edit := range replayEdits(t, src, targets[rel]) {
			if only := os.Getenv("GX_DECL_DIFF_REPLAY_EDITS"); only != "" && !strings.Contains(","+only+",", ","+edit.name+",") {
				continue
			}
			if edit.src == "" {
				t.Logf("%-38s %-24s (no such declaration)", rel, edit.name)
				continue
			}
			next := replayRows(t, repo, rev, rel, edit.src)
			d := diffDeclarations(base.nodes, base.edges, next.nodes, next.edges)
			t.Logf("%-38s %-24s qualifies=%-5v decls=%d changed=%d added=%d removed=%d rows=%d/%d reasons=%v",
				rel, edit.name, d.Qualifies, d.Declarations, d.Changed, d.Added, d.Removed, d.ChangedRows, d.TotalRows, d.Reasons)
		}
	}
}

type replayEdit struct{ name, src string }

// replayEdits derives the ordinary edits of one file from its source.
func replayEdits(t *testing.T, src, target string) []replayEdit {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var fn, method *ast.FuncDecl
	var typ *ast.TypeSpec
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == target {
				fn = d
			}
			if d.Recv != nil && method == nil {
				method = d
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok && typ == nil {
					typ = ts
				}
			}
		}
	}
	lines := strings.Split(src, "\n")
	lineOf := func(p token.Pos) int { return fset.Position(p).Line - 1 }
	replaceOnLine := func(line int, old, repl string) string {
		out := append([]string(nil), lines...)
		if !strings.Contains(out[line], old) {
			return ""
		}
		out[line] = strings.Replace(out[line], old, repl, 1)
		return strings.Join(out, "\n")
	}
	var edits []replayEdit
	if fn != nil {
		l := lineOf(fn.Name.Pos())
		body := lineOf(fn.Body.Lbrace)
		edits = append(edits,
			replayEdit{"rename_function", replaceOnLine(l, "func "+target+"(", "func "+target+"Ren1(")},
			replayEdit{"body_edit_same_lines", replaceOnLine(body, "{", "{ println(\"edited\");")},
			replayEdit{"body_edit_add_line", replaceOnLine(body, "{", "{\n\tprintln(\"edited\")")},
			replayEdit{"changed_signature", replaceOnLine(l, "func "+target+"(", "func "+target+"(extra int, ")},
			replayEdit{"added_function_middle", strings.Join(append(append(append([]string(nil), lines[:lineOf(fn.Pos())]...), "func addedByReplay() int { return 1 }", ""), lines[lineOf(fn.Pos()):]...), "\n")},
		)
	}
	if method != nil {
		edits = append(edits, replayEdit{"rename_method", replaceOnLine(lineOf(method.Name.Pos()), " "+method.Name.Name+"(", " "+method.Name.Name+"Ren1(")})
	} else {
		edits = append(edits, replayEdit{"rename_method", ""})
	}
	if typ != nil {
		edits = append(edits, replayEdit{"rename_type", replaceOnLine(lineOf(typ.Name.Pos()), typ.Name.Name+" ", typ.Name.Name+"Ren1 ")})
	} else {
		edits = append(edits, replayEdit{"rename_type", ""})
	}
	edits = append(edits, replayEdit{"added_function_end", strings.TrimRight(src, "\n") + "\n\nfunc addedByReplay() int { return 1 }\n"})
	return edits
}

type replayFileRows struct {
	nodes []*graph.Node
	edges []*graph.Edge
}

// replayRows indexes rel's package at rev with rel's content replaced by
// src, and returns rel's rows.
func replayRows(t *testing.T, repo, rev, rel, src string) replayFileRows {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Dir(rel)
	out, err := exec.Command("git", "-C", repo, "ls-tree", "--name-only", rev+":"+dir).Output()
	if err != nil {
		t.Fatalf("ls-tree %s: %v", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		p := filepath.Join(dir, name)
		content := src
		if p != rel {
			content = gitShow(t, repo, rev, p)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := graph.New()
	idx := newTestIndexer(g)
	if _, err := idx.Index(root); err != nil {
		t.Fatalf("index: %v", err)
	}
	rows := replayFileRows{nodes: g.GetFileNodes(rel)}
	for _, e := range g.AllEdges() {
		if e != nil && e.FilePath == rel {
			rows.edges = append(rows.edges, e)
		}
	}
	if len(rows.nodes) == 0 {
		t.Fatalf("no rows for %s", rel)
	}
	return rows
}

func gitShow(t *testing.T, repo, rev, rel string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "show", fmt.Sprintf("%s:%s", rev, rel)).Output()
	if err != nil {
		t.Fatalf("git show %s: %v", rel, err)
	}
	return string(out)
}
