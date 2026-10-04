package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

const hierarchyFrontierBase = "package pkg\n\ntype Runner interface {\n\tRun() int\n}\n\ntype Job struct{}\n\nfunc (Job) Run() int {\n\treturn 1\n}\n\ntype Idle struct{}\n\nfunc helper() int {\n\treturn 2\n}\n"

func hierarchyImplements(r graph.Reader) []string {
	var rows []string
	for e := range r.EdgesByKind(graph.EdgeImplements) {
		if e != nil {
			rows = append(rows, e.From+" -> "+e.To)
		}
	}
	sort.Strings(rows)
	return rows
}

// A save that renames a free function moves no type's hierarchy: the
// inferred implements/overrides rows are republished, not re-inferred over the
// repository's interfaces. A save that gives a type the method an interface
// requires re-infers, and the implements edge lands as in a whole index.
func TestPerSaveHierarchyFrontierOnlyWhenTypesMove(t *testing.T) {
	cases := []struct {
		name         string
		edited       string
		wantInferred bool
	}{
		{"rename_function", strings.Replace(hierarchyFrontierBase, "func helper()", "func helperRen()", 1), false},
		{"method_added", hierarchyFrontierBase + "\nfunc (Idle) Run() int {\n\treturn 3\n}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builderIsolateGit(t)
			dir := builderTempDir(t, "hier-"+tc.name)
			builderWriteTree(t, dir, map[string]string{
				"go.mod":     "module example.com/hier\n\ngo 1.22\n",
				"pkg/job.go": hierarchyFrontierBase,
			})
			store := builderOpenStore(t, "hier-"+tc.name)
			builderIndex(t, store, dir)
			core, logs := observer.New(zap.InfoLevel)
			idx := New(store, builderRegistry(), config.Default().Index, zap.New(core))
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			idx.rootPath = dir
			path := filepath.Join(dir, "pkg", "job.go")
			if err := os.WriteFile(path, []byte(tc.edited), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := idx.IndexFile(path); err != nil {
				t.Fatalf("IndexFile: %v", err)
			}
			republished := int64(-1)
			for _, entry := range logs.All() {
				if entry.Message == "incremental derived passes complete" {
					republished, _ = entry.ContextMap()["hierarchy_republished"].(int64)
				}
			}
			if republished < 0 {
				t.Fatal("no derived pass ran")
			}
			if inferred := republished == 0; inferred != tc.wantInferred {
				t.Fatalf("republished %d hierarchy rows; want the inference to run: %v", republished, tc.wantInferred)
			}
			clean := builderOpenStore(t, "hier-clean-"+tc.name)
			builderIndex(t, clean, dir)
			served, want := hierarchyImplements(store), hierarchyImplements(clean)
			if strings.Join(served, "\n") != strings.Join(want, "\n") {
				t.Fatalf("implements after the save differ from a whole index\n served: %v\n clean:  %v", served, want)
			}
			if tc.wantInferred && len(want) < 2 {
				t.Fatalf("the fixture must infer both implementations: %v", want)
			}
		})
	}
}
