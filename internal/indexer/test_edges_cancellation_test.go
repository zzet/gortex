package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type cancelTestProjectionStore struct {
	*graph.Graph
	cancel context.CancelFunc
	reads  int
}

func (g *cancelTestProjectionStore) NodesInFilesByKind(files []string, kinds []graph.NodeKind) []*graph.Node {
	g.reads++
	nodes := g.Graph.NodesInFilesByKind(files, kinds)
	if g.cancel != nil {
		g.cancel()
	}
	return nodes
}

func TestRetargetedTestProjectionCancellationKeepsCompletedGroupsRepairable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &cancelTestProjectionStore{Graph: graph.New(), cancel: cancel}
	g.AddNode(&graph.Node{ID: "prod::Subject", Kind: graph.KindFunction, Name: "Subject", FilePath: "prod.go", Language: "go"})
	files := make([]string, testMetadataLookupBatchSize+1)
	for i := range files {
		file := fmt.Sprintf("pkg/f%04d_test.go", i)
		files[i] = file
		g.AddBatch([]*graph.Node{
			{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go", Meta: map[string]any{PendingTestProjectionMeta: true, "retained": "yes"}},
			{ID: file + "::TestSubject", Kind: graph.KindFunction, Name: "TestSubject", FilePath: file, Language: "go"},
		}, []*graph.Edge{
			{From: file + "::TestSubject", To: "prod::Subject", Kind: graph.EdgeCalls, FilePath: file},
			{From: file + "::TestSubject", To: "old::Subject", Kind: graph.EdgeTests, FilePath: file},
		})
	}
	// The first group's caller sees a callee classified only by the last
	// group. Completing that later group must reconcile the incoming caller,
	// rather than leave a test-to-test projection from the earlier group.
	g.AddEdge(&graph.Edge{From: files[0] + "::TestSubject", To: files[len(files)-1] + "::TestSubject", Kind: graph.EdgeCalls, FilePath: files[0]})
	_, _, err := markTestSymbolsAndEmitEdgesForFilesContext(ctx, g, files)
	if !errors.Is(err, context.Canceled) || g.reads != 1 {
		t.Fatalf("cancellation = %v, file batches read = %d", err, g.reads)
	}
	first, last := files[0]+"::TestSubject", files[len(files)-1]+"::TestSubject"
	if findEdge(g, graph.EdgeTests, first, "prod::Subject") == nil || findEdge(g, graph.EdgeTests, first, "old::Subject") != nil {
		t.Fatal("cancellation split the completed group's eviction/replacement")
	}
	if findEdge(g, graph.EdgeTests, last, "prod::Subject") != nil || findEdge(g, graph.EdgeTests, last, "old::Subject") == nil {
		t.Fatal("cancellation started the remaining group's projection")
	}
	if g.GetNode(files[0]).Meta[PendingTestProjectionMeta] != nil || g.GetNode(files[len(files)-1]).Meta[PendingTestProjectionMeta] != true || g.GetNode(files[0]).Meta["retained"] != "yes" {
		t.Fatal("cancellation discharged uncompleted debt or lost unrelated metadata")
	}
	// Retry the same frontier normally, including already completed work.
	g.cancel = nil
	repairCtx, finishRepair := context.WithCancel(context.Background())
	defer finishRepair()
	if _, _, err = markTestSymbolsAndEmitEdgesForFilesContext(repairCtx, g, files); err != nil {
		t.Fatal(err)
	}
	if g.reads != 3 {
		t.Fatalf("normal repair did not visit both bounded groups: %d reads", g.reads)
	}
	if findEdge(g, graph.EdgeTests, last, "prod::Subject") == nil || findEdge(g, graph.EdgeTests, last, "old::Subject") != nil {
		t.Fatal("normal repair did not replace the remaining stale projection")
	}
	if findEdge(g, graph.EdgeTests, first, last) != nil {
		t.Fatal("later callee classification left the earlier caller's test-to-test projection")
	}
}

type failingTestProjectionPublication struct {
	*graph.Graph
	err error
}

func (g *failingTestProjectionPublication) AddBatchChecked(nodes []*graph.Node, edges []*graph.Edge) error {
	if g.err != nil {
		return g.err
	}
	g.AddBatch(nodes, edges)
	return nil
}

func TestRetargetedTestProjectionFailedPublicationRetainsDebt(t *testing.T) {
	writeErr := errors.New("test projection publication failed")
	g := &failingTestProjectionPublication{Graph: graph.New(), err: writeErr}
	g.AddBatch([]*graph.Node{
		{ID: "a_test.go", Kind: graph.KindFile, FilePath: "a_test.go", Meta: map[string]any{PendingTestProjectionMeta: true}},
		{ID: "a_test.go::TestA", Kind: graph.KindFunction, Name: "TestA", FilePath: "a_test.go", Language: "go"},
		{ID: "prod::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go"},
	}, []*graph.Edge{{From: "a_test.go::TestA", To: "prod::A", Kind: graph.EdgeCalls, FilePath: "a_test.go"}})
	_, _, err := markTestSymbolsAndEmitEdgesForFilesContext(context.Background(), g, []string{"a_test.go"})
	if !errors.Is(err, writeErr) || g.GetNode("a_test.go").Meta[PendingTestProjectionMeta] != true {
		t.Fatalf("failed publication lost debt: %v", err)
	}
	g.err = nil
	if _, _, err = markTestSymbolsAndEmitEdgesForFilesContext(context.Background(), g, []string{"a_test.go"}); err != nil {
		t.Fatal(err)
	}
	if findEdge(g, graph.EdgeTests, "a_test.go::TestA", "prod::A") == nil || g.GetNode("a_test.go").Meta[PendingTestProjectionMeta] != nil {
		t.Fatal("successful publication did not atomically discharge its repair")
	}
}

func TestRetargetedTestProjectionCanceledBeforeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &cancelTestProjectionStore{Graph: graph.New()}
	_, _, err := markTestSymbolsAndEmitEdgesForFilesContext(ctx, g, []string{"pkg/subject_test.go"})
	if !errors.Is(err, context.Canceled) || g.reads != 0 {
		t.Fatalf("cancellation = %v, graph reads = %d", err, g.reads)
	}
}
