package resolver

import (
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

// Keep candidate order explicit, including in the no-index scan.
type orderedImportFileStore struct {
	graph.Store
	files []graph.FileNodeIdentity
}

func (s orderedImportFileStore) FileNodeIdentitiesSeq(_ []string) iter.Seq[graph.FileNodeIdentity] {
	return func(yield func(graph.FileNodeIdentity) bool) {
		for _, file := range s.files {
			if !yield(file) {
				return
			}
		}
	}
}

func sameRepoImportFixture(importPath, mode string) (*Resolver, *graph.Edge) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "src/main.go", Kind: graph.KindFile, Name: "main.go",
		FilePath: "src/main.go", Language: "go", RepoPrefix: "src",
	})
	files := []graph.FileNodeIdentity{
		{ID: "src/a/config/config.go", FilePath: "src/a/config/config.go", RepoPrefix: "src"},
		{ID: "src/b/config/config.go", FilePath: "src/b/config/config.go", RepoPrefix: "src"},
	}
	for _, file := range files {
		g.AddNode(&graph.Node{
			ID: file.ID, Kind: graph.KindFile, Name: "config.go",
			FilePath: file.FilePath, Language: "go", RepoPrefix: file.RepoPrefix,
		})
	}
	r := New(orderedImportFileStore{Store: g, files: files})
	switch mode {
	case "dir-index":
		r.dirIndex = map[string][]graph.FileNodeIdentity{importPath: files}
	case "last-dir-index":
		r.dirIndex = map[string][]graph.FileNodeIdentity{}
		r.lastDirIndex = map[string][]graph.FileNodeIdentity{"config": files}
	}
	e := &graph.Edge{
		From: "src/main.go", To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: "src/main.go", Line: 3,
	}
	return r, e
}

func TestResolveImportSameRepoPrefersMatchingPath(t *testing.T) {
	for _, mode := range []string{"dir-index", "last-dir-index", "full-scan"} {
		t.Run(mode, func(t *testing.T) {
			const importPath = "example.com/module/b/config"
			r, e := sameRepoImportFixture(importPath, mode)
			stats := &ResolveStats{}
			r.resolveImport(e, importPath, stats)
			assert.Equal(t, "src/b/config/config.go", e.To)
			assert.Equal(t, 1, stats.Resolved)
		})
	}
}

func TestResolveImportSameRepoNoMatchKeepsToday(t *testing.T) {
	for _, mode := range []string{"dir-index", "last-dir-index", "full-scan"} {
		t.Run(mode, func(t *testing.T) {
			const importPath = "example.com/module/c/config"
			r, e := sameRepoImportFixture(importPath, mode)
			stats := &ResolveStats{}
			r.resolveImport(e, importPath, stats)
			assert.Equal(t, "src/a/config/config.go", e.To, "keep the first candidate when no folder matches")
			assert.Equal(t, 1, stats.Resolved)
		})
	}

	for _, tc := range []struct {
		name       string
		importPath string
		localFile  string
		dependency string
	}{
		{
			name:       "declared-sibling-module",
			importPath: "example.com/billing/internal/config",
			localFile:  "src/services/billing/internal/config/config.go",
			dependency: "example.com/billing",
		},
		{
			name:       "dot-less-nested-module",
			importPath: "api/internal/config",
			localFile:  "src/backend/internal/config/config.go",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{
				ID: "src/main.go", Kind: graph.KindFile, Name: "main.go",
				FilePath: "src/main.go", Language: "go", RepoPrefix: "src",
			})
			g.AddNode(&graph.Node{
				ID: tc.localFile, Kind: graph.KindFile, Name: "config.go",
				FilePath: tc.localFile, Language: "go", RepoPrefix: "src",
			})
			if tc.dependency != "" {
				addDepNode(t, g, "src", tc.dependency)
			}
			e := &graph.Edge{
				From: "src/main.go", To: "unresolved::import::" + tc.importPath,
				Kind: graph.EdgeImports, FilePath: "src/main.go", Line: 3,
			}
			g.AddEdge(e)

			stats := New(g).ResolveAll()

			assert.Equal(t, tc.localFile, e.To, "keep the local binding when no folder matches")
			assert.Equal(t, 1, stats.Resolved)
		})
	}
}

func TestResolveImportRepoRootPrefersMatchingPath(t *testing.T) {
	for _, mode := range []string{"dir-index", "full-scan", "full-scan-reversed"} {
		for _, ownership := range []string{"unknown", "certified-root", "no-root"} {
			t.Run(mode+"/"+ownership, func(t *testing.T) {
				g := graph.New()
				g.AddNode(&graph.Node{
					ID: "repo/app/main.go", Kind: graph.KindFile,
					FilePath: "repo/app/main.go", Language: "go", RepoPrefix: "repo",
				})
				files := []graph.FileNodeIdentity{
					{ID: "repo/root.go", FilePath: "repo/root.go", RepoPrefix: "repo"},
					{ID: "repo/repo/pkg.go", FilePath: "repo/repo/pkg.go", RepoPrefix: "repo"},
				}
				if ownership == "no-root" {
					files = files[1:]
				}
				for _, file := range files {
					g.AddNode(&graph.Node{ID: file.ID, Kind: graph.KindFile,
						FilePath: file.FilePath, Language: "go", RepoPrefix: "repo"})
				}
				if mode == "full-scan-reversed" && len(files) == 2 {
					files[0], files[1] = files[1], files[0]
				}
				r := New(orderedImportFileStore{Store: g, files: files})
				if mode == "dir-index" {
					r.buildDirIndexes()
				}
				if ownership == "certified-root" {
					r.SetGoPackageOwnership(func(candidate GoImportCandidate) GoPackageOwnershipResult {
						if candidate.CandidateFilePath == "repo/root.go" {
							return GoPackageOwnershipExact
						}
						return GoPackageOwnershipDifferent
					})
				}
				e := &graph.Edge{From: "repo/app/main.go", To: "unresolved::import::repo",
					Kind: graph.EdgeImports, FilePath: "repo/app/main.go", Line: 3}
				stats := &ResolveStats{}
				r.resolveImport(e, "repo", stats)
				switch ownership {
				case "unknown":
					assert.Equal(t, "repo/repo/pkg.go", e.To)
					assert.Equal(t, 1, stats.Resolved)
				case "certified-root":
					assert.Equal(t, "repo/root.go", e.To)
					assert.Equal(t, 1, stats.Resolved)
				case "no-root":
					assert.Equal(t, "repo/repo/pkg.go", e.To)
					assert.Equal(t, 1, stats.Resolved)
				}
			})
		}
	}
}

// Ownership is stronger than a repo-relative suffix: a nested module's import
// path does not include the directory that contains its go.mod.
func TestResolveImportSameRepoOwnershipAndLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, callerLanguage, callerFile, importPath, correct, other, otherLanguage string
		otherOwnership                                                              GoPackageOwnershipResult
	}{
		{"nested-go-yaml", "go", "backend/cmd/app/main.go", "github.com/acme/app/internal/config", "backend/internal/config/config.go", "config/app.yaml", "yaml", GoPackageOwnershipUnknown},
		{"nested-go-python", "go", "backend/cmd/app/main.go", "github.com/acme/app/internal/config", "backend/internal/config/config.go", "internal/config/tool.py", "python", GoPackageOwnershipUnknown},
		{"exact-before-unknown-go", "go", "backend/cmd/app/main.go", "github.com/acme/app/internal/config", "backend/internal/config/config.go", "config/unknown.go", "go", GoPackageOwnershipUnknown},
		{"nested-go-other-module", "go", "backend/cmd/app/main.go", "github.com/acme/app/internal/config", "backend/internal/config/config.go", "config/other.go", "go", GoPackageOwnershipDifferent},
		{"typescript", "typescript", "app/main.ts", "b/config", "b/config/index.ts", "a/config/index.ts", "typescript", GoPackageOwnershipUnknown},
		{"python", "python", "app/main.py", "config", "config/settings.py", "a/config/settings.py", "python", GoPackageOwnershipUnknown},
	} {
		for _, prefix := range []string{"", "repo"} {
			for _, provider := range []bool{false, true} {
				for _, collect := range []bool{false, true} {
					for _, mode := range []string{"dir-index", "last-dir-index", "full-scan"} {
						for _, reverse := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/prefix=%s/provider=%v/collect=%v/%s/reverse=%v", tc.name, prefix, provider, collect, mode, reverse), func(t *testing.T) {
								qualify := func(file string) string {
									if prefix == "" {
										return file
									}
									return prefix + "/" + file
								}
								caller, correct, other := qualify(tc.callerFile), qualify(tc.correct), qualify(tc.other)
								g := graph.New()
								g.AddNode(&graph.Node{ID: caller, Kind: graph.KindFile, FilePath: caller, Language: tc.callerLanguage, RepoPrefix: prefix})
								g.AddNode(&graph.Node{ID: correct, Kind: graph.KindFile, FilePath: correct, Language: tc.callerLanguage, RepoPrefix: prefix})
								g.AddNode(&graph.Node{ID: other, Kind: graph.KindFile, FilePath: other, Language: tc.otherLanguage, RepoPrefix: prefix})
								files := []graph.FileNodeIdentity{{ID: correct, FilePath: correct, RepoPrefix: prefix}, {ID: other, FilePath: other, RepoPrefix: prefix}}
								if reverse {
									files[0], files[1] = files[1], files[0]
								}
								r := New(orderedImportFileStore{Store: g, files: files})
								switch mode {
								case "dir-index":
									r.dirIndex = map[string][]graph.FileNodeIdentity{tc.importPath: files}
								case "last-dir-index":
									r.dirIndex = map[string][]graph.FileNodeIdentity{}
									r.lastDirIndex = map[string][]graph.FileNodeIdentity{"config": files}
								}
								if provider {
									r.SetGoPackageOwnership(func(c GoImportCandidate) GoPackageOwnershipResult {
										if c.CandidateFilePath == correct {
											return GoPackageOwnershipExact
										}
										return tc.otherOwnership
									})
								}
								if collect {
									r.SetWorkspaceMembership(func(string) string { return "" })
								}
								e := &graph.Edge{From: caller, To: "unresolved::import::" + tc.importPath, Kind: graph.EdgeImports, FilePath: caller, Line: 3}
								stats := &ResolveStats{}
								r.resolveImport(e, tc.importPath, stats)
								want := correct
								if !provider && (tc.otherLanguage == "go" ||
									(tc.otherLanguage != tc.callerLanguage && reverse)) {
									want = other // no ownership/matching folder: retain the first fallback
								}
								assert.Equal(t, want, e.To)
								assert.Equal(t, 1, stats.Resolved)
							})
						}
					}
				}
			}
		}
	}
}

func TestResolveImportSameRepoDifferentLanguageFallback(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/main.go", Kind: graph.KindFile, FilePath: "repo/main.go", Language: "go", RepoPrefix: "repo"})
	g.AddNode(&graph.Node{ID: "repo/config/settings.py", Kind: graph.KindFile, FilePath: "repo/config/settings.py", Language: "python", RepoPrefix: "repo"})
	e := &graph.Edge{From: "repo/main.go", To: "unresolved::import::config", Kind: graph.EdgeImports, FilePath: "repo/main.go", Line: 3}
	g.AddEdge(e)
	New(g).ResolveAll()
	assert.Equal(t, "repo/config/settings.py", e.To, "another language remains the fallback when no folder matches")
}
