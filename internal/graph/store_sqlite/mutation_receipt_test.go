package store_sqlite

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func openMutationReceiptStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mutation-receipt.sqlite"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	return store
}

func TestSQLiteMutationReceiptCapturesExactResolutionDelta(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: "repo/src/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", QualName: "pkg.Caller", FilePath: "src/a.go", RepoPrefix: "repo"},
		{ID: "repo/src/b.go::Load", Kind: graph.KindFunction, Name: "Load", QualName: "pkg.Load", FilePath: "src/b.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: "repo/src/a.go::Caller", To: "repo::" + graph.UnresolvedMarker + "Load", Kind: graph.EdgeImports,
		FilePath: "src/a.go", Alias: "loader",
	}})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want complete resolution delta", receipt)
	}
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
	if want := []string{"src/a.go"}; !slices.Equal(receipt.UnresolvedFiles, want) {
		t.Fatalf("unresolved files = %v, want %v", receipt.UnresolvedFiles, want)
	}
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(receipt.CrossRepoFiles(), want) {
		t.Fatalf("cross-repo files = %v, want %v", receipt.CrossRepoFiles(), want)
	}
	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "Caller", "Load", "pkg.Caller", "pkg.Load")
	assertSQLiteReceiptContains(t, "target ids", receipt.TargetIDs,
		"repo/src/a.go::Caller", "repo/src/b.go::Load", "repo::"+graph.UnresolvedMarker+"Load")
	assertSQLiteReceiptContains(t, "import candidates", receipt.ImportCandidates, "Load", "loader")
}

func TestSQLiteMutationReceiptCapturesResolvedEdgeFrontier(t *testing.T) {
	store := openMutationReceiptStore(t)
	const (
		sourceID = "repo/src/a.go::Caller"
		targetID = "other/pkg::Target"
	)
	store.AddBatch([]*graph.Node{
		{ID: sourceID, Kind: graph.KindFunction, Name: "Caller", FilePath: "src/a.go", RepoPrefix: "repo"},
		{ID: targetID, Kind: graph.KindFunction, Name: "Target", FilePath: "target.go", RepoPrefix: "other"},
	}, nil)

	token := store.BeginMutationReceipt()
	store.AddEdge(&graph.Edge{
		From: sourceID, To: targetID, Kind: graph.EdgeImports, Alias: "dependency",
	})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want complete resolved-edge frontier", receipt)
	}
	assertSQLiteReceiptContains(t, "changed files", receipt.ChangedFiles, "src/a.go")
	if len(receipt.UnresolvedFiles) != 0 || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("resolved edge entered unresolved frontier: %+v", receipt)
	}
	if want := []string{"src/a.go"}; !slices.Equal(receipt.CrossRepoFiles(), want) {
		t.Fatalf("cross-repo files = %v, want %v", receipt.CrossRepoFiles(), want)
	}
	assertSQLiteReceiptContains(t, "target ids", receipt.TargetIDs, targetID)
	assertSQLiteReceiptContains(t, "import candidates", receipt.ImportCandidates, targetID, "dependency")
}

func TestSQLiteMutationReceiptIdempotentAndAttributeOnlyWritesAreNeutral(t *testing.T) {
	store := openMutationReceiptStore(t)
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
	edge := &graph.Edge{From: node.ID, To: "repo::" + graph.UnresolvedMarker + "B", Kind: graph.EdgeCalls, FilePath: "a.go"}
	store.AddNode(node)
	store.AddEdge(edge)

	token := store.BeginMutationReceipt()
	store.AddNode(node)
	store.AddEdge(edge)
	store.PersistEdgeAttributes(&graph.Edge{
		From: edge.From, To: edge.To, Kind: edge.Kind, FilePath: edge.FilePath,
		Confidence: 0.9, ConfidenceLabel: "HIGH", Origin: "semantic", Tier: "semantic",
	})
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete {
		t.Fatalf("neutral receipt unexpectedly incomplete: %+v", receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.UnresolvedFiles) != 0 || len(receipt.ResolutionFiles()) != 0 || len(receipt.CrossRepoFiles()) != 0 {
		t.Fatalf("neutral writes produced mutation frontiers: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptIdentityChangingUpsertCapturesExactFrontier(t *testing.T) {
	const id = "repo/a.go::A"
	tests := []struct {
		name      string
		before    graph.Node
		after     graph.Node
		wantFiles []string
		wantNames []string
	}{
		{
			name:      "rename",
			before:    graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			after:     graph.Node{ID: id, Kind: graph.KindFunction, Name: "Renamed", QualName: "pkg.Renamed", FilePath: "a.go", RepoPrefix: "repo"},
			wantFiles: []string{"a.go"},
			wantNames: []string{"A", "pkg.A", "Renamed", "pkg.Renamed"},
		},
		{
			name:      "move",
			before:    graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			after:     graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "b.go", RepoPrefix: "repo"},
			wantFiles: []string{"a.go", "b.go"},
			wantNames: []string{"A", "pkg.A"},
		},
		{
			name:      "referenceable kind transition",
			before:    graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			after:     graph.Node{ID: id, Kind: graph.KindMethod, Name: "A", QualName: "pkg.T.A", FilePath: "a.go", RepoPrefix: "repo"},
			wantFiles: []string{"a.go"},
			wantNames: []string{"A", "pkg.A", "pkg.T.A"},
		},
		{
			name:      "gains referenceability",
			before:    graph.Node{ID: id, Kind: graph.KindFile, Name: "source", FilePath: "a.go", RepoPrefix: "repo"},
			after:     graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			wantFiles: []string{"a.go"},
			wantNames: []string{"source", "A", "pkg.A"},
		},
		{
			name:      "loses referenceability",
			before:    graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			after:     graph.Node{ID: id, Kind: graph.KindFile, Name: "source", FilePath: "a.go", RepoPrefix: "repo"},
			wantFiles: []string{"a.go"},
			wantNames: []string{"A", "pkg.A", "source"},
		},
		{
			name:      "repository transition",
			before:    graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "old/a.go", RepoPrefix: "old"},
			after:     graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "new/a.go", RepoPrefix: "new"},
			wantFiles: []string{"new/a.go", "old/a.go"},
			wantNames: []string{"A", "pkg.A"},
		},
		{
			name:      "missing nonreferenceable side file remains exact",
			before:    graph.Node{ID: id, Kind: graph.KindFile, Name: "source", RepoPrefix: "repo"},
			after:     graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			wantFiles: []string{"a.go"},
			wantNames: []string{"source", "A", "pkg.A"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openMutationReceiptStore(t)
			store.AddNode(&tt.before)

			token := store.BeginMutationReceipt()
			store.AddNode(&tt.after)
			receipt := store.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant {
				t.Fatalf("identity-changing UPSERT receipt = %+v, want complete resolution delta", receipt)
			}
			if !slices.Equal(receipt.ResolutionFiles(), tt.wantFiles) {
				t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), tt.wantFiles)
			}
			assertSQLiteReceiptContains(t, "target ids", receipt.TargetIDs, id)
			assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, tt.wantNames...)
		})
	}
}

func TestSQLiteMutationReceiptNonreferenceableIdentityChangeIsNeutral(t *testing.T) {
	store := openMutationReceiptStore(t)
	const id = "repo/a.go"
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: "old", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: "new", FilePath: "b.go", RepoPrefix: "other"})
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("nonreferenceable identity change receipt = %+v, want complete and resolution-irrelevant", receipt)
	}
}

func TestSQLiteMutationReceiptIdentityChangeMissingReferenceableFileFailsClosed(t *testing.T) {
	const id = "repo/a.go::A"
	tests := []struct {
		name   string
		before graph.Node
		after  graph.Node
	}{
		{
			name:   "old identity file missing",
			before: graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", RepoPrefix: "repo"},
			after:  graph.Node{ID: id, Kind: graph.KindFunction, Name: "Renamed", QualName: "pkg.Renamed", FilePath: "a.go", RepoPrefix: "repo"},
		},
		{
			name:   "final identity file missing",
			before: graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
			after:  graph.Node{ID: id, Kind: graph.KindFunction, Name: "Renamed", QualName: "pkg.Renamed", RepoPrefix: "repo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openMutationReceiptStore(t)
			store.AddNode(&tt.before)
			token := store.BeginMutationReceipt()
			store.AddNode(&tt.after)
			receipt := store.EndMutationReceipt(token)
			if receipt.Complete || !receipt.ResolutionRelevant {
				t.Fatalf("missing-file identity change receipt = %+v, want incomplete resolution delta", receipt)
			}
			if receipt.IncompleteReason != "node_identity_change_without_exact_file" {
				t.Fatalf("incomplete reason = %q, want node_identity_change_without_exact_file", receipt.IncompleteReason)
			}
		})
	}
}

func TestSQLiteMutationReceiptDuplicateNodeBatchNamesIncompleteReason(t *testing.T) {
	store := openMutationReceiptStore(t)
	const id = "repo/a.go::A"
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: id, Kind: graph.KindFunction, Name: "FirstRename", QualName: "pkg.FirstRename", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: id, Kind: graph.KindFunction, Name: "FinalRename", QualName: "pkg.FinalRename", FilePath: "a.go", RepoPrefix: "repo"},
	}, nil)
	receipt := store.EndMutationReceipt(token)
	if receipt.Complete {
		t.Fatalf("identity-changing duplicate batch returned complete receipt: %+v", receipt)
	}
	if receipt.IncompleteReason != "duplicate_node_batch" {
		t.Fatalf("incomplete reason = %q, want duplicate_node_batch", receipt.IncompleteReason)
	}
}

func TestSQLiteMutationReceiptDuplicateContractOwnersRemainExact(t *testing.T) {
	store := openMutationReceiptStore(t)
	const (
		contractID = "ws::error"
		providerID = "trellis/provider.go::serveError"
		consumerID = "axonhub/consumer.js::syncError"
	)
	store.AddBatch([]*graph.Node{
		{ID: contractID, Kind: graph.KindContract, Name: contractID, FilePath: "old/socket.go", RepoPrefix: "trellis", Meta: map[string]any{"role": "provider"}},
		{ID: providerID, Kind: graph.KindFunction, Name: "serveError", FilePath: "provider.go", RepoPrefix: "trellis"},
		{ID: consumerID, Kind: graph.KindFunction, Name: "syncError", FilePath: "consumer.js", RepoPrefix: "axonhub"},
	}, nil)

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: contractID, Kind: graph.KindContract, Name: contractID, FilePath: "provider.go", RepoPrefix: "trellis", Meta: map[string]any{"role": "provider"}},
		{ID: contractID, Kind: graph.KindContract, Name: contractID, FilePath: "consumer.js", RepoPrefix: "axonhub", Meta: map[string]any{"role": "consumer"}},
	}, []*graph.Edge{
		{From: providerID, To: contractID, Kind: graph.EdgeProvides, FilePath: "provider.go", Line: 10},
		{From: consumerID, To: contractID, Kind: graph.EdgeConsumes, FilePath: "consumer.js", Line: 20},
	})
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || receipt.IncompleteReason != "" {
		t.Fatalf("duplicate contract receipt = %+v, want complete and resolution-irrelevant", receipt)
	}
	if got := store.GetNode(contractID); got == nil || got.FilePath != "consumer.js" || got.RepoPrefix != "axonhub" || got.Meta["role"] != "consumer" {
		t.Fatalf("final contract node = %+v, want last owner representation", got)
	}
	if edges := store.GetOutEdges(providerID); len(edges) != 1 || edges[0].To != contractID || edges[0].Kind != graph.EdgeProvides {
		t.Fatalf("provider edges = %+v, want retained provides edge", edges)
	}
	if edges := store.GetOutEdges(consumerID); len(edges) != 1 || edges[0].To != contractID || edges[0].Kind != graph.EdgeConsumes {
		t.Fatalf("consumer edges = %+v, want retained consumes edge", edges)
	}
}

func TestSQLiteMutationReceiptDuplicateSemanticEnrichmentKeepsExactReceipt(t *testing.T) {
	store := openMutationReceiptStore(t)
	const id = "repo/a.go::A"
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "string", "semantic_source": "lsp"}},
		{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "number", "semantic_source": "lsp"}},
	}, nil)
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant {
		t.Fatalf("enrichment-only duplicate batch receipt = %+v, want complete and resolution-irrelevant", receipt)
	}
	if len(receipt.UnresolvedFiles) != 0 || len(receipt.ResolutionFiles()) != 0 || len(receipt.CrossRepoFiles()) != 0 {
		t.Fatalf("enrichment-only duplicate batch produced mutation frontiers: %+v", receipt)
	}
	if got := store.GetNode(id); got == nil || got.Meta["semantic_type"] != "number" {
		t.Fatalf("final enriched node = %+v, want semantic_type number", got)
	}
}

func TestSQLiteMutationReceiptNewDuplicateNodeRecordsFinalIdentity(t *testing.T) {
	store := openMutationReceiptStore(t)
	const id = "repo/a.go::A"

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "string"}},
		{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo", Meta: map[string]any{"semantic_type": "number"}},
	}, nil)
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("new duplicate batch receipt = %+v, want complete resolution delta", receipt)
	}
	if want := []string{"a.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "A", "pkg.A")
	assertSQLiteReceiptContains(t, "target ids", receipt.TargetIDs, id)
	if got := store.GetNode(id); got == nil || got.Meta["semantic_type"] != "number" {
		t.Fatalf("final new node = %+v, want semantic_type number", got)
	}
}

func TestSQLiteMutationReceiptBatchRollbackPublishesNothing(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("invalid batch unexpectedly succeeded")
			}
		}()
		store.AddBatch([]*graph.Node{
			{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"},
			{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "b.go", RepoPrefix: "repo", Meta: map[string]any{"unsupported": make(chan int)}},
		}, nil)
	}()
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("rolled-back batch leaked receipt events: %+v", receipt)
	}
	if node := store.GetNode("repo/a.go::A"); node != nil {
		t.Fatalf("rolled-back batch leaked node: %+v", node)
	}
}

func TestSQLiteMutationReceiptsOverlapWithoutStealingEvents(t *testing.T) {
	store := openMutationReceiptStore(t)
	outer := store.BeginMutationReceipt()
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
	inner := store.BeginMutationReceipt()
	store.AddNode(&graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "b.go", RepoPrefix: "repo"})
	outerReceipt := store.EndMutationReceipt(outer)
	store.AddNode(&graph.Node{ID: "repo/c.go::C", Kind: graph.KindFunction, Name: "C", FilePath: "c.go", RepoPrefix: "repo"})
	innerReceipt := store.EndMutationReceipt(inner)

	assertSQLiteReceiptContains(t, "outer files", outerReceipt.ResolutionFiles(), "a.go", "b.go")
	if slices.Contains(outerReceipt.ResolutionFiles(), "c.go") {
		t.Fatalf("outer receipt observed mutation after it ended: %+v", outerReceipt)
	}
	assertSQLiteReceiptContains(t, "inner files", innerReceipt.ResolutionFiles(), "b.go", "c.go")
	if slices.Contains(innerReceipt.ResolutionFiles(), "a.go") {
		t.Fatalf("inner receipt observed mutation before it began: %+v", innerReceipt)
	}
}

func TestSQLiteMutationReceiptsConcurrentOverlap(t *testing.T) {
	store := openMutationReceiptStore(t)
	const workers = 12
	ready := sync.WaitGroup{}
	ready.Add(workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			token := store.BeginMutationReceipt()
			ready.Done()
			<-start
			id := string(rune('a' + i))
			store.AddNode(&graph.Node{ID: "repo/" + id + ".go::" + id, Kind: graph.KindFunction, Name: id, FilePath: id + ".go", RepoPrefix: "repo"})
			receipt := store.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.DefinitionFiles, id+".go") {
				t.Errorf("concurrent receipt %d = %+v", i, receipt)
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
}

func TestSQLiteMutationReceiptBoundaryWaitsForInFlightWrite(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()

	store.writeMu.Lock()
	started := make(chan struct{})
	receiptCh := make(chan graph.MutationReceipt, 1)
	go func() {
		close(started)
		receiptCh <- store.EndMutationReceipt(token)
	}()
	<-started
	select {
	case receipt := <-receiptCh:
		store.writeMu.Unlock()
		t.Fatalf("EndMutationReceipt overtook in-flight write: %+v", receipt)
	case <-time.After(20 * time.Millisecond):
	}

	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
	changed, err := store.insertNodeLocked(store.stmtInsertNode, node)
	if err != nil {
		store.writeMu.Unlock()
		t.Fatalf("insert node: %v", err)
	}
	delta := newSQLiteMutationReceiptAccumulator()
	recordSQLiteAddedNode(delta, node)
	if changed {
		store.mergeMutationReceiptLocked(delta)
	}
	store.writeMu.Unlock()

	select {
	case receipt := <-receiptCh:
		if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.DefinitionFiles, "a.go") {
			t.Fatalf("receipt missed in-flight write: %+v", receipt)
		}
	case <-time.After(time.Second):
		t.Fatal("EndMutationReceipt did not complete after write drained")
	}
}

func TestSQLiteMutationReceiptEdgeSourceFileFallback(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddEdge(&graph.Edge{From: "repo/a.go::A", To: "repo::" + graph.UnresolvedMarker + "B", Kind: graph.EdgeCalls})
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.ChangedFiles, "a.go") ||
		!slices.Contains(receipt.UnresolvedFiles, "a.go") || !slices.Contains(receipt.CrossRepoFiles(), "a.go") {
		t.Fatalf("source-file fallback receipt = %+v", receipt)
	}

	missing := store.BeginMutationReceipt()
	store.AddEdge(&graph.Edge{From: "repo/missing.go::Missing", To: "repo::" + graph.UnresolvedMarker + "C", Kind: graph.EdgeCalls})
	if receipt := store.EndMutationReceipt(missing); receipt.Complete {
		t.Fatalf("missing source file returned complete receipt: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptNoOpMutationsStayComplete(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	if store.RemoveEdge("missing", "missing", graph.EdgeCalls) {
		t.Fatal("missing edge unexpectedly removed")
	}
	if err := store.PurgeRepo("missing"); err != nil {
		t.Fatalf("purge missing repo: %v", err)
	}
	if err := store.RekeyRepoPrefix("missing", "new"); err != nil {
		t.Fatalf("rekey missing repo: %v", err)
	}
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("no-op mutations changed receipt: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptPurgeAndRekeyFailClosedAfterChange(t *testing.T) {
	t.Run("purge", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
		token := store.BeginMutationReceipt()
		if err := store.PurgeRepo("repo"); err != nil {
			t.Fatalf("purge repo: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("purge returned complete receipt: %+v", receipt)
		}
	})

	t.Run("purge sidecar only", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		if err := store.SetFileMtime("repo", "a.go", 1); err != nil {
			t.Fatalf("seed file mtime: %v", err)
		}
		token := store.BeginMutationReceipt()
		if err := store.PurgeRepo("repo"); err != nil {
			t.Fatalf("purge repo: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("sidecar-only purge returned complete receipt: %+v", receipt)
		}
	})

	t.Run("rekey", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		if err := store.SetFileMtime("old", "a.go", 1); err != nil {
			t.Fatalf("seed file mtime: %v", err)
		}
		token := store.BeginMutationReceipt()
		if err := store.RekeyRepoPrefix("old", "new"); err != nil {
			t.Fatalf("rekey repo: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("rekey returned complete receipt: %+v", receipt)
		}
	})
}

func TestSQLiteMutationReceiptBulkBoundariesFailClosed(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		token := store.BeginMutationReceipt()
		store.BeginBulkLoad()
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("BeginBulkLoad returned complete receipt: %+v", receipt)
		}
		if err := store.FlushBulk(); err != nil {
			t.Fatalf("flush bulk: %v", err)
		}
	})

	t.Run("flush", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.BeginBulkLoad()
		token := store.BeginMutationReceipt()
		if err := store.FlushBulk(); err != nil {
			t.Fatalf("flush bulk: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("FlushBulk returned complete receipt: %+v", receipt)
		}
	})
}

func TestSQLiteMutationReceiptTopologyMutations(t *testing.T) {
	t.Run("reindex edge", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		edge := &graph.Edge{From: "repo/a.go::A", To: "repo/old.go::Old", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddEdge(edge)
		updated := *edge
		updated.To = "repo/new.go::New"
		token := store.BeginMutationReceipt()
		store.ReindexEdge(&updated, edge.To)
		if receipt := store.EndMutationReceipt(token); !receipt.Complete || receipt.ResolutionRelevant {
			t.Fatalf("ReindexEdge receipt = %+v, want complete and resolution-irrelevant", receipt)
		}

		noop := store.BeginMutationReceipt()
		store.ReindexEdge(&updated, updated.To)
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op ReindexEdge returned incomplete receipt: %+v", receipt)
		}
	})

	t.Run("reindex edges", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		edge := &graph.Edge{From: "repo/a.go::A", To: "repo/old.go::Old", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddEdge(edge)
		updated := *edge
		updated.To = "repo/new.go::New"
		token := store.BeginMutationReceipt()
		store.ReindexEdges([]graph.EdgeReindex{{Edge: &updated, OldTo: edge.To}})
		if receipt := store.EndMutationReceipt(token); !receipt.Complete || receipt.ResolutionRelevant {
			t.Fatalf("ReindexEdges receipt = %+v, want complete and resolution-irrelevant", receipt)
		}

		noop := store.BeginMutationReceipt()
		store.ReindexEdges([]graph.EdgeReindex{{Edge: &updated, OldTo: updated.To}})
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op ReindexEdges returned incomplete receipt: %+v", receipt)
		}
	})

	t.Run("evict file", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
		token := store.BeginMutationReceipt()
		nodes, _ := store.EvictFile("a.go")
		if nodes != 1 {
			t.Fatalf("EvictFile nodes = %d, want 1", nodes)
		}
		receipt := store.EndMutationReceipt(token)
		if !receipt.Complete || !receipt.ResolutionRelevant {
			t.Fatalf("EvictFile receipt = %+v, want complete exact eviction frontier", receipt)
		}
		if want := []string{"a.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
			t.Fatalf("EvictFile definition files = %v, want %v", receipt.DefinitionFiles, want)
		}
		assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "A")

		noop := store.BeginMutationReceipt()
		store.EvictFile("missing.go")
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op EvictFile returned incomplete receipt: %+v", receipt)
		}
	})

	t.Run("evict repo", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
		token := store.BeginMutationReceipt()
		nodes, _ := store.EvictRepo("repo")
		if nodes != 1 {
			t.Fatalf("EvictRepo nodes = %d, want 1", nodes)
		}
		// EvictRepo defaults to the calling handle's generation
		// (EvictRepoCurrentGeneration), so it names a bounded, exactly
		// describable set of doomed nodes and keeps the receipt complete -
		// mirroring the file eviction above. The unbounded all-generation
		// sweep (EvictRepoAllGenerations) is what fails the receipt closed.
		receipt := store.EndMutationReceipt(token)
		if !receipt.Complete || !receipt.ResolutionRelevant {
			t.Fatalf("EvictRepo receipt = %+v, want complete generation-scoped eviction frontier", receipt)
		}
		if want := []string{"a.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
			t.Fatalf("EvictRepo definition files = %v, want %v", receipt.DefinitionFiles, want)
		}
		assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "A")

		noop := store.BeginMutationReceipt()
		store.EvictRepo("missing")
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op EvictRepo returned incomplete receipt: %+v", receipt)
		}
	})
}

func TestSQLiteMutationReceiptReindexEdgeRollbackIsAtomic(t *testing.T) {
	store := openMutationReceiptStore(t)
	edge := &graph.Edge{From: "repo/a.go::A", To: "repo/old.go::Old", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
	store.AddEdge(edge)
	bad := *edge
	bad.To = "repo/new.go::New"
	bad.Meta = map[string]any{"unsupported": make(chan int)}

	token := store.BeginMutationReceipt()
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("invalid ReindexEdge unexpectedly succeeded")
			}
		}()
		store.ReindexEdge(&bad, edge.To)
	}()
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("rolled-back ReindexEdge changed receipt: %+v", receipt)
	}
	edges := store.GetOutEdges(edge.From)
	if len(edges) != 1 || edges[0].To != edge.To {
		t.Fatalf("rolled-back ReindexEdge changed stored topology: %+v", edges)
	}
}

func TestSQLiteMutationReceiptProvenanceOnlyWriteIsNeutral(t *testing.T) {
	store := openMutationReceiptStore(t)
	edge := &graph.Edge{
		From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls,
		FilePath: "a.go", Line: 1, Origin: "heuristic", Tier: "heuristic",
	}
	store.AddEdge(edge)
	token := store.BeginMutationReceipt()
	if !store.SetEdgeProvenance(edge, "semantic") {
		t.Fatal("SetEdgeProvenance did not update edge")
	}
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("provenance-only write changed receipt: %+v", receipt)
	}
}

func TestSQLiteDuplicateEdgeWritesPreservePersistedAnalysis(t *testing.T) {
	t.Run("AddEdge", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		edge := &graph.Edge{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddEdge(edge)
		buildMinimalAnalysisGeneration(t, store, "add-edge-noop", 0, true)
		before := store.AnalysisMutationRevision()

		store.AddEdge(edge)
		if after := store.AnalysisMutationRevision(); after != before {
			t.Fatalf("duplicate AddEdge advanced analysis revision: before=%d after=%d", before, after)
		}
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if !found {
			t.Fatal("duplicate AddEdge discarded active analysis")
		}
	})

	t.Run("AddBatch", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
		edge := &graph.Edge{From: node.ID, To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddBatch([]*graph.Node{node}, []*graph.Edge{edge})
		buildMinimalAnalysisGeneration(t, store, "add-batch-noop", 0, true)
		before := store.AnalysisMutationRevision()

		store.AddBatch([]*graph.Node{nil, node}, []*graph.Edge{nil, edge})
		if after := store.AnalysisMutationRevision(); after != before {
			t.Fatalf("duplicate AddBatch advanced analysis revision: before=%d after=%d", before, after)
		}
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if !found {
			t.Fatal("duplicate AddBatch discarded active analysis")
		}
	})

	t.Run("new AddEdge invalidates", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		buildMinimalAnalysisGeneration(t, store, "add-edge-change", 0, true)
		store.AddEdge(&graph.Edge{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1})
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if found {
			t.Fatal("new AddEdge preserved stale active analysis")
		}
	})

	t.Run("new AddBatch edge invalidates", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		buildMinimalAnalysisGeneration(t, store, "add-batch-change", 0, true)
		store.AddBatch(nil, []*graph.Edge{{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}})
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if found {
			t.Fatal("new AddBatch edge preserved stale active analysis")
		}
	})

	t.Run("filtered batch", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		buildMinimalAnalysisGeneration(t, store, "filtered-batch-noop", 0, true)
		store.AddBatch([]*graph.Node{nil}, []*graph.Edge{nil})
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if !found {
			t.Fatal("filtered AddBatch discarded active analysis")
		}
	})
}

func TestSQLitePurgeInvalidatesPersistedAnalysis(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
	buildMinimalAnalysisGeneration(t, store, "purge", 0, true)
	before := store.AnalysisMutationRevision()

	if err := store.PurgeRepo("repo"); err != nil {
		t.Fatalf("purge repo: %v", err)
	}
	if after := store.AnalysisMutationRevision(); after <= before {
		t.Fatalf("analysis revision did not advance: before=%d after=%d", before, after)
	}
	if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
		t.Fatalf("load active analysis: %v", err)
	} else if found {
		t.Fatal("purge left stale analysis generation active")
	}
}

func TestSQLiteMutationReceiptUnknownTokenFailsClosed(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	_ = store.EndMutationReceipt(token)
	if receipt := store.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("already-ended token returned complete receipt: %+v", receipt)
	}
}

func assertSQLiteReceiptContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	for _, value := range want {
		if !slices.Contains(got, value) {
			t.Errorf("%s = %v, missing %q", label, got, value)
		}
	}
}

func TestSQLiteMutationReceiptEvictFilesCapturesExactFrontier(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go::B", Kind: graph.KindType, Name: "B", FilePath: "b.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: graph.KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: graph.EdgeCalls, FilePath: "keep.go", Line: 3,
	}})

	// Mirror the reindex composition: the surviving caller's edge is parked
	// under an unresolved stub BEFORE the eviction, so no resolved incoming
	// edge from a surviving source is deleted and the receipt can stay exact.
	token := store.BeginMutationReceipt()
	in := store.GetInEdgesByNodeIDs([]string{"repo/a.go::A"})
	stub := in["repo/a.go::A"][0]
	oldTo := stub.To
	graph.StashRestubProvenance(stub)
	stub.To = graph.UnresolvedMarker + "A"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: stub, OldTo: oldTo}})
	nodes, edges := store.EvictFiles([]string{"a.go", "b.go"})
	receipt := store.EndMutationReceipt(token)

	if nodes != 2 || edges != 0 {
		t.Fatalf("EvictFiles removed nodes=%d edges=%d, want 2/0 (the restubbed edge no longer touches the doomed nodes)", nodes, edges)
	}
	if !receipt.Complete {
		t.Fatalf("EvictFiles receipt incomplete (%q), want exact frontier: %+v", receipt.IncompleteReason, receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("evicting referenceable definitions must be resolution-relevant: %+v", receipt)
	}
	if want := []string{"a.go", "b.go"}; !slices.Equal(receipt.DefinitionFiles, want) {
		t.Fatalf("definition files = %v, want %v", receipt.DefinitionFiles, want)
	}
	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "A", "pkg.A", "B")
	assertSQLiteReceiptContains(t, "target ids", receipt.TargetIDs, "repo/a.go::A", "repo/b.go::B")
	assertSQLiteReceiptContains(t, "changed files", receipt.ChangedFiles, "a.go", "b.go")
	if slices.Contains(receipt.DefinitionFiles, "keep.go") {
		t.Fatalf("unrelated file leaked into definition frontier: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptEvictFilesNonreferenceableOnlyStaysNeutral(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddNode(&graph.Node{ID: "repo/doc.md::note", Kind: graph.KindImport, Name: "note", FilePath: "doc.md", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.EvictFiles([]string{"doc.md"})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("non-referenceable eviction receipt incomplete (%q): %+v", receipt.IncompleteReason, receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("non-referenceable eviction entered the resolution frontier: %+v", receipt)
	}
	assertSQLiteReceiptContains(t, "changed files", receipt.ChangedFiles, "doc.md")
}

// SQLite twin of the graph-side restub-write pin: the parked stub keeps the
// receipt complete and resolution-relevant, contributes its name, and keeps
// the surviving caller's file out of UnresolvedFiles.
func TestSQLiteMutationReceiptRestubWriteStaysOutOfUnresolvedFiles(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: graph.KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: graph.EdgeCalls,
		FilePath: "keep.go", Line: 3, Origin: graph.OriginLSPResolved, Confidence: 1,
	}})
	in := store.GetInEdgesByNodeIDs([]string{"repo/a.go::A"})
	if len(in["repo/a.go::A"]) != 1 {
		t.Fatalf("expected one incoming edge, got %d", len(in["repo/a.go::A"]))
	}
	e := in["repo/a.go::A"][0]

	token := store.BeginMutationReceipt()
	oldTo := e.To
	graph.StashRestubProvenance(e)
	e.To = graph.UnresolvedMarker + "A"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: e, OldTo: oldTo}})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("restub write must not void the receipt: %+v", receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("a parked stub still needs the name frontier: %+v", receipt)
	}
	if slices.Contains(receipt.UnresolvedFiles, "keep.go") {
		t.Fatalf("restubbed caller file leaked into UnresolvedFiles (forward pass would demote its tier): %+v", receipt)
	}
	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "A")
}

// SQLite twin: an eviction deleting a resolved incoming edge from a surviving
// source still describes its resolution delta exactly, on every active
// window. No pass reconstructs a deleted edge, so failing closed would only
// buy a larger pass onto the same graph.
func TestSQLiteMutationReceiptEvictSurvivingIncomingEdgeStaysExact(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: graph.KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: "repo/keep.go::Keep", To: "repo/a.go::A", Kind: graph.EdgeCalls, FilePath: "keep.go", Line: 3,
	}})

	outer := store.BeginMutationReceipt()
	inner := store.BeginMutationReceipt()
	store.EvictFile("a.go")
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})
	innerReceipt := store.EndMutationReceipt(inner)
	outerReceipt := store.EndMutationReceipt(outer)

	if !innerReceipt.Complete || !innerReceipt.ResolutionRelevant {
		t.Fatalf("inner receipt = %+v, want a complete resolution-relevant delta", innerReceipt)
	}
	if !outerReceipt.Complete || !outerReceipt.ResolutionRelevant {
		t.Fatalf("outer receipt = %+v, want a complete resolution-relevant delta", outerReceipt)
	}
	assertSQLiteReceiptContains(t, "evicted names", innerReceipt.EvictedNames, "A", "pkg.A")
	if edges := store.GetOutEdges("repo/keep.go::Keep"); len(edges) != 0 {
		t.Fatalf("surviving caller edges = %v, want the eviction to have destroyed it", edges)
	}
}

// SQLite twin of the non-referenceable case: the surviving file's import edge
// into a doomed package node is destroyed, and the package's own import stubs
// are what the receipt has to describe.
func TestSQLiteMutationReceiptEvictImportToNonreferenceableStaysExact(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::pkg", Kind: graph.KindPackage, Name: "pkg", QualName: "example/pkg", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::Keep", Kind: graph.KindFunction, Name: "Keep", FilePath: "keep.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: "repo/keep.go::Keep", To: "repo/a.go::pkg", Kind: graph.EdgeImports, FilePath: "keep.go",
	}})

	token := store.BeginMutationReceipt()
	store.EvictFiles([]string{"a.go"})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
	}
	assertSQLiteReceiptContains(t, "evicted names", receipt.EvictedNames, "import::example/pkg", "import::pkg")
	if edges := store.GetOutEdges("repo/keep.go::Keep"); len(edges) != 0 {
		t.Fatalf("surviving importer edges = %v, want the eviction to have destroyed it", edges)
	}
}

func TestSQLiteMutationReceiptEvictAmbiguousImportPackageCandidateRecordsImportStub(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{{
		ID: "one::pkg", Kind: graph.KindPackage, Name: "pkg", QualName: "example/pkg",
		FilePath: "a/one.go", RepoPrefix: "one",
	}}, nil)

	token := store.BeginMutationReceipt()
	store.EvictFiles([]string{"a/one.go"})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want a complete resolution-relevant delta", receipt)
	}
	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "import::example/pkg", "import::pkg")
	assertSQLiteReceiptContains(t, "evicted names", receipt.EvictedNames, "import::example/pkg", "import::pkg")
	if want := []string{"a/one.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
}

// SQLite mirror: evicting a file node contributes no stub name, but a file
// node is an import candidate, so the receipt must stay resolution-relevant
// and put the file in the frontier rather than certify the eviction neutral.
func TestSQLiteMutationReceiptEvictNamelessNodeStaysResolutionRelevant(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repo/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", RepoPrefix: "repo"},
	}, nil)

	token := store.BeginMutationReceipt()
	store.EvictFiles([]string{"b.go"})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("receipt = %+v, want complete", receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want resolution-relevant: an evicted file node can rebind a pending import", receipt)
	}
	if files := receipt.ResolutionFiles(); !slices.Contains(files, "b.go") {
		t.Fatalf("resolution files = %v, want the evicted file in the frontier", files)
	}
}

// The SQLite mirror of the Graph EvictedNames split: the name frontier's input
// carries the vanished definition and not the added one, which the file
// frontier already reaches through DefinitionFiles.
func TestSQLiteMutationReceiptEvictedNamesCarryOnlyVanishedDefinitions(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{{
		ID: "repo/gone.go::Gone", Kind: graph.KindFunction, Name: "Gone", QualName: "pkg.Gone",
		FilePath: "gone.go", RepoPrefix: "repo",
	}}, nil)

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{{
		ID: "repo/new.go::Added", Kind: graph.KindFunction, Name: "Added", QualName: "pkg.Added",
		FilePath: "new.go", RepoPrefix: "repo",
	}}, nil)
	store.EvictFiles([]string{"gone.go"})
	receipt := store.EndMutationReceipt(token)

	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "Added", "pkg.Added", "Gone", "pkg.Gone")
	assertSQLiteReceiptContains(t, "evicted names", receipt.EvictedNames, "Gone", "pkg.Gone")
	for _, added := range []string{"Added", "pkg.Added"} {
		if slices.Contains(receipt.EvictedNames, added) {
			t.Fatalf("evicted names %v include the added definition %q", receipt.EvictedNames, added)
		}
	}
}

func TestSQLiteMutationReceiptEvictUnmappedImportCandidateKindFailsClosed(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddBatch([]*graph.Node{{
		ID: "one::mod", Kind: graph.KindModule, Name: "mod", QualName: "example/pkg",
		FilePath: "m/mod.go", RepoPrefix: "one",
	}}, nil)

	token := store.BeginMutationReceipt()
	store.EvictFiles([]string{"m/mod.go"})
	receipt := store.EndMutationReceipt(token)

	if receipt.Complete {
		t.Fatalf("receipt = %+v, want incomplete: the kind is a qualified-name import candidate without an exact stub mapping", receipt)
	}
}
