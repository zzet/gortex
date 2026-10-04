package indexer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
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

// TestResolverIdentityProbeRealRepoPayload applies one body edit of each file
// in GX_RESOLVER_IDENTITY_REAL_FILES to a copy of GX_RESOLVER_IDENTITY_REAL_STORE
// (a whole index of the clone at GX_RESOLVER_IDENTITY_REAL_TREE) through the
// per-file delta path and prints, for every edge-source marker the generation
// carries, the rows by which the source's served out-set differs from the
// base: the rows the edit actually changed. Diagnostic only.
func TestResolverIdentityProbeRealRepoPayload(t *testing.T) {
	tree := os.Getenv("GX_RESOLVER_IDENTITY_REAL_TREE")
	storePath := os.Getenv("GX_RESOLVER_IDENTITY_REAL_STORE")
	if tree == "" || storePath == "" {
		t.Skip("GX_RESOLVER_IDENTITY_REAL_TREE / _STORE not set")
	}
	builderIsolateGit(t)
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	editCfg := cfg.Index
	files := strings.Split(os.Getenv("GX_RESOLVER_IDENTITY_REAL_FILES"), ",")
	for _, rel := range files {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		t.Run(strings.ReplaceAll(rel, "/", "_"), func(t *testing.T) {
			full := filepath.Join(tree, filepath.FromSlash(rel))
			original, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(full, original, 0o644) })
			copyPath := filepath.Join(t.TempDir(), "store.sqlite")
			if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
				t.Fatalf("clone store: %v %s", err, out)
			}
			store := builderOpenStoreAt(t, copyPath)
			t.Cleanup(func() { _ = store.Close() })
			logger := zap.NewNop()
			if os.Getenv("GX_RESOLVER_IDENTITY_LOG") != "" {
				t.Setenv("GX_DELTA_REAL_LOG", os.Getenv("GX_RESOLVER_IDENTITY_LOG"))
				logger = editDeltaRealLogger(t)
			}
			builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: editCfg, Logger: logger}
			chains := newDirtyChainBuilder(t, builder, store, tree, true)
			sampler, err := gitstate.NewDirtySampler(tree, "", "")
			if err != nil {
				t.Fatal(err)
			}
			chains.sampler = sampler
			edited, ok := realRepositoryInsertBodyStatementN(string(original), 1)
			if !ok {
				t.Fatalf("no body in %s", rel)
			}
			if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			recordLastEditDelta(nil)
			started := time.Now()
			id, _, chain := chains.build()
			t.Logf("build %s: gen=%d %s", rel, id, time.Since(started))
			rep := LastEditDeltaReport()
			if rep == nil {
				t.Fatalf("not built by the delta path")
			}
			t.Logf("delta: covered=%d claimed=%d payload=%d/%d markers=%d tombstones=%d",
				rep.CoveredPaths, rep.ClaimedSources, rep.PayloadNodes, rep.PayloadEdges, rep.EdgeSources, rep.Tombstones)
			db := parityOpenRaw(t, store)
			rows, err := db.Query(`SELECT source_id FROM generation_edge_sources WHERE view_gen = ? ORDER BY source_id`, id)
			if err != nil {
				t.Fatal(err)
			}
			var sources []string
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				sources = append(sources, s)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			composed := dirtyChainComposed(t, store, chain)
			graphPath := builderRepoPrefix + "/" + rel
			beforeNodes := store.GetFileNodes(graphPath)
			afterNodes := composed.GetFileNodes(graphPath)
			sb, sa := resolver.DeclarationSurfaceOf(beforeNodes), resolver.DeclarationSurfaceOf(afterNodes)
			for key, digest := range sa {
				if sb[key] != digest {
					t.Logf("surface key changed: %s", key)
					if len(sb[key]) < 4000 {
						t.Logf("   before %q", sb[key])
						t.Logf("   after  %q", digest)
					}
				}
			}
			kinds := map[string]int{}
			for _, src := range sources {
				served := builderRenderEdges(composed.GetOutEdges(src))
				base := builderRenderEdges(store.GetOutEdges(src))
				sc, bc := map[string]int{}, map[string]int{}
				for _, r := range served {
					sc[r]++
				}
				for _, r := range base {
					bc[r]++
				}
				changed := 0
				for r, n := range sc {
					if bc[r] != n {
						changed++
						if changed <= 3 {
							t.Logf("  %s served-only %s", src, r)
						}
					}
				}
				for r, n := range bc {
					if sc[r] != n {
						changed++
						if changed <= 6 {
							t.Logf("  %s base-only   %s", src, r)
						}
					}
				}
				for _, e := range composed.GetOutEdges(src) {
					if e.FilePath != graphPath {
						t.Logf("  %s served elsewhere: %s", src, builderRenderEdge(e))
					}
				}
				for _, e := range store.GetOutEdges(src) {
					if e.FilePath != graphPath {
						t.Logf("  %s base elsewhere:   %s", src, builderRenderEdge(e))
					}
				}
				kinds[fmt.Sprintf("out=%d changed=%d", len(served), changed)]++
				t.Logf("marker %s: served out=%d base out=%d changed rows=%d", src, len(served), len(base), changed)
			}
			t.Logf("markers=%d %v", len(sources), kinds)
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

// realRepositoryInsertBodyStatementN adds `_ = n` as the first statement of
// the first function whose signature ends its line with an opening brace. The
// edit changes no declaration, import or signature: a body edit.
func realRepositoryInsertBodyStatementN(src string, n int) (string, bool) {
	lines := strings.SplitAfter(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "func ") && strings.HasSuffix(trimmed, "{") {
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:i+1]...)
			out = append(out, "\t_ = "+strconv.Itoa(n)+"\n")
			out = append(out, lines[i+1:]...)
			return strings.Join(out, ""), true
		}
	}
	return "", false
}
