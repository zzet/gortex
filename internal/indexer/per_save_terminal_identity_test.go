package indexer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// External terminals after a per-save must be the whole index's rows: one stub
// spelling per stdlib symbol, an import of a dependency kept on its `dep::`
// terminal with the import node's depends_on_module edge, and no reference
// bound to a stub the attribution passes minted after the whole index had
// resolved everything.
func perSaveTerminalTree() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/term\n\ngo 1.22\n\nrequire github.com/spf13/viper v1.21.0\n",
		"a/a.go": "package a\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\n\t\"github.com/spf13/viper\"\n)\n\n// A errs.\nfunc A(s string) error {\n\t_ = viper.New()\n\treturn fmt.Errorf(\"x %s\", strings.TrimSpace(s))\n}\n\n// B errs plainly.\nfunc B() error {\n\treturn fmt.Errorf(\"y\")\n}\n",
		"b/b.go": "package b\n\n// Ok is a flag.\nfunc Ok(b bool) bool {\n\treturn b\n}\n",
		"c/c.go": "package c\n\nimport \"C\"\n\n// Flag converts through cgo.\nfunc Flag() {\n\t_ = C.bool(true)\n}\n",
	}
}

// perSaveTerminalRows renders every edge that touches an external terminal,
// an import or a module link.
func perSaveTerminalRows(r graph.Reader) []string {
	var rows []string
	for _, e := range r.AllEdges() {
		if e == nil {
			continue
		}
		ends := e.From + " " + e.To
		if e.Kind == graph.EdgeImports || e.Kind == graph.EdgeDependsOnModule ||
			strings.Contains(ends, "stdlib::") || strings.Contains(ends, "dep::") ||
			strings.Contains(ends, "builtin::") || strings.Contains(ends, "external-call::") {
			rows = append(rows, builderRenderEdge(e))
		}
	}
	slices.Sort(rows)
	return rows
}

func TestPerSaveExternalTerminalsMatchAWholeIndex(t *testing.T) {
	for _, edit := range []struct{ path, from, to string }{
		// the edited file's call moves a line: its dataflow sites move with it
		{"a/a.go", "\t_ = viper.New()\n", "\t_ = viper.New()\n\t_ = 1\n"},
		// a file whose `bool` references must stay on the builtin type
		{"b/b.go", "\treturn b\n", "\t_ = 1\n\treturn b\n"},
	} {
		t.Run(edit.path, func(t *testing.T) {
			builderIsolateGit(t)
			dir := builderTempDir(t, "repo")
			builderWriteTree(t, dir, perSaveTerminalTree())
			store := builderOpenStore(t, "per-save")
			idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			if _, err := idx.Index(dir); err != nil {
				t.Fatalf("index: %v", err)
			}
			full := filepath.Join(dir, filepath.FromSlash(edit.path))
			src, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			edited := strings.Replace(string(src), edit.from, edit.to, 1)
			if edited == string(src) {
				t.Fatalf("the edit of %s did not apply", edit.path)
			}
			if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := idx.IncrementalReindexPaths(dir, []string{edit.path}); err != nil {
				t.Fatalf("per-save: %v", err)
			}
			clean := builderOpenStore(t, "clean")
			builderIndex(t, clean, dir)
			served, whole := perSaveTerminalRows(store), perSaveTerminalRows(clean)
			if !slices.Equal(served, whole) {
				t.Errorf("per-save terminal rows differ from a whole index\nper-save:\n  %s\nwhole index:\n  %s",
					strings.Join(served, "\n  "), strings.Join(whole, "\n  "))
			}
			for _, row := range whole {
				if strings.Contains(row, "From:stdlib::") || strings.Contains(row, "To:stdlib::") {
					t.Errorf("a whole index spells a stdlib stub without its repository: %s", row)
				}
			}
		})
	}
}
