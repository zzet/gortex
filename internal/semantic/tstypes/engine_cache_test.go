package tstypes

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type lookupCountingStore struct {
	graph.Store
	findInRepoCalls    int
	findCalls          int
	nodeCalls          int
	inEdgeCalls        int
	outEdgeCalls       int
	inEdgeBatchCalls   int
	outEdgeBatchCalls  int
	nodesByIDCalls     int
	maxInEdgeBatchLen  int
	maxOutEdgeBatchLen int
	repoNameBatchCalls int
}

func (s *lookupCountingStore) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	s.findInRepoCalls++
	return s.Store.FindNodesByNameInRepo(name, repoPrefix)
}

func (s *lookupCountingStore) FindNodesByName(name string) []*graph.Node {
	s.findCalls++
	return s.Store.FindNodesByName(name)
}

func (s *lookupCountingStore) GetInEdges(id string) []*graph.Edge {
	s.inEdgeCalls++
	return s.Store.GetInEdges(id)
}

func (s *lookupCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.outEdgeCalls++
	return s.Store.GetOutEdges(id)
}

func (s *lookupCountingStore) GetNode(id string) *graph.Node {
	s.nodeCalls++
	return s.Store.GetNode(id)
}

func (s *lookupCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inEdgeBatchCalls++
	if len(ids) > s.maxInEdgeBatchLen {
		s.maxInEdgeBatchLen = len(ids)
	}
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *lookupCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outEdgeBatchCalls++
	if len(ids) > s.maxOutEdgeBatchLen {
		s.maxOutEdgeBatchLen = len(ids)
	}
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *lookupCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodesByIDCalls++
	return s.Store.GetNodesByIDs(ids)
}

func (s *lookupCountingStore) FindNodesByNamesInRepo(names []string, repoPrefix string) map[string][]*graph.Node {
	s.repoNameBatchCalls++
	if finder, ok := s.Store.(graph.RepoNamesNodeFinder); ok {
		return finder.FindNodesByNamesInRepo(names, repoPrefix)
	}
	return nil
}

// FindNodesByNamesInRepoLanguages is the language-scoped candidate lookup the
// paging engine now issues; count it through the same bounded-page counter.
func (s *lookupCountingStore) FindNodesByNamesInRepoLanguages(names []string, repoPrefix string, languages []string) map[string][]*graph.Node {
	s.repoNameBatchCalls++
	return graph.FindNodesByNamesInRepoLanguages(s.Store, names, repoPrefix, languages)
}

func TestApplierMemoizesStoreLookups(t *testing.T) {
	base := graph.New()
	typeNode := &graph.Node{
		ID:         "repo/types.ts::Remote",
		Kind:       graph.KindType,
		Name:       "Remote",
		FilePath:   "repo/types.ts",
		Language:   "typescript",
		RepoPrefix: "repo",
	}
	methodNode := &graph.Node{
		ID:         "repo/types.ts::Remote.run",
		Kind:       graph.KindMethod,
		Name:       "run",
		FilePath:   "repo/types.ts",
		Language:   "typescript",
		RepoPrefix: "repo",
	}
	base.AddBatch([]*graph.Node{typeNode, methodNode}, []*graph.Edge{{
		From: methodNode.ID,
		To:   typeNode.ID,
		Kind: graph.EdgeMemberOf,
	}})

	store := &lookupCountingStore{Store: base}
	ap := newApplier(store, TypeScriptSpec(), "test-types")
	idx := &fileIndex{
		facts: &fileFacts{file: "repo/use.ts", repoPrefix: "repo", calls: []callFact{
			{recvType: "Remote"}, {recvType: "Missing"},
		}},
		imports: map[string]string{},
		types:   map[string]*graph.Node{},
	}
	ap.preload([]*fileFacts{idx.facts})

	for i := 0; i < 100; i++ {
		if got := ap.resolveTypeNode(idx, "Remote"); got == nil || got.ID != typeNode.ID {
			t.Fatalf("resolveTypeNode(Remote) = %#v, want %s", got, typeNode.ID)
		}
	}
	if got := store.findInRepoCalls; got != 0 {
		t.Fatalf("FindNodesByNameInRepo calls = %d, want 0 after page-name prefetch", got)
	}
	if got := store.repoNameBatchCalls; got != 1 {
		t.Fatalf("FindNodesByNamesInRepo calls = %d, want one bounded page query", got)
	}

	for i := 0; i < 100; i++ {
		if got := ap.resolveTypeNode(idx, "Missing"); got != nil {
			t.Fatalf("resolveTypeNode(Missing) = %#v, want nil", got)
		}
	}
	if got := store.findInRepoCalls; got != 0 {
		t.Fatalf("FindNodesByNameInRepo calls including cached miss = %d, want 0", got)
	}

	for i := 0; i < 100; i++ {
		if got := ap.methodOn(typeNode, "run", 0, 0); got == nil || got.ID != methodNode.ID {
			t.Fatalf("methodOn(Remote, run) = %#v, want %s", got, methodNode.ID)
		}
	}
	if got := store.inEdgeCalls; got != 0 {
		t.Fatalf("GetInEdges calls = %d, want 0", got)
	}
	if got := store.outEdgeCalls; got != 0 {
		t.Fatalf("GetOutEdges calls = %d, want 0", got)
	}
	if got := store.nodeCalls; got != 0 {
		t.Fatalf("GetNode calls = %d, want 0", got)
	}
	if got := store.nodesByIDCalls; got != 1 {
		t.Fatalf("GetNodesByIDs calls = %d, want one batched member-frontier query", got)
	}
	if got := store.inEdgeBatchCalls; got != 2 {
		t.Fatalf("GetInEdgesByNodeIDs calls = %d, want two bounded frontier rounds", got)
	}
	if got := store.outEdgeBatchCalls; got != 2 {
		t.Fatalf("GetOutEdgesByNodeIDs calls = %d, want two bounded frontier rounds", got)
	}
}

func TestMethodMemoizationKeepsDepthBound(t *testing.T) {
	base := graph.New()
	child := &graph.Node{ID: "repo/types.ts::Child", Kind: graph.KindType, Name: "Child", Language: "typescript", RepoPrefix: "repo"}
	parent := &graph.Node{ID: "repo/types.ts::Parent", Kind: graph.KindType, Name: "Parent", Language: "typescript", RepoPrefix: "repo"}
	method := &graph.Node{ID: "repo/types.ts::Parent.run", Kind: graph.KindMethod, Name: "run", Language: "typescript", RepoPrefix: "repo"}
	base.AddBatch([]*graph.Node{child, parent, method}, []*graph.Edge{
		{From: child.ID, To: parent.ID, Kind: graph.EdgeExtends},
		{From: method.ID, To: parent.ID, Kind: graph.EdgeMemberOf},
	})

	ap := newApplier(base, TypeScriptSpec(), "test-types")
	ap.preload([]*fileFacts{{
		file: "repo/use.ts", repoPrefix: "repo",
		calls: []callFact{{recvType: "Child", method: "run"}},
	}})
	if got := ap.methodOn(child, "run", 0, extendsWalkDepth); got != nil {
		t.Fatalf("depth-limited method lookup = %#v, want nil", got)
	}
	if got := ap.methodOn(child, "run", 0, 0); got == nil || got.ID != method.ID {
		t.Fatalf("root method lookup after depth-limited miss = %#v, want %s", got, method.ID)
	}
}

func TestApplicationFrontierBatchQueriesDoNotScalePerSymbol(t *testing.T) {
	type counts struct {
		outBatch int
		inBatch  int
		nodes    int
	}
	run := func(t *testing.T, size int) counts {
		t.Helper()
		base := graph.New()
		nodes := make([]*graph.Node, 0, size*5)
		edges := make([]*graph.Edge, 0, size*5)
		children := make([]*graph.Node, 0, size)
		selected := make([]string, 0, size)
		unrelated := make([]string, 0, size)
		for i := 0; i < size; i++ {
			prefix := fmt.Sprintf("repo/use.ts::T%d", i)
			child := &graph.Node{
				ID: prefix, Kind: graph.KindType, Name: fmt.Sprintf("T%d", i),
				FilePath: "repo/use.ts", Language: "typescript", RepoPrefix: "repo",
			}
			parent := &graph.Node{
				ID: prefix + "Parent", Kind: graph.KindType, Name: fmt.Sprintf("T%dParent", i),
				FilePath: "repo/legacy.ts", RepoPrefix: "repo",
			}
			zero := &graph.Node{
				ID: parent.ID + ".run0", Kind: graph.KindMethod, Name: "run",
				FilePath: parent.FilePath, RepoPrefix: "repo",
			}
			one := &graph.Node{
				ID: parent.ID + ".run1", Kind: graph.KindMethod, Name: "run",
				FilePath: parent.FilePath, RepoPrefix: "repo",
			}
			param := &graph.Node{
				ID: one.ID + ".arg", Kind: graph.KindParam, Name: "arg",
				FilePath: parent.FilePath, RepoPrefix: "repo",
			}
			callTarget := &graph.Node{
				ID: prefix + "Unrelated", Kind: graph.KindFunction, Name: "unrelated",
				FilePath: "repo/other.js", RepoPrefix: "repo",
			}
			nodes = append(nodes, child, parent, zero, one, param, callTarget)
			edges = append(edges,
				&graph.Edge{From: child.ID, To: parent.ID, Kind: graph.EdgeExtends},
				&graph.Edge{From: child.ID, To: callTarget.ID, Kind: graph.EdgeCalls},
				&graph.Edge{From: zero.ID, To: parent.ID, Kind: graph.EdgeMemberOf},
				&graph.Edge{From: one.ID, To: parent.ID, Kind: graph.EdgeMemberOf},
				&graph.Edge{From: param.ID, To: one.ID, Kind: graph.EdgeParamOf},
			)
			children = append(children, child)
			selected = append(selected, one.ID)
			unrelated = append(unrelated, callTarget.ID)
		}
		base.AddBatch(nodes, edges)

		store := &lookupCountingStore{Store: base}
		ap := newApplier(store, TypeScriptSpec(), "test-types")
		ap.preload([]*fileFacts{{file: "repo/use.ts", repoPrefix: "repo"}})
		for i, child := range children {
			if got := ap.methodOn(child, "run", 1, 0); got == nil || got.ID != selected[i] {
				t.Fatalf("methodOn(%s, run/1) = %#v, want %s", child.ID, got, selected[i])
			}
			// The frontier preload loads the structural kinds only; the
			// child's outgoing call is no caller's (the page has no call
			// facts), so it is never read.
			gotEdges := ap.outEdges(child.ID)
			var wantEdges []*graph.Edge
			for _, e := range base.GetOutEdges(child.ID) {
				if edgeKindIn(e.Kind, ap.outKinds) {
					wantEdges = append(wantEdges, e)
				}
			}
			if ap.callsLoaded[child.ID] {
				t.Fatalf("frontier preload loaded the outgoing calls of %s", child.ID)
			}
			if len(gotEdges) != len(wantEdges) {
				t.Fatalf("out edge count for %s = %d, want %d", child.ID, len(gotEdges), len(wantEdges))
			}
			for j := range gotEdges {
				if gotEdges[j].From != wantEdges[j].From || gotEdges[j].To != wantEdges[j].To || gotEdges[j].Kind != wantEdges[j].Kind {
					t.Fatalf("out edge %d for %s = %#v, want %#v", j, child.ID, gotEdges[j], wantEdges[j])
				}
			}
			if ap.nodeLoaded[unrelated[i]] {
				t.Fatalf("unrelated call target %s was expanded into the node frontier", unrelated[i])
			}
		}
		if store.outEdgeCalls != 0 || store.inEdgeCalls != 0 || store.nodeCalls != 0 {
			t.Fatalf("scalar lookups: out=%d in=%d node=%d, want all zero", store.outEdgeCalls, store.inEdgeCalls, store.nodeCalls)
		}
		return counts{outBatch: store.outEdgeBatchCalls, inBatch: store.inEdgeBatchCalls, nodes: store.nodesByIDCalls}
	}

	small := run(t, 1)
	large := run(t, 128)
	if small != (counts{outBatch: 3, inBatch: 3, nodes: 3}) {
		t.Fatalf("small frontier query counts = %+v, want 3 bounded rounds per projection", small)
	}
	if large != small {
		t.Fatalf("large frontier query counts = %+v, want scale-independent %+v", large, small)
	}
}

func TestLanguageFilesSkipVendoredSources(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"src/app.ts", "node_modules/pkg/index.ts"} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("export const value = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/src/app.ts", Kind: graph.KindFile, Name: "app.ts", FilePath: "repo/src/app.ts", Language: "typescript", RepoPrefix: "repo"},
		{ID: "repo/node_modules/pkg/index.ts", Kind: graph.KindFile, Name: "index.ts", FilePath: "repo/node_modules/pkg/index.ts", Language: "typescript", RepoPrefix: "repo"},
	}, nil)

	files := languageFiles(g, TypeScriptSpec(), "repo", root)
	if len(files) != 1 || files[0].node.FilePath != "repo/src/app.ts" {
		t.Fatalf("languageFiles() = %#v, want only repo/src/app.ts", files)
	}
}
