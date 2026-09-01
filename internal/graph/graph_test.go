package graph

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func makeNode(id, name string, kind NodeKind, file, lang string) *Node {
	return &Node{
		ID:        id,
		Kind:      kind,
		Name:      name,
		QualName:  "pkg." + name,
		FilePath:  file,
		StartLine: 1,
		EndLine:   10,
		Language:  lang,
	}
}

func TestAddAndGetNode(t *testing.T) {
	g := New()
	n := makeNode("a.go::Foo", "Foo", KindFunction, "a.go", "go")
	g.AddNode(n)

	assert.Equal(t, n, g.GetNode("a.go::Foo"))
	assert.Equal(t, n, g.GetNodeByQualName("pkg.Foo"))
	assert.Equal(t, []*Node{n}, g.FindNodesByName("Foo"))
	assert.Equal(t, []*Node{n}, g.GetFileNodes("a.go"))
	assert.Nil(t, g.GetNode("nonexistent"))
}

func TestEmptyRepoLanguageLookupDoesNotDuplicateShadowIndex(t *testing.T) {
	g := New()
	goNode := makeNode("a.go::Go", "Go", KindFunction, "a.go", "go")
	pyNode := makeNode("a.py::Py", "Py", KindFunction, "a.py", "python")
	g.AddBatch([]*Node{goNode, pyNode}, nil)

	for _, s := range g.shards {
		s.mu.RLock()
		if len(s.byRepo[""]) != 0 || len(s.byRepoIdx[""]) != 0 {
			s.mu.RUnlock()
			t.Fatal("empty-prefix shadow nodes were duplicated into the byRepo index")
		}
		s.mu.RUnlock()
	}
	assert.Equal(t, []*Node{goNode}, g.GetRepoNodesByLanguage("", "go"))
	if nodes, edges := g.EvictRepo(""); nodes != 0 || edges != 0 {
		t.Fatalf("EvictRepo(empty) removed nodes=%d edges=%d, want historical no-op", nodes, edges)
	}
	assert.Equal(t, goNode, g.GetNode(goNode.ID))
}

func TestAddAndGetEdge(t *testing.T) {
	g := New()
	n1 := makeNode("a.go::Foo", "Foo", KindFunction, "a.go", "go")
	n2 := makeNode("b.go::Bar", "Bar", KindFunction, "b.go", "go")
	g.AddNode(n1)
	g.AddNode(n2)

	e := &Edge{From: n1.ID, To: n2.ID, Kind: EdgeCalls, FilePath: "a.go", Line: 5}
	g.AddEdge(e)

	out := g.GetOutEdges(n1.ID)
	require.Len(t, out, 1)
	assert.Equal(t, EdgeCalls, out[0].Kind)

	in := g.GetInEdges(n2.ID)
	require.Len(t, in, 1)
	assert.Equal(t, n1.ID, in[0].From)
}

func TestEvictFile(t *testing.T) {
	g := New()
	n1 := makeNode("a.go::Foo", "Foo", KindFunction, "a.go", "go")
	n2 := makeNode("a.go::Bar", "Bar", KindFunction, "a.go", "go")
	n3 := makeNode("b.go::Baz", "Baz", KindFunction, "b.go", "go")
	g.AddNode(n1)
	g.AddNode(n2)
	g.AddNode(n3)

	g.AddEdge(&Edge{From: n1.ID, To: n3.ID, Kind: EdgeCalls, FilePath: "a.go", Line: 1})
	g.AddEdge(&Edge{From: n3.ID, To: n2.ID, Kind: EdgeCalls, FilePath: "b.go", Line: 2})

	nodesRm, edgesRm := g.EvictFile("a.go")
	assert.Equal(t, 2, nodesRm)
	assert.Equal(t, 2, edgesRm) // both edges reference evicted node IDs

	assert.Nil(t, g.GetNode("a.go::Foo"))
	assert.Nil(t, g.GetNode("a.go::Bar"))
	assert.NotNil(t, g.GetNode("b.go::Baz"))

	// Edge from b.go to a.go::Bar should also be cleaned from inEdges.
	assert.Empty(t, g.GetOutEdges("a.go::Foo"))
}

func TestEvictFile_NoNodes(t *testing.T) {
	g := New()
	n, e := g.EvictFile("nonexistent.go")
	assert.Equal(t, 0, n)
	assert.Equal(t, 0, e)
}

func TestNodeAndEdgeCount(t *testing.T) {
	g := New()
	g.AddNode(makeNode("a.go::A", "A", KindFunction, "a.go", "go"))
	g.AddNode(makeNode("b.go::B", "B", KindType, "b.go", "go"))
	g.AddEdge(&Edge{From: "a.go::A", To: "b.go::B", Kind: EdgeReferences, FilePath: "a.go", Line: 1})

	assert.Equal(t, 2, g.NodeCount())
	assert.Equal(t, 1, g.EdgeCount())
}

func TestStats(t *testing.T) {
	g := New()
	g.AddNode(makeNode("a.go::A", "A", KindFunction, "a.go", "go"))
	g.AddNode(makeNode("b.go::B", "B", KindType, "b.go", "go"))
	g.AddNode(makeNode("c.ts::C", "C", KindFunction, "c.ts", "typescript"))
	g.AddEdge(&Edge{From: "a.go::A", To: "b.go::B", Kind: EdgeReferences, FilePath: "a.go", Line: 1})

	s := g.Stats()
	assert.Equal(t, 3, s.TotalNodes)
	assert.Equal(t, 1, s.TotalEdges)
	assert.Equal(t, 2, s.ByKind["function"])
	assert.Equal(t, 1, s.ByKind["type"])
	assert.Equal(t, 2, s.ByLanguage["go"])
	assert.Equal(t, 1, s.ByLanguage["typescript"])
}

func TestAllNodesAndEdges(t *testing.T) {
	g := New()
	g.AddNode(makeNode("a.go::A", "A", KindFunction, "a.go", "go"))
	g.AddNode(makeNode("b.go::B", "B", KindFunction, "b.go", "go"))
	g.AddEdge(&Edge{From: "a.go::A", To: "b.go::B", Kind: EdgeCalls, FilePath: "a.go", Line: 1})

	assert.Len(t, g.AllNodes(), 2)
	assert.Len(t, g.AllEdges(), 1)
}

func TestConcurrency(t *testing.T) {
	g := New()
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "file.go::" + string(rune('A'+i))
			n := makeNode(id, string(rune('A'+i)), KindFunction, "file.go", "go")
			n.QualName = "" // avoid collision
			g.AddNode(n)
		}(i)
	}

	// Concurrent readers.
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.NodeCount()
			_ = g.GetFileNodes("file.go")
			_ = g.Stats()
		}()
	}

	wg.Wait()
}

func TestNodeBrief(t *testing.T) {
	n := &Node{
		ID: "a.go::Foo", Kind: KindFunction, Name: "Foo",
		QualName: "pkg.Foo", FilePath: "a.go", StartLine: 10, EndLine: 20,
		Language: "go", Meta: map[string]any{"signature": "func Foo()"},
	}
	b := n.Brief()
	assert.Equal(t, "a.go::Foo", b["id"])
	assert.Equal(t, "Foo", b["name"])
	assert.Equal(t, NodeKind("function"), b["kind"])
	assert.Equal(t, "a.go", b["file_path"])
	assert.Equal(t, 10, b["start_line"])
	// Should NOT contain meta, qual_name, end_line, language.
	_, hasMeta := b["meta"]
	assert.False(t, hasMeta)
}

func TestValidNodeKind(t *testing.T) {
	assert.True(t, ValidNodeKind(KindFunction))
	assert.True(t, ValidNodeKind(KindFile))
	assert.False(t, ValidNodeKind(NodeKind("unknown")))
}

func makeRepoNode(id, name string, kind NodeKind, file, lang, repo string) *Node {
	return &Node{
		ID:         id,
		Kind:       kind,
		Name:       name,
		QualName:   repo + "." + name,
		FilePath:   file,
		StartLine:  1,
		EndLine:    10,
		Language:   lang,
		RepoPrefix: repo,
	}
}

func TestAddNode_ByRepoIndex(t *testing.T) {
	g := New()
	n1 := makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA")
	n2 := makeRepoNode("repoB/b.go::Bar", "Bar", KindFunction, "repoB/b.go", "go", "repoB")
	n3 := makeRepoNode("repoA/c.go::Baz", "Baz", KindType, "repoA/c.go", "go", "repoA")
	g.AddNode(n1)
	g.AddNode(n2)
	g.AddNode(n3)

	repoANodes := g.GetRepoNodes("repoA")
	assert.Len(t, repoANodes, 2)
	repoBNodes := g.GetRepoNodes("repoB")
	assert.Len(t, repoBNodes, 1)
	assert.Equal(t, "Bar", repoBNodes[0].Name)
}

func TestAddNode_EmptyRepoPrefix(t *testing.T) {
	g := New()
	n := makeNode("a.go::Foo", "Foo", KindFunction, "a.go", "go")
	g.AddNode(n)

	// Nodes without RepoPrefix are not duplicated in byRepo, but the exact
	// empty-prefix projection still reads them from the shard map.
	assert.Equal(t, []*Node{n}, g.GetRepoNodes(""))
	assert.Empty(t, g.RepoPrefixes())
}

func TestGetRepoNodes_ReturnsCopy(t *testing.T) {
	g := New()
	n := makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA")
	g.AddNode(n)

	nodes := g.GetRepoNodes("repoA")
	nodes[0] = nil // mutate the returned slice
	assert.NotNil(t, g.GetRepoNodes("repoA")[0], "GetRepoNodes should return a copy")
}

func TestGetRepoNodes_NotFound(t *testing.T) {
	g := New()
	assert.Empty(t, g.GetRepoNodes("nonexistent"))
}

func TestEvictRepo(t *testing.T) {
	g := New()
	nA1 := makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA")
	nA2 := makeRepoNode("repoA/a.go::Bar", "Bar", KindFunction, "repoA/a.go", "go", "repoA")
	nB1 := makeRepoNode("repoB/b.go::Baz", "Baz", KindFunction, "repoB/b.go", "go", "repoB")
	g.AddNode(nA1)
	g.AddNode(nA2)
	g.AddNode(nB1)

	// Edges: intra-repoA, cross-repo A→B, intra-repoB self-ref
	g.AddEdge(&Edge{From: nA1.ID, To: nA2.ID, Kind: EdgeCalls, FilePath: "repoA/a.go", Line: 1})
	g.AddEdge(&Edge{From: nA1.ID, To: nB1.ID, Kind: EdgeCalls, FilePath: "repoA/a.go", Line: 2, CrossRepo: true})
	g.AddEdge(&Edge{From: nB1.ID, To: nA2.ID, Kind: EdgeCalls, FilePath: "repoB/b.go", Line: 3, CrossRepo: true})

	nodesRm, edgesRm := g.EvictRepo("repoA")
	assert.Equal(t, 2, nodesRm)
	assert.Equal(t, 3, edgesRm) // all 3 edges reference repoA nodes

	// repoA nodes gone.
	assert.Nil(t, g.GetNode("repoA/a.go::Foo"))
	assert.Nil(t, g.GetNode("repoA/a.go::Bar"))
	assert.Empty(t, g.GetRepoNodes("repoA"))

	// repoB node still present.
	assert.NotNil(t, g.GetNode("repoB/b.go::Baz"))
	assert.Len(t, g.GetRepoNodes("repoB"), 1)

	// byName cleaned for evicted nodes.
	assert.Empty(t, g.FindNodesByName("Foo"))
	assert.Empty(t, g.FindNodesByName("Bar"))
	assert.Len(t, g.FindNodesByName("Baz"), 1)

	// byFile cleaned for evicted nodes.
	assert.Empty(t, g.GetFileNodes("repoA/a.go"))
	assert.Len(t, g.GetFileNodes("repoB/b.go"), 1)
}

func TestEvictRepo_NoNodes(t *testing.T) {
	g := New()
	n, e := g.EvictRepo("nonexistent")
	assert.Equal(t, 0, n)
	assert.Equal(t, 0, e)
}

func TestEvictRepo_QualNameCleaned(t *testing.T) {
	g := New()
	n := makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA")
	g.AddNode(n)

	assert.NotNil(t, g.GetNodeByQualName("repoA.Foo"))
	g.EvictRepo("repoA")
	assert.Nil(t, g.GetNodeByQualName("repoA.Foo"))
}

func TestRepoStats(t *testing.T) {
	g := New()
	g.AddNode(makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA"))
	g.AddNode(makeRepoNode("repoA/b.go::Bar", "Bar", KindType, "repoA/b.go", "go", "repoA"))
	g.AddNode(makeRepoNode("repoB/c.ts::Baz", "Baz", KindFunction, "repoB/c.ts", "typescript", "repoB"))

	// Edge from repoA node.
	g.AddEdge(&Edge{From: "repoA/a.go::Foo", To: "repoA/b.go::Bar", Kind: EdgeCalls, FilePath: "repoA/a.go", Line: 1})
	// Cross-repo edge from repoA to repoB.
	g.AddEdge(&Edge{From: "repoA/a.go::Foo", To: "repoB/c.ts::Baz", Kind: EdgeCalls, FilePath: "repoA/a.go", Line: 2, CrossRepo: true})

	stats := g.RepoStats()
	require.Len(t, stats, 2)

	sA := stats["repoA"]
	assert.Equal(t, 2, sA.TotalNodes)
	assert.Equal(t, 2, sA.TotalEdges) // both edges originate from repoA
	assert.Equal(t, 1, sA.ByKind["function"])
	assert.Equal(t, 1, sA.ByKind["type"])
	assert.Equal(t, 2, sA.ByLanguage["go"])

	sB := stats["repoB"]
	assert.Equal(t, 1, sB.TotalNodes)
	assert.Equal(t, 0, sB.TotalEdges) // no edges originate from repoB
	assert.Equal(t, 1, sB.ByKind["function"])
	assert.Equal(t, 1, sB.ByLanguage["typescript"])
}

func TestRepoStats_Empty(t *testing.T) {
	g := New()
	stats := g.RepoStats()
	assert.Empty(t, stats)
}

func TestRepoPrefixes(t *testing.T) {
	g := New()
	g.AddNode(makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA"))
	g.AddNode(makeRepoNode("repoB/b.go::Bar", "Bar", KindFunction, "repoB/b.go", "go", "repoB"))
	g.AddNode(makeRepoNode("repoA/c.go::Baz", "Baz", KindFunction, "repoA/c.go", "go", "repoA"))

	prefixes := g.RepoPrefixes()
	assert.Len(t, prefixes, 2)
	assert.ElementsMatch(t, []string{"repoA", "repoB"}, prefixes)
}

func TestRepoPrefixes_Empty(t *testing.T) {
	g := New()
	assert.Empty(t, g.RepoPrefixes())
}

func TestEvictFile_CleansByRepoIndex(t *testing.T) {
	g := New()
	n1 := makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA")
	n2 := makeRepoNode("repoA/b.go::Bar", "Bar", KindFunction, "repoA/b.go", "go", "repoA")
	g.AddNode(n1)
	g.AddNode(n2)

	assert.Len(t, g.GetRepoNodes("repoA"), 2)

	g.EvictFile("repoA/a.go")

	// Only n2 should remain in byRepo.
	repoNodes := g.GetRepoNodes("repoA")
	require.Len(t, repoNodes, 1)
	assert.Equal(t, "Bar", repoNodes[0].Name)
}

func TestEvictFile_CleansByRepoIndex_LastNode(t *testing.T) {
	g := New()
	n := makeRepoNode("repoA/a.go::Foo", "Foo", KindFunction, "repoA/a.go", "go", "repoA")
	g.AddNode(n)

	g.EvictFile("repoA/a.go")

	assert.Empty(t, g.GetRepoNodes("repoA"))
	assert.Empty(t, g.RepoPrefixes())
}

// Feature: multi-repo-support, Property 7: Per-repo index correctness

// genRepoPrefix generates a short repo prefix like "repoA", "repoB", etc.
func genRepoPrefix() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		return "repo" + rapid.StringMatching(`[A-Z][a-z]{0,4}`).Draw(t, "suffix")
	})
}

// genMultiRepoGraph builds a graph with 2-5 repo prefixes and 1-10 nodes per repo,
// plus cross-repo edges. Returns the graph and a map of prefix → expected node IDs.
func genMultiRepoGraph(t *rapid.T) (*Graph, map[string][]string) {
	g := New()
	numRepos := rapid.IntRange(2, 5).Draw(t, "numRepos")

	// Generate distinct prefixes.
	prefixSet := make(map[string]bool)
	var prefixes []string
	for len(prefixes) < numRepos {
		p := genRepoPrefix().Draw(t, "prefix")
		if !prefixSet[p] {
			prefixSet[p] = true
			prefixes = append(prefixes, p)
		}
	}

	expected := make(map[string][]string) // prefix → node IDs
	var allNodeIDs []string
	seenIDs := make(map[string]bool)
	kinds := []NodeKind{KindFunction, KindType, KindMethod, KindVariable}

	for _, prefix := range prefixes {
		numNodes := rapid.IntRange(1, 10).Draw(t, "numNodes_"+prefix)
		for j := 0; j < numNodes; j++ {
			name := rapid.StringMatching(`[A-Z][a-zA-Z0-9]{1,10}`).Draw(t, "name")
			file := prefix + "/" + rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "file") + ".go"
			id := file + "::" + name
			kind := kinds[rapid.IntRange(0, len(kinds)-1).Draw(t, "kind")]
			n := makeRepoNode(id, name, kind, file, "go", prefix)
			g.AddNode(n)
			// AddNode upserts by ID, so a drawn (name, file) collision must
			// also appear only once in the expected model.
			if !seenIDs[id] {
				seenIDs[id] = true
				expected[prefix] = append(expected[prefix], id)
				allNodeIDs = append(allNodeIDs, id)
			}
		}
	}

	// Add some edges including cross-repo ones.
	if len(allNodeIDs) >= 2 {
		numEdges := rapid.IntRange(1, len(allNodeIDs)).Draw(t, "numEdges")
		for i := 0; i < numEdges; i++ {
			fromIdx := rapid.IntRange(0, len(allNodeIDs)-1).Draw(t, "fromIdx")
			toIdx := rapid.IntRange(0, len(allNodeIDs)-1).Draw(t, "toIdx")
			if fromIdx == toIdx {
				continue
			}
			fromNode := g.GetNode(allNodeIDs[fromIdx])
			toNode := g.GetNode(allNodeIDs[toIdx])
			crossRepo := fromNode.RepoPrefix != toNode.RepoPrefix
			g.AddEdge(&Edge{
				From:      allNodeIDs[fromIdx],
				To:        allNodeIDs[toIdx],
				Kind:      EdgeCalls,
				FilePath:  fromNode.FilePath,
				Line:      i + 1,
				CrossRepo: crossRepo,
			})
		}
	}

	return g, expected
}

// TestPropertyPerRepoIndexCorrectness verifies that GetRepoNodes returns exactly
// the nodes with matching RepoPrefix, and the union of all GetRepoNodes equals
// all nodes that have a RepoPrefix.
func TestPropertyPerRepoIndexCorrectness(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g, expected := genMultiRepoGraph(rt)

		// For each prefix, GetRepoNodes must return exactly the matching nodes.
		for prefix, wantIDs := range expected {
			got := g.GetRepoNodes(prefix)
			gotIDs := make([]string, len(got))
			for i, n := range got {
				gotIDs[i] = n.ID
				// Every returned node must have the correct RepoPrefix.
				assert.Equal(rt, prefix, n.RepoPrefix,
					"GetRepoNodes(%q) returned node %q with wrong RepoPrefix %q", prefix, n.ID, n.RepoPrefix)
			}
			assert.ElementsMatch(rt, wantIDs, gotIDs,
				"GetRepoNodes(%q) returned wrong set of node IDs", prefix)
		}

		// Union of all GetRepoNodes for all prefixes must equal all nodes with a RepoPrefix.
		prefixes := g.RepoPrefixes()
		unionIDs := make(map[string]bool)
		for _, p := range prefixes {
			for _, n := range g.GetRepoNodes(p) {
				unionIDs[n.ID] = true
			}
		}

		allNodes := g.AllNodes()
		repoNodeIDs := make(map[string]bool)
		for _, n := range allNodes {
			if n.RepoPrefix != "" {
				repoNodeIDs[n.ID] = true
			}
		}

		assert.Equal(rt, repoNodeIDs, unionIDs,
			"Union of GetRepoNodes for all prefixes must equal all nodes with a RepoPrefix")
	})
}

// Feature: multi-repo-support, Property 8: Repo eviction completeness

// TestPropertyRepoEvictionCompleteness verifies that after EvictRepo, zero nodes/edges
// remain for the evicted prefix, and other repos are unchanged.
func TestPropertyRepoEvictionCompleteness(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g, expected := genMultiRepoGraph(rt)

		// Pick a repo to evict.
		prefixes := g.RepoPrefixes()
		require.NotEmpty(rt, prefixes, "graph must have at least one repo prefix")
		evictPrefix := prefixes[rapid.IntRange(0, len(prefixes)-1).Draw(rt, "evictIdx")]

		// Record pre-eviction state for other repos.
		otherCounts := make(map[string]int)
		for _, p := range prefixes {
			if p != evictPrefix {
				otherCounts[p] = len(g.GetRepoNodes(p))
			}
		}

		// Evict.
		nodesRm, _ := g.EvictRepo(evictPrefix)
		assert.Equal(rt, len(expected[evictPrefix]), nodesRm,
			"EvictRepo should remove exactly the expected number of nodes")

		// Verify GetRepoNodes returns empty for evicted prefix.
		assert.Empty(rt, g.GetRepoNodes(evictPrefix),
			"GetRepoNodes(%q) must be empty after eviction", evictPrefix)

		// Verify no nodes in AllNodes have the evicted prefix.
		for _, n := range g.AllNodes() {
			assert.NotEqual(rt, evictPrefix, n.RepoPrefix,
				"AllNodes() still contains node %q with evicted prefix %q", n.ID, evictPrefix)
		}

		// Verify no edges reference evicted node IDs.
		evictedIDs := make(map[string]bool)
		for _, id := range expected[evictPrefix] {
			evictedIDs[id] = true
		}
		for _, e := range g.AllEdges() {
			assert.False(rt, evictedIDs[e.From],
				"Edge from %q → %q still references evicted node", e.From, e.To)
			assert.False(rt, evictedIDs[e.To],
				"Edge from %q → %q still references evicted node", e.From, e.To)
		}

		// Verify other repos' node counts are unchanged.
		for p, wantCount := range otherCounts {
			gotCount := len(g.GetRepoNodes(p))
			assert.Equal(rt, wantCount, gotCount,
				"Repo %q node count changed after evicting %q", p, evictPrefix)
		}
	})
}
