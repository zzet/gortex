package indexer

import (
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
)

func TestImportPlacementRepoRootCarriesMatchingNestedFolder(t *testing.T) {
	g := graph.New()
	for _, file := range []string{"repo/app/main.ts", "repo/index.ts", "repo/repo/index.ts"} {
		g.AddNode(&graph.Node{
			ID: file, Kind: graph.KindFile, FilePath: file,
			Language: "typescript", RepoPrefix: "repo",
		})
	}
	edge := &graph.Edge{
		From: "repo/app/main.ts", To: "unresolved::import::repo",
		Kind: graph.EdgeImports, FilePath: "repo/app/main.ts", Line: 1,
	}
	g.AddEdge(edge)
	resolver.New(g).ResolveAll()
	if edge.To != "repo/repo/index.ts" {
		t.Fatalf("import bound to %q, want matching nested folder", edge.To)
	}

	walk := &closureWalk{req: BuildRequest{Base: g, RepoPrefix: "repo"}}
	assertImportPlacement(t, walk, "repo", []string{"app/main.ts"}, []string{"repo/index.ts", "repo/repo/index.ts"})
}

// A nested Go module can own the prefix-named package even with a file at the
// outer repository root. Certified ownership must keep that valid binding.
func TestImportPlacementRepoRootCertifiedNested(t *testing.T) {
	g := graph.New()
	for _, file := range []string{"repo/app/main.go", "repo/root.go", "repo/repo/pkg.go"} {
		g.AddNode(&graph.Node{
			ID: file, Kind: graph.KindFile, FilePath: file,
			Language: "go", RepoPrefix: "repo",
		})
	}
	edge := &graph.Edge{From: "repo/app/main.go", To: "unresolved::import::repo",
		Kind: graph.EdgeImports, FilePath: "repo/app/main.go", Line: 3}
	g.AddEdge(edge)
	r := resolver.New(g)
	r.SetGoPackageOwnership(func(c resolver.GoImportCandidate) resolver.GoPackageOwnershipResult {
		if c.CandidateFilePath == "repo/repo/pkg.go" {
			return resolver.GoPackageOwnershipExact
		}
		return resolver.GoPackageOwnershipDifferent
	})
	r.ResolveAll()
	if edge.To != "repo/repo/pkg.go" {
		t.Fatalf("certified import bound to %q, want nested package", edge.To)
	}
	walk := &closureWalk{req: BuildRequest{Base: g, RepoPrefix: "repo"}}
	assertImportPlacement(t, walk, "repo", []string{"app/main.go"},
		[]string{"repo/root.go", "repo/repo/pkg.go"})
}

func TestImportPlacementSameRepoRanking(t *testing.T) {
	for _, tc := range []struct {
		name, language, caller, spec, direct, fallback, directLanguage string
		ownership                                                      bool
	}{
		{"ownership", "go", "app/main.go", "repo/config", "config/unknown.go", "backend/config/config.go", "go", true},
		{"language", "go", "app/main.go", "repo/config", "config/settings.py", "repo/config/config.go", "python", false},
		{"workspace", "typescript", "w1/app/main.ts", "repo/button", "button/index.ts", "repo/button/index.ts", "typescript", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			caller, direct, fallback := "repo/"+tc.caller, "repo/"+tc.direct, "repo/"+tc.fallback
			for _, f := range []struct{ path, language string }{{caller, tc.language}, {direct, tc.directLanguage}, {fallback, tc.language}} {
				g.AddNode(&graph.Node{ID: f.path, Kind: graph.KindFile, FilePath: f.path, Language: f.language, RepoPrefix: "repo"})
			}
			e := &graph.Edge{From: caller, To: "unresolved::import::" + tc.spec, Kind: graph.EdgeImports, FilePath: caller, Line: 3}
			g.AddEdge(e)
			r := resolver.New(g)
			if tc.ownership {
				r.SetGoPackageOwnership(func(c resolver.GoImportCandidate) resolver.GoPackageOwnershipResult {
					if c.CandidateFilePath == fallback {
						return resolver.GoPackageOwnershipExact
					}
					return resolver.GoPackageOwnershipUnknown
				})
			}
			if tc.name == "workspace" {
				r.SetWorkspaceMembership(func(file string) string {
					if strings.HasPrefix(file, "repo/w1/") || strings.HasPrefix(file, "repo/repo/") {
						return "w1"
					}
					return ""
				})
			}
			r.ResolveAll()
			if e.To != fallback {
				t.Fatalf("bound %q, want %q", e.To, fallback)
			}
			walk := &closureWalk{req: BuildRequest{Base: g, RepoPrefix: "repo"}}
			assertImportPlacement(t, walk, tc.spec, []string{tc.caller}, []string{direct, fallback})
		})
	}
}

func TestResolveImportNestedGoModuleMixedFolders(t *testing.T) {
	for _, prefix := range []string{"repo", ""} {
		for _, tc := range []struct {
			name, otherFile, otherBody, manifest string
		}{
			{"yaml", "config/app.yaml", "server:\n  port: 8080\n", ""},
			{"python", "internal/config/tool.py", "DEBUG = True\n", ""},
			{"different-go-module", "config/other.go", "package config\n\nfunc Other() {}\n", "module github.com/acme/other\n\ngo 1.23\n"},
			{"unknown-go-module", "config/unknown.go", "package config\n\nfunc Other() {}\n", "module [invalid\n"},
		} {
			t.Run(prefix+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				files := map[string]string{
					"backend/go.mod":                    "module github.com/acme/app\n\ngo 1.23\n",
					"backend/cmd/app/main.go":           "package main\n\nimport \"github.com/acme/app/internal/config\"\n\nfunc main() { config.Load() }\n",
					"backend/internal/config/config.go": "package config\n\nfunc Load() {}\n",
					tc.otherFile:                        tc.otherBody,
				}
				if tc.manifest != "" {
					files["config/go.mod"] = tc.manifest
				}
				builderWriteTree(t, dir, files)
				g := graph.New()
				reg := parser.NewRegistry()
				languages.RegisterAll(reg)
				cfg := config.Default().Index
				cfg.Workers = 2
				idx := New(g, reg, cfg, zap.NewNop())
				idx.SetRepoPrefix(prefix)
				if _, err := idx.Index(dir); err != nil {
					t.Fatal(err)
				}
				idx.ResolveAll()
				caller, want := "backend/cmd/app/main.go", "backend/internal/config/config.go"
				if prefix != "" {
					caller, want = prefix+"/"+caller, prefix+"/"+want
				}
				imports := 0
				for _, edge := range g.GetOutEdges(caller) {
					if edge.Kind != graph.EdgeImports {
						continue
					}
					imports++
					if edge.To != want {
						t.Errorf("import bound %q, want %q", edge.To, want)
					}
				}
				if imports != 1 {
					t.Fatalf("got %d imports, want one", imports)
				}
			})
		}
	}
}
