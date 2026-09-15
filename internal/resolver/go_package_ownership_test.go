package resolver

import (
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type ownershipResolverFixture struct {
	r                  *Resolver
	mainFile, caller   *graph.Node
	files, definitions []*graph.Node
}

func newOwnershipResolverFixture(t testing.TB, language string) ownershipResolverFixture {
	t.Helper()
	g := graph.New()
	f := ownershipResolverFixture{
		mainFile: &graph.Node{ID: "repo/main.go", Kind: graph.KindFile, FilePath: "repo/main.go", RepoPrefix: "repo", Language: language},
		caller:   &graph.Node{ID: "repo/main.go::Call", Name: "Call", Kind: graph.KindFunction, FilePath: "repo/main.go", RepoPrefix: "repo", Language: language},
	}
	for _, file := range []string{"repo/internal/graph/a.go", "repo/misc/graph/b.go"} {
		f.files = append(f.files, &graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo", Language: language})
		f.definitions = append(f.definitions, &graph.Node{ID: file + "::Use", Name: "Use", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo", Language: language})
	}
	nodes := append([]*graph.Node{f.mainFile, f.caller}, f.files...)
	nodes = append(nodes, f.definitions...)
	g.AddBatch(nodes, nil)
	f.r = New(g)
	f.r.nodeByID = make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		f.r.nodeByID[node.ID] = node
	}
	f.r.dirByFilePath = map[string]string{
		f.files[0].FilePath: "repo/internal/graph",
		f.files[1].FilePath: "repo/misc/graph",
	}
	return f
}

func ownershipFileIdentity(node *graph.Node) graph.FileNodeIdentity {
	return graph.FileNodeIdentity{ID: node.ID, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix}
}

func installFixtureOwnership(t testing.TB, f ownershipResolverFixture, importPath, expectedFile string) {
	t.Helper()
	f.r.SetGoPackageOwnership(func(query GoImportCandidate) GoPackageOwnershipResult {
		if query.ImportPath != importPath || query.ImporterRepoPrefix != "repo" || query.ImporterFilePath != "repo/main.go" {
			t.Errorf("unexpected identity query: %+v", query)
			return GoPackageOwnershipUnknown
		}
		if query.CandidateRepoPrefix != "repo" {
			return GoPackageOwnershipUnknown
		}
		for _, file := range f.files {
			if query.CandidateFilePath == file.FilePath {
				// Independent fixture authority: one real module identity plus
				// the complete package directory, never a basename or cold answer.
				packageImport := path.Join("example.test/fixture", path.Dir(strings.TrimPrefix(file.FilePath, "repo/")))
				if packageImport == query.ImportPath {
					if file.FilePath != expectedFile {
						t.Errorf("expected target disagrees with module/package authority: %s != %s", file.FilePath, expectedFile)
					}
					return GoPackageOwnershipExact
				}
				return GoPackageOwnershipDifferent
			}
		}
		return GoPackageOwnershipUnknown
	})
}

func TestGoOwnershipOptionalProviderExternExactImport(t *testing.T) {
	for _, exact := range []int{0, 1} {
		t.Run([]string{"exact_internal", "exact_misc"}[exact], func(t *testing.T) {
			f := newOwnershipResolverFixture(t, "go")
			importPath := []string{"example.test/fixture/internal/graph", "example.test/fixture/misc/graph"}[exact]
			e := &graph.Edge{From: f.caller.ID, To: "unresolved::extern::" + importPath + "::Use", Kind: graph.EdgeCalls, FilePath: f.mainFile.FilePath}
			scope, _ := f.r.resolverNameScopeForEdge(e, "")
			// Force the unrelated same-basename package first; do not depend on
			// backend ordering to make the regression fail.
			f.r.nodesByExternLanguageName = map[string]map[string][]*graph.Node{
				scope.languageKey: {"Use": {f.definitions[1-exact], f.definitions[exact]}},
			}
			installFixtureOwnership(t, f, importPath, f.files[exact].FilePath)
			var stats ResolveStats
			f.r.resolveExtern(e, importPath+"::Use", &stats)
			if e.To != f.definitions[exact].ID || e.CrossRepo || stats.Resolved != 1 || stats.External != 0 || stats.Unresolved != 0 {
				t.Fatalf("edge=%+v stats=%+v want target=%s", e, stats, f.definitions[exact].ID)
			}
		})
	}
}

func TestGoOwnershipOptionalProviderImportEveryPhysicalCandidatePath(t *testing.T) {
	for _, exact := range []int{0, 1} {
		for _, mode := range []string{"qualified_name", "direct_directory", "last_component", "full_scan"} {
			t.Run([]string{"exact_internal", "exact_misc"}[exact]+"/"+mode, func(t *testing.T) {
				f := newOwnershipResolverFixture(t, "go")
				importPath := []string{"example.test/fixture/internal/graph", "example.test/fixture/misc/graph"}[exact]
				ordered := []*graph.Node{f.files[1-exact], f.files[exact]}
				f.r.nodesByQualName = map[string][]*graph.Node{importPath: {}}
				f.r.dirIndex = map[string][]graph.FileNodeIdentity{}
				f.r.lastDirIndex = map[string][]graph.FileNodeIdentity{}
				files := []graph.FileNodeIdentity{ownershipFileIdentity(ordered[0]), ownershipFileIdentity(ordered[1])}
				switch mode {
				case "qualified_name":
					f.r.nodesByQualName[importPath] = ordered
				case "direct_directory":
					f.r.dirIndex[importPath] = files
				case "last_component":
					f.r.lastDirIndex["graph"] = files
				case "full_scan":
					f.r.dirIndex = nil
				}
				installFixtureOwnership(t, f, importPath, f.files[exact].FilePath)
				e := &graph.Edge{From: f.mainFile.ID, To: "unresolved::import::" + importPath, Kind: graph.EdgeImports, FilePath: f.mainFile.FilePath}
				var stats ResolveStats
				f.r.resolveImport(e, importPath, &stats)
				if e.To != f.files[exact].ID || e.CrossRepo || stats.Resolved != 1 || stats.External != 0 || stats.Unresolved != 0 {
					t.Fatalf("edge=%+v stats=%+v want target=%s", e, stats, f.files[exact].ID)
				}
			})
		}
	}
}

func TestGoOwnershipOptionalProviderNilAndUnknownPreserveExternOrder(t *testing.T) {
	for _, mode := range []string{"nil", "unknown", "unknown_before_exact"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnershipResolverFixture(t, "go")
			importPath := "example.test/fixture/misc/graph"
			e := &graph.Edge{From: f.caller.ID, To: "unresolved::extern::" + importPath + "::Use", Kind: graph.EdgeCalls, FilePath: f.mainFile.FilePath}
			scope, _ := f.r.resolverNameScopeForEdge(e, "")
			f.r.nodesByExternLanguageName = map[string]map[string][]*graph.Node{scope.languageKey: {"Use": f.definitions}}
			if mode != "nil" {
				f.r.SetGoPackageOwnership(func(query GoImportCandidate) GoPackageOwnershipResult {
					if mode == "unknown_before_exact" && query.CandidateFilePath == f.files[1].FilePath {
						return GoPackageOwnershipExact
					}
					return GoPackageOwnershipUnknown
				})
			}
			var stats ResolveStats
			f.r.resolveExtern(e, importPath+"::Use", &stats)
			if e.To != f.definitions[0].ID {
				t.Fatalf("changed compatible ordering: %s", e.To)
			}
		})
	}
}

func TestGoOwnershipProviderNotConsultedForNonGoOrMissingSource(t *testing.T) {
	for _, language := range []string{"typescript", "javascript", "python", ""} {
		t.Run(language, func(t *testing.T) {
			f := newOwnershipResolverFixture(t, language)
			f.r.SetGoPackageOwnership(func(GoImportCandidate) GoPackageOwnershipResult {
				t.Fatal("consulted Go provider for non-Go source")
				return GoPackageOwnershipDifferent
			})
			e := &graph.Edge{From: f.caller.ID, FilePath: f.caller.FilePath}
			gate := f.r.goImportGateForEdge(e, "example.test/fixture/misc/graph")
			if !gate.retainNode(f.definitions[0]) || !gate.retainFile(ownershipFileIdentity(f.files[0])) {
				t.Fatal("non-Go filtered")
			}
		})
	}
	f := newOwnershipResolverFixture(t, "go")
	f.r.SetGoPackageOwnership(func(GoImportCandidate) GoPackageOwnershipResult {
		t.Fatal("consulted provider without hydrated source")
		return GoPackageOwnershipDifferent
	})
	f.r.nodeByID = map[string]*graph.Node{}
	// The backing graph DOES contain this Go caller. Falling through the
	// authoritative empty cache to the store would invoke the failing provider.
	if !f.r.goImportGateForEdge(&graph.Edge{From: f.caller.ID}, "x").retainNode(f.definitions[0]) {
		t.Fatal("missing source filtered")
	}
	// A nil optional provider must not read the graph at all.
	zero := &Resolver{}
	if !zero.goImportGateForEdge(&graph.Edge{From: "missing"}, "x").retainNode(f.definitions[0]) {
		t.Fatal("nil provider filtered")
	}
}

func TestGoOwnershipQualifiedCandidateFilterDoesNotMutateSharedSlice(t *testing.T) {
	f := newOwnershipResolverFixture(t, "go")
	original := []*graph.Node{f.definitions[0], nil, f.definitions[1], f.definitions[0]}
	wantOriginal := append([]*graph.Node(nil), original...)
	gate := goImportCandidateGate{lookup: func(query GoImportCandidate) GoPackageOwnershipResult {
		if query.CandidateID == f.definitions[1].ID {
			return GoPackageOwnershipDifferent
		}
		return GoPackageOwnershipUnknown
	}}
	filtered := gate.filterNodes(original)
	if !reflect.DeepEqual(original, wantOriginal) {
		t.Fatal("modified shared cache slice")
	}
	if !reflect.DeepEqual(filtered, []*graph.Node{f.definitions[0], nil, f.definitions[0]}) {
		t.Fatalf("changed unknown order/duplicates: %v", filtered)
	}
	noOp := (goImportCandidateGate{}).filterNodes(original)
	if &noOp[0] != &original[0] {
		t.Fatal("nil provider copied candidates")
	}
}

func TestGoOwnershipInvalidVerdictAndFilelessCandidateRemainUnknown(t *testing.T) {
	calls := 0
	gate := goImportCandidateGate{lookup: func(GoImportCandidate) GoPackageOwnershipResult { calls++; return GoPackageOwnershipResult(255) }}
	if !gate.retainNode(&graph.Node{ID: "candidate", FilePath: "repo/graph/a.go"}) {
		t.Fatal("invalid verdict rejected candidate")
	}
	if !gate.retainNode(&graph.Node{ID: "dep::example.test/pkg"}) || calls != 1 {
		t.Fatal("fileless dependency candidate consulted/rejected")
	}
}

func TestGoOwnershipKnownWrongPackageCannotReenterFallback(t *testing.T) {
	f := newOwnershipResolverFixture(t, "go")
	importPath := "example.test/fixture/misc/graph"
	installFixtureOwnership(t, f, importPath, f.files[1].FilePath)
	call := &graph.Edge{From: f.caller.ID, FilePath: f.mainFile.FilePath, Kind: graph.EdgeCalls}
	scope, _ := f.r.resolverNameScopeForEdge(call, "")
	f.r.nodesByExternLanguageName = map[string]map[string][]*graph.Node{scope.languageKey: {"Use": {f.definitions[0]}}}
	var callStats ResolveStats
	f.r.resolveExtern(call, importPath+"::Use", &callStats)
	if call.To != "dep::"+importPath+"::Use" || callStats.Resolved != 0 || callStats.External != 1 {
		t.Fatalf("known wrong Go package escaped extern rejection: edge=%+v stats=%+v", call, callStats)
	}
	wrong := ownershipFileIdentity(f.files[0])
	f.r.nodesByQualName = map[string][]*graph.Node{importPath: {f.files[0]}}
	f.r.dirIndex = map[string][]graph.FileNodeIdentity{importPath: {wrong}}
	f.r.lastDirIndex = map[string][]graph.FileNodeIdentity{"graph": {wrong}}
	imp := &graph.Edge{From: f.mainFile.ID, FilePath: f.mainFile.FilePath, Kind: graph.EdgeImports}
	var importStats ResolveStats
	f.r.resolveImport(imp, importPath, &importStats)
	if imp.To != "external::"+importPath || importStats.Resolved != 0 || importStats.External != 1 {
		t.Fatalf("known wrong Go package escaped import rejection: edge=%+v stats=%+v", imp, importStats)
	}
}

func TestGoOwnershipRebindAndClearBeforeResolution(t *testing.T) {
	f := newOwnershipResolverFixture(t, "go")
	for _, exact := range []int{0, 1} {
		importPath := []string{"example.test/fixture/internal/graph", "example.test/fixture/misc/graph"}[exact]
		installFixtureOwnership(t, f, importPath, f.files[exact].FilePath)
		call := &graph.Edge{From: f.caller.ID, FilePath: f.mainFile.FilePath, Kind: graph.EdgeCalls}
		scope, _ := f.r.resolverNameScopeForEdge(call, "")
		f.r.nodesByExternLanguageName = map[string]map[string][]*graph.Node{scope.languageKey: {"Use": f.definitions}}
		var stats ResolveStats
		f.r.resolveExtern(call, importPath+"::Use", &stats)
		if call.To != f.definitions[exact].ID {
			t.Fatalf("provider rebind selected %s want %s", call.To, f.definitions[exact].ID)
		}
	}
	f.r.SetGoPackageOwnership(nil)
	call := &graph.Edge{From: f.caller.ID, FilePath: f.mainFile.FilePath, Kind: graph.EdgeCalls}
	var stats ResolveStats
	f.r.resolveExtern(call, "example.test/fixture/misc/graph::Use", &stats)
	if call.To != f.definitions[0].ID {
		t.Fatalf("nil provider did not restore legacy compatibility: %s", call.To)
	}
}
