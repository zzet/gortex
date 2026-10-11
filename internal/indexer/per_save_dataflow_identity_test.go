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

// A whole index keys a dataflow edge (value_flow, arg_of) from the placeholder
// of the reference at its site and moves it to what that reference binds to,
// so its source always equals the reference's target. The primary per-save
// path must serve the same rows after every save: these tests compare its
// dataflow edges with a whole index of the edited tree.

const perSaveDataflowModule = "example.com/dataflow"

func perSaveDataflowTree() map[string]string {
	return map[string]string{
		"go.mod":      "module " + perSaveDataflowModule + "\n\ngo 1.22\n",
		"a/a.go":      "package a\n\nimport \"" + perSaveDataflowModule + "/b\"\n\n// A returns B.\nfunc A() int {\n\treturn b.B()\n}\n",
		"b/b.go":      "package b\n\nimport \"" + perSaveDataflowModule + "/c\"\n\n// B returns C plus a helper.\nfunc B() int {\n\treturn c.C() + Helper()\n}\n",
		"b/helper.go": "package b\n\n// Helper is a same-package helper.\nfunc Helper() int {\n\treturn 2\n}\n",
		"c/c.go":      "package c\n\n// C is the bottom.\nfunc C() int {\n\treturn 1\n}\n",
	}
}

// perSaveDataflowRows renders every dataflow edge of r, sorted.
func perSaveDataflowRows(r graph.Reader) []string {
	var rows []string
	for _, e := range r.AllEdges() {
		if e != nil && graph.PlaceholderSourceKind(e.Kind) {
			rows = append(rows, builderRenderEdge(e))
		}
	}
	slices.Sort(rows)
	return rows
}

func TestPerSaveDataflowSourcesMatchAWholeIndex(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		edit  map[string]string // "" removes the file
	}{
		// The signature change restubs A's call into B: B's value_flow
		// into A, recorded in a.go, follows it and is re-bound. b.go's own
		// calls are re-used from the prior resolution, and the dataflow
		// edges keyed from their placeholders take the re-used target.
		{"signature", []string{"b/b.go"}, map[string]string{
			"b/b.go": "package b\n\nimport \"" + perSaveDataflowModule + "/c\"\n\n// B returns C plus a helper.\nfunc B(delta int) int {\n\treturn c.C() + Helper() + delta\n}\n",
		}},
		// A body edit keeps C's identity and contract: C's value_flow into
		// B, recorded in b.go, is re-stated with the carried call.
		{"body", []string{"c/c.go"}, map[string]string{
			"c/c.go": "package c\n\n// C is the bottom.\nfunc C() int {\n\tv := 1\n\treturn v\n}\n",
		}},
		// Deleting Helper leaves B's call unresolved: its value_flow goes
		// back to the placeholder source a whole index keeps.
		{"delete", []string{"b/helper.go"}, map[string]string{
			"b/helper.go": "",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			builderIsolateGit(t)
			dir := builderTempDir(t, "repo")
			builderWriteTree(t, dir, perSaveDataflowTree())
			store := builderOpenStore(t, "per-save")
			idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			if _, err := idx.Index(dir); err != nil {
				t.Fatalf("index: %v", err)
			}
			for rel, body := range c.edit {
				full := filepath.Join(dir, filepath.FromSlash(rel))
				if body == "" {
					if err := os.Remove(full); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := idx.IncrementalReindexPaths(dir, c.paths); err != nil {
				t.Fatalf("per-save: %v", err)
			}
			clean := builderOpenStore(t, "clean")
			builderIndex(t, clean, dir)
			served, whole := perSaveDataflowRows(store), perSaveDataflowRows(clean)
			if !slices.Equal(served, whole) {
				t.Errorf("per-save dataflow edges differ from a whole index\nper-save:\n  %s\nwhole index:\n  %s",
					strings.Join(served, "\n  "), strings.Join(whole, "\n  "))
			}
			if len(whole) == 0 {
				t.Fatal("the fixture must produce dataflow edges")
			}
		})
	}
}
