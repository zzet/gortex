package indexer

import (
	"context"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// Each invocation owns its nodes: owner-aware eviction may copy or invalidate
// the canonical contract, so the base and output corpus must not share pointers.
func contextContractWithdrawalCorpus() *graph.Graph {
	outputPath := builderGraphPath(builderRepoPrefix, "handler.go")
	contractPath := builderGraphPath(builderRepoPrefix, "routes.rb")
	ordinaryPath := builderGraphPath(builderRepoPrefix, "helper.go")
	owner := &graph.Node{ID: outputPath + "::Handler", Kind: graph.KindFunction,
		Name: "Handler", FilePath: outputPath, RepoPrefix: builderRepoPrefix}
	contract := &graph.Node{ID: contractPath + "::Route", Kind: graph.KindContract,
		Name: "Route", FilePath: contractPath, RepoPrefix: builderRepoPrefix}
	ordinary := &graph.Node{ID: ordinaryPath + "::Helper", Kind: graph.KindFunction,
		Name: "Helper", FilePath: ordinaryPath, RepoPrefix: builderRepoPrefix}
	corpus := graph.New()
	// EdgeProvides is one of the owner kinds used by Graph.EvictFiles. The
	// edge and its existing source are both recorded at the surviving output
	// path; no synthetic contract-owner metadata is needed for this contract.
	corpus.AddBatch([]*graph.Node{owner, contract, ordinary}, []*graph.Edge{{
		From: owner.ID, To: contract.ID, Kind: graph.EdgeProvides,
		FilePath: outputPath, Line: 3,
	}})
	return corpus
}

func TestContextContractEvictionRetainsCanonicalOwnedByOutput(t *testing.T) {
	corpus := contextContractWithdrawalCorpus()
	contractPath := builderGraphPath(builderRepoPrefix, "routes.rb")
	outputPath := builderGraphPath(builderRepoPrefix, "handler.go")
	corpus.EvictFiles([]string{contractPath})
	got := corpus.GetNode(contractPath + "::Route")
	if got == nil || got.Kind != graph.KindContract || got.FilePath != contractPath {
		t.Fatalf("owner-aware eviction lost or relocated its shared canonical: %+v", got)
	}
	edges := corpus.GetOutEdges(outputPath + "::Handler")
	if len(edges) != 1 || edges[0].Kind != graph.EdgeProvides || edges[0].To != got.ID || edges[0].FilePath != outputPath {
		t.Fatalf("owner-aware eviction changed the surviving output owner: %+v", edges)
	}
}

func TestContextContractWithdrawalRetainsCanonicalAndMasksCoherently(t *testing.T) {
	base := contextContractWithdrawalCorpus()
	corpus := contextContractWithdrawalCorpus()
	contractPath := builderGraphPath(builderRepoPrefix, "routes.rb")
	ordinaryPath := builderGraphPath(builderRepoPrefix, "helper.go")
	outputPath := builderGraphPath(builderRepoPrefix, "handler.go")
	plan := buildPlan{
		indexed: []string{"handler.go", "routes.rb", "helper.go"},
		context: []string{"routes.rb", "helper.go"},
	}
	req := BuildRequest{RepoPrefix: builderRepoPrefix, Base: base}
	store := builderOpenStore(t, "contract-context-withdrawal")
	handle := builderContextDerivedHandle(t, store)
	builder := &SparseGenerationBuilder{Store: store, Logger: zap.NewNop()}
	separation, err := builder.withholdContextPayload(context.Background(), req, plan, corpus)
	if err != nil {
		t.Fatal(err)
	}
	var report BuildReport
	separation.record(&plan, &report)

	// Mirror the in-memory filter's subsequent drain into its private output
	// generation. The unchanged publication guard must accept the result.
	handle.AddBatch(corpus.AllNodes(), corpus.AllEdges())
	if err := builder.writeMasks(req, plan, handle, &report); err != nil {
		t.Fatalf("context separation produced an incoherent generation: %v", err)
	}
	if _, withdrawn := plan.withdrawn[contractPath]; withdrawn {
		t.Fatal("shared contract path was declared read-only context")
	}
	if _, withdrawn := plan.withdrawn[ordinaryPath]; !withdrawn {
		t.Fatal("ordinary unchanged context was retained alongside the contract")
	}
	if !slices.Equal(report.ContextRetainedPaths, []string{contractPath}) {
		t.Fatalf("retained context = %v, want only %s", report.ContextRetainedPaths, contractPath)
	}
	if report.ContextMasks != 1 || report.ReplaceMasks != 2 {
		t.Fatalf("mask counts context=%d replace=%d, want 1 and 2", report.ContextMasks, report.ReplaceMasks)
	}
	if corpus.GetNode(ordinaryPath+"::Helper") != nil || handle.GetNode(ordinaryPath+"::Helper") != nil {
		t.Fatal("withdrawn ordinary context kept payload")
	}
	if got := handle.GetNode(contractPath + "::Route"); got == nil || got.FilePath != contractPath || got.Kind != graph.KindContract {
		t.Fatalf("retained contract payload = %+v", got)
	}
	edges := handle.GetOutEdges(outputPath + "::Handler")
	if len(edges) != 1 || edges[0].Kind != graph.EdgeProvides || edges[0].To != contractPath+"::Route" || edges[0].FilePath != outputPath {
		t.Fatalf("retained contract lost its output owner: %+v", edges)
	}
	for _, node := range handle.AllNodes() {
		if _, withdrawn := plan.withdrawn[node.FilePath]; withdrawn {
			t.Fatalf("node %s survived at withdrawn context %s", node.ID, node.FilePath)
		}
	}
	// The independent base remains intact for composition of the withdrawn
	// ordinary file and was never mutated by the corpus eviction.
	if base.GetNode(ordinaryPath+"::Helper") == nil || base.GetNode(contractPath+"::Route") == nil {
		t.Fatal("context separation mutated the lower layer")
	}
}
