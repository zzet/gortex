package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// paramReadCountingStore counts the parameter-edge reads by owner.
type paramReadCountingStore struct {
	graph.Store
	owners map[string]int
}

func (s *paramReadCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	for _, id := range ids {
		s.owners[id]++
	}
	return s.Store.GetInEdgesByNodeIDs(ids)
}

// NodesInFilesByKind counts the file-scoped parameter reads, per file.
func (s *paramReadCountingStore) NodesInFilesByKind(files []string, kinds []graph.NodeKind) []*graph.Node {
	for _, f := range files {
		s.owners["file:"+f]++
	}
	return s.Store.(graph.NodesInFilesByKindFinder).NodesInFilesByKind(files, kinds)
}

// The callee parameter index is kept per stack for callees declared outside
// the change set: every answer equals the uncached index, a second delta over
// the stack reads no unchanged callee again, and a callee in the change set
// is read on every delta.
func TestEditDeltaParamIndexIsKeptPerStackOutsideTheChangeSet(t *testing.T) {
	resetEditDeltaParamIndexes()
	t.Cleanup(resetEditDeltaParamIndexes)
	g := graph.New()
	callee := func(file, name string, params int) string {
		id := "repo/" + file + "::" + name
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: "repo/" + file, RepoPrefix: "repo", Language: "go"})
		for i := 0; i < params; i++ {
			pid := fmt.Sprintf("%s#param:p%d", id, i)
			g.AddNode(&graph.Node{ID: pid, Kind: graph.KindParam, Name: fmt.Sprintf("p%d", i), FilePath: "repo/" + file,
				RepoPrefix: "repo", Language: "go", Meta: map[string]any{"position": i}})
			g.AddEdge(&graph.Edge{From: pid, To: id, Kind: graph.EdgeParamOf, FilePath: "repo/" + file})
		}
		return id
	}
	stable := callee("lib/lib.go", "Helper", 2)
	changed := callee("app/app.go", "Local", 1)
	callees := map[string]struct{}{stable: {}, changed: {}}
	want := fmt.Sprint(buildParamPositionIndex(g, callees))

	store := &paramReadCountingStore{Store: g, owners: map[string]int{}}
	for delta := 1; delta <= 2; delta++ {
		p := newEditDeltaParamIndex("stack", []string{"repo/app/app.go"})
		if got := fmt.Sprint(p.index(store, callees)); got != want {
			t.Fatalf("delta %d: %s, want %s", delta, got, want)
		}
	}
	stableReads := store.owners[stable] + store.owners["file:repo/lib/lib.go"]
	changedReads := store.owners[changed] + store.owners["file:repo/app/app.go"]
	if stableReads != 1 {
		t.Errorf("the unchanged callee was read %d times over two deltas, want once", stableReads)
	}
	if changedReads != 2 {
		t.Errorf("the changed callee was read %d times over two deltas, want every delta", changedReads)
	}
	if store.owners[stable]+store.owners[changed] != 0 {
		t.Errorf("a Go callee's parameters were read through its in-edges (%d reads), not its file", store.owners[stable]+store.owners[changed])
	}
}

// The file-scoped parameter index equals the param_of-edge index for every
// function and method of a real Go extraction (receivers, variadics, several
// names sharing a type, no parameters).
func TestParamIndexByFileMatchesTheEdgeIndex(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\ntype T struct{}\n\nfunc (t *T) M(a, b int, rest ...string) {}\n\nfunc F(x int, y string) {}\n\nfunc None() {}\n\nfunc G(fn func(int) int, n int) int { return fn(n) }\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("repo")
	if _, err := idx.Index(dir); err != nil {
		t.Fatal(err)
	}
	callees := map[string]struct{}{}
	for _, kind := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
		for n := range g.NodesByKind(kind) {
			callees[n.ID] = struct{}{}
		}
	}
	if len(callees) < 4 {
		t.Fatalf("fixture precondition: %d callees", len(callees))
	}
	byEdges := buildParamPositionIndex(g, callees)
	byFile := paramPositionIndexByFile(g, callees)
	if fmt.Sprint(byFile) != fmt.Sprint(byEdges) {
		t.Fatalf("by file %v\nby edges %v", byFile, byEdges)
	}
	if len(byEdges) < 3 {
		t.Fatalf("fixture precondition: %d callees with parameters", len(byEdges))
	}
}
