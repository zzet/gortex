package indexer

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// derivedReadGuard is the store a per-file delta's derived passes run on (a
// DeltaWriter over the indexed graph, every capability of it available) that
// records every whole-graph or whole-repository read a pass makes and every
// file-scoped read of a file outside the allowed set.
type derivedReadGuard struct {
	*graph.DeltaWriter
	allowed map[string]struct{}
	mu      sync.Mutex
	whole   []string
	outside []string
}

func (g *derivedReadGuard) noteWhole(what string) {
	g.mu.Lock()
	g.whole = append(g.whole, what)
	g.mu.Unlock()
}

func (g *derivedReadGuard) noteFiles(what string, paths ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range paths {
		if _, ok := g.allowed[p]; !ok {
			g.outside = append(g.outside, what+" "+p)
		}
	}
}

func (g *derivedReadGuard) AllNodes() []*graph.Node {
	g.noteWhole("AllNodes")
	return g.DeltaWriter.AllNodes()
}
func (g *derivedReadGuard) AllEdges() []*graph.Edge {
	g.noteWhole("AllEdges")
	return g.DeltaWriter.AllEdges()
}
func (g *derivedReadGuard) NodesByKind(k graph.NodeKind) iter.Seq[*graph.Node] {
	g.noteWhole("NodesByKind(" + string(k) + ")")
	return g.DeltaWriter.NodesByKind(k)
}
func (g *derivedReadGuard) EdgesByKind(k graph.EdgeKind) iter.Seq[*graph.Edge] {
	g.noteWhole("EdgesByKind(" + string(k) + ")")
	return g.DeltaWriter.EdgesByKind(k)
}
func (g *derivedReadGuard) GetRepoNodes(repo string) []*graph.Node {
	g.noteWhole("GetRepoNodes")
	return g.DeltaWriter.GetRepoNodes(repo)
}
func (g *derivedReadGuard) GetRepoNodesByLanguage(repo, lang string) []*graph.Node {
	g.noteWhole("GetRepoNodesByLanguage")
	return g.DeltaWriter.GetRepoNodesByLanguage(repo, lang)
}
func (g *derivedReadGuard) GetRepoNonContentNodes(repo string) []*graph.Node {
	g.noteWhole("GetRepoNonContentNodes")
	return g.DeltaWriter.GetRepoNonContentNodes(repo)
}
func (g *derivedReadGuard) GetRepoEdges(repo string) []*graph.Edge {
	g.noteWhole("GetRepoEdges")
	return g.DeltaWriter.GetRepoEdges(repo)
}
func (g *derivedReadGuard) RepoEdgesByKinds(repos []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	g.noteWhole(fmt.Sprintf("RepoEdgesByKinds%v", kinds))
	return g.DeltaWriter.RepoEdgesByKinds(repos, kinds)
}
func (g *derivedReadGuard) FileNodeIdentitiesSeq(repos []string) iter.Seq[graph.FileNodeIdentity] {
	g.noteWhole("FileNodeIdentitiesSeq")
	return g.DeltaWriter.FileNodeIdentitiesSeq(repos)
}
func (g *derivedReadGuard) DistinctExternalTargets(kinds []graph.EdgeKind) []string {
	g.noteWhole("DistinctExternalTargets")
	return g.DeltaWriter.DistinctExternalTargets(kinds)
}
func (g *derivedReadGuard) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	g.noteScope("NodesInScopeSeq", files, kinds)
	return g.DeltaWriter.NodesInScopeSeq(repos, files, kinds...)
}
func (g *derivedReadGuard) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	g.noteScope("NodesLightInScopeSeq", files, nil)
	return g.DeltaWriter.NodesLightInScopeSeq(repos, files)
}
func (g *derivedReadGuard) EdgesInScopeSeq(repos, files []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	g.noteScope("EdgesInScopeSeq", files, kinds)
	return g.DeltaWriter.EdgesInScopeSeq(repos, files, kinds...)
}
func (g *derivedReadGuard) GetFileNodes(path string) []*graph.Node {
	g.noteFiles("GetFileNodes", path)
	return g.DeltaWriter.GetFileNodes(path)
}
func (g *derivedReadGuard) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	g.noteFiles("GetFileNodesByPaths", paths...)
	return g.DeltaWriter.GetFileNodesByPaths(paths)
}

// noteScope records a scoped read with no file scope (a repository-wide one)
// as whole, and one with a file scope by its files.
func (g *derivedReadGuard) noteScope(what string, files []string, kinds any) {
	if len(files) == 0 {
		g.noteWhole(fmt.Sprintf("%s%v", what, kinds))
		return
	}
	g.noteFiles(what, files...)
}

// Every derived pass a per-file delta runs reads the changed files and what
// their own rows reach, never the repository: a delta over one file of a
// large repository must not pay for the repository. The passes run with
// every family flagged (declarations, imports, runtime, tests, contracts)
// over a store that records whole-repository reads (AllNodes/AllEdges,
// NodesByKind/EdgesByKind, GetRepoNodes/GetRepoEdges) and file-scoped reads
// of any other file; both must stay empty.
func TestIncrementalDerivedPassesReadOnlyTheDeltasFiles(t *testing.T) {
	builderIsolateGit(t)
	root := builderTempDir(t, "derived-scope")
	builderGit(t, root, "init", "--initial-branch=main")
	builderWriteTree(t, root, map[string]string{
		"go.mod":      "module example.test/m\n\ngo 1.23\n",
		"p/a.go":      "package p\n\nimport \"os\"\n\ntype Setter interface{ Set() }\n\ntype Box struct{ n int }\n\nfunc (b *Box) Set() { b.n = 1 }\n\nfunc Env() string { return os.Getenv(\"HOME\") }\n",
		"p/b.go":      "package p\n\nfunc Use(b *Box) { b.Set() }\n",
		"p/a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestSet(t *testing.T) { (&Box{}).Set() }\n",
		"q/c.go":      "package q\n\nimport \"example.test/m/p\"\n\nfunc Call() string { return p.Env() }\n",
	})
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "fixture")

	g := graph.New()
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := New(g, builderRegistry(), cfg, zap.NewNop())
	idx.SetRepoPrefix(builderRepoPrefix)
	_, err := idx.Index(root)
	require.NoError(t, err)

	changed := builderRepoPrefix + "/p/a.go"
	var typeIDs []string
	for _, n := range g.GetFileNodes(changed) {
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			typeIDs = append(typeIDs, n.ID)
		}
	}
	require.NotEmpty(t, typeIDs, "fixture precondition: the changed file declares types")
	guard := &derivedReadGuard{DeltaWriter: graph.NewDeltaWriter(g, nil), allowed: map[string]struct{}{changed: {}}}
	mi := NewMultiIndexer(guard, idx.registry, nil, nil, zap.NewNop())
	mi.indexers[builderRepoPrefix] = idx
	mi.repos[builderRepoPrefix] = &RepoMetadata{RepoPrefix: builderRepoPrefix, RootPath: root}
	var symbolIDs []string
	for _, n := range g.GetFileNodes(changed) {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			symbolIDs = append(symbolIDs, n.ID)
		}
	}
	// The contract family carries its frontier, as a source file's delta
	// does (refreshContractsForFiles): the flag without one comes only from a
	// module manifest, which a delta never builds (dependencyManifestPath).
	report := mi.runIncrementalDerivedPassesWithPriorTopologyHeld(context.Background(), map[string]DerivedInvalidationPlan{
		builderRepoPrefix: {
			Files:             []string{changed},
			TypeIDs:           typeIDs,
			ContractSymbolIDs: symbolIDs,
			Flags: DerivedInvalidatesDeclarations | DerivedInvalidatesImports | DerivedInvalidatesRuntime |
				DerivedInvalidatesTests | DerivedInvalidatesContracts,
		},
	}, nil)
	require.Positive(t, report.TestEdges+report.Capability, "fixture precondition: the passes derived nothing, so their reads prove nothing")

	// The one repository-scoped read a delta keeps: the implements pass's
	// interface census. A changed type may implement any interface of its
	// repository, and an interface's method set lives in the interface node's
	// Meta, so the candidates cannot be found by method name; the census is
	// one kind-and-repository indexed read (0-48 ms per delta on the real
	// repository).
	allowedWhole := map[string]bool{"NodesInScopeSeq[interface]": true}
	whole := guard.whole[:0]
	for _, w := range guard.whole {
		if !allowedWhole[w] {
			whole = append(whole, w)
		}
	}
	guard.whole = whole
	sort.Strings(guard.whole)
	sort.Strings(guard.outside)
	if len(guard.whole) > 0 || len(guard.outside) > 0 {
		t.Fatalf("a derived pass over one changed file read beyond it:\n whole-repository reads: %s\n other files: %s",
			strings.Join(guard.whole, ", "), strings.Join(guard.outside, ", "))
	}
}
