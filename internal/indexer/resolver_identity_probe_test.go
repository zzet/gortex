package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestResolverIdentityProbeInvalidationClasses prints, for each invalidation
// class, the edge rows where the primary per-save path differs from a whole
// index of the same tree. Diagnostic only (GX_RESOLVER_IDENTITY_PROBE=1).
func TestResolverIdentityProbeInvalidationClasses(t *testing.T) {
	if os.Getenv("GX_RESOLVER_IDENTITY_PROBE") != "1" {
		t.Skip("diagnostic probe; set GX_RESOLVER_IDENTITY_PROBE=1")
	}
	mod := accumulatedDirtyModule
	cases := []struct {
		name  string
		paths []string
		edit  func(r *invalidationRun)
	}{
		{"signature", []string{"chain/b/b.go"}, func(r *invalidationRun) {
			r.write("chain/b/b.go", "package b\n\nimport \""+mod+"/chain/c\"\n\n// B calls down the chain.\nfunc B(delta int) int {\n\treturn c.C() + delta\n}\n")
		}},
		{"import", []string{"consumer/consumer.go"}, func(r *invalidationRun) {
			r.write("consumer/consumer.go", "package consumer\n\nimport \""+mod+"/chain/b\"\n\n// Consume is the chain's one importing consumer.\nfunc Consume() int {\n\treturn b.B()\n}\n")
		}},
		{"delete", []string{"chain/d/d.go"}, func(r *invalidationRun) {
			r.write("chain/d/d.go", invalidationDirtyD)
			r.remove("chain/d/d.go")
		}},
		{"rename", []string{"chain/d/d.go", "chain/e/e.go", "chain/c/c.go"}, func(r *invalidationRun) {
			r.remove("chain/d/d.go")
			r.write("chain/e/e.go", strings.Replace(invalidationDirtyD, "package d", "package e", 1))
			r.write("chain/c/c.go", "package c\n\nimport \""+mod+"/chain/e\"\n\n// C calls down the chain.\nfunc C() int {\n\treturn e.D() + 1\n}\n")
		}},
		{"undo", []string{"chain/b/b.go"}, func(r *invalidationRun) {
			r.write("chain/b/b.go", invalidationCommitted(t, "chain/b/b.go"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newInvalidationRun(t, "probe-"+c.name, 1)
			c.edit(r)
			dirty := append([]string(nil), c.paths...)
			for p := range r.accumulated {
				dirty = append(dirty, p)
			}
			primary := primaryPerSaveOf(t, r.repoDir, dirty, config.Default().Index)
			clean := builderOpenStore(t, "clean-probe-"+c.name)
			builderIndex(t, clean, r.repoDir)
			p := builderRenderEdges(primary.AllEdges())
			f := builderRenderEdges(clean.AllEdges())
			pc, fc := map[string]int{}, map[string]int{}
			for _, r := range p {
				pc[r]++
			}
			for _, r := range f {
				fc[r]++
			}
			for r, n := range pc {
				if fc[r] != n {
					t.Logf("primary-only(%d/%d) %s", n, fc[r], strings.ReplaceAll(r, "\n", " "))
				}
			}
			for r, n := range fc {
				if pc[r] != n {
					t.Logf("clean-only(%d/%d) %s", n, pc[r], strings.ReplaceAll(r, "\n", " "))
				}
			}
		})
	}
}

// TestResolverIdentityProbeStdlibSpelling prints the stdlib/dep rows of a
// whole index and of a per-save of the same tree. Diagnostic only.
func TestResolverIdentityProbeStdlibSpelling(t *testing.T) {
	if os.Getenv("GX_RESOLVER_IDENTITY_PROBE") != "1" {
		t.Skip("diagnostic probe; set GX_RESOLVER_IDENTITY_PROBE=1")
	}
	builderIsolateGit(t)
	tree := map[string]string{
		"go.mod": "module example.com/sp\n\ngo 1.22\n\nrequire github.com/spf13/viper v1.21.0\n",
		"a/a.go": "package a\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\n\t\"github.com/spf13/viper\"\n)\n\n// A errs.\nfunc A(s string) error {\n\t_ = viper.New()\n\treturn fmt.Errorf(\"x %s\", strings.TrimSpace(s))\n}\n\n// Ok is a flag.\nfunc Ok(b bool) bool { return b }\n\n// B errs plainly.\nfunc B() error {\n\treturn fmt.Errorf(\"y\")\n}\n",
	}
	dir := builderTempDir(t, "repo")
	builderWriteTree(t, dir, tree)
	store := builderOpenStore(t, "per-save")
	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	defer idx.Close()
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if _, err := idx.Index(dir); err != nil {
		t.Fatal(err)
	}
	dump := func(label string, r graph.Reader) map[string]bool {
		out := map[string]bool{}
		for _, e := range r.AllEdges() {
			if strings.Contains(e.From+e.To, "viper") || strings.Contains(e.From+e.To, "stdlib") || strings.Contains(e.From+e.To, "dep::") || strings.Contains(e.From+e.To, "builtin") || e.Kind == graph.EdgeImports || e.Kind == graph.EdgeDependsOnModule {
				out[builderRenderEdge(e)] = true
			}
		}
		return out
	}
	whole := dump("whole", store)
	full := filepath.Join(dir, "a/a.go")
	src, _ := os.ReadFile(full)
	_ = os.WriteFile(full, []byte(strings.Replace(string(src), "\t_ = viper.New()\n", "\t_ = viper.New()\n\t_ = 1\n", 1)), 0o644)
	if _, err := idx.IncrementalReindexPaths(dir, []string{"a/a.go"}); err != nil {
		t.Fatal(err)
	}
	clean := builderOpenStore(t, "clean")
	builderIndex(t, clean, dir)
	per, cl := dump("per", store), dump("clean", clean)
	for r := range per {
		if !cl[r] {
			t.Logf("per-save-only %s", r)
		}
	}
	for r := range cl {
		if !per[r] {
			t.Logf("clean-only    %s", r)
		}
	}
	for r := range whole {
		t.Logf("whole(before) %s", r)
	}
	for _, id := range []string{builderRepoPrefix + "/a/a.go::import::github.com/spf13/viper"} {
		t.Logf("node %s per-save=%v clean=%v", id, store.GetNode(id) != nil, clean.GetNode(id) != nil)
	}
}

// primaryPerSaveOf indexes HEAD of repoDir whole into a fresh store, brings a
// scratch checkout to the working tree's bytes at paths, and applies them
// through the primary per-save path.
func primaryPerSaveOf(t *testing.T, repoDir string, paths []string, cfg config.IndexConfig) *store_sqlite.Store {
	t.Helper()
	scratch := builderTempDir(t, "primary")
	builderGit(t, repoDir, "worktree", "add", "--detach", scratch, "HEAD")
	store := builderOpenStore(t, "primary")
	idx := New(store, builderRegistry(), cfg, zap.NewNop())
	t.Cleanup(func() { idx.Close() })
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	if _, err := idx.Index(scratch); err != nil {
		t.Fatalf("primary index: %v", err)
	}
	for _, rel := range paths {
		src := filepath.Join(repoDir, filepath.FromSlash(rel))
		dst := filepath.Join(scratch, filepath.FromSlash(rel))
		data, err := os.ReadFile(src)
		switch {
		case err == nil:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dst, err)
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				t.Fatalf("write %s: %v", dst, err)
			}
		case os.IsNotExist(err):
			_ = os.Remove(dst)
		default:
			t.Fatalf("read %s: %v", src, err)
		}
	}
	if _, err := idx.IncrementalReindexPaths(scratch, paths); err != nil {
		t.Fatalf("primary per-save: %v", err)
	}
	return store
}
