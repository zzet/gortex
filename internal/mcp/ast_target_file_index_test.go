package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

type astTargetBoundedStore struct {
	graph.Store
	calls []boundedFileCall
	read  func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error)
}

func (store *astTargetBoundedStore) AllNodes() []*graph.Node {
	panic("AST enclosing lookup must not call AllNodes")
}

func (store *astTargetBoundedStore) FindFileNodesBounded(
	ctx context.Context,
	path string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	store.calls = append(store.calls, boundedFileCall{path: path, scope: scope, limit: limit})
	return store.read(ctx, path, scope, limit)
}

type astTargetUnboundedStore struct{ graph.Store }

func (store *astTargetUnboundedStore) AllNodes() []*graph.Node {
	panic("missing bounded capability must fail closed without AllNodes")
}

func TestEnrichASTMatchesPreservesOrderAndDeduplicates(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	server := &Server{graph: probe}
	matches := []astquery.Match{
		{File: "repo/b.ts", Line: 1},
		{File: "repo/a.go", Line: 1},
		{File: "repo/b.ts", Line: 2},
		{File: "", Line: 1},
		{File: "repo/c.py", Line: 1},
	}

	server.enrichASTMatchesContext(context.Background(), matches)

	want := []string{"repo/b.ts", "repo/a.go", "repo/c.py"}
	if len(probe.calls) != len(want) {
		t.Fatalf("bounded calls = %#v, want %v", probe.calls, want)
	}
	for index, path := range want {
		if probe.calls[index].path != path || probe.calls[index].limit != localizationFileNodeLimit {
			t.Fatalf("bounded call %d = %#v, want path=%q limit=%d", index, probe.calls[index], path, localizationFileNodeLimit)
		}
	}
}

func TestEnrichASTMatchesSharesRequestBudget(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{
			Nodes: []*graph.Node{{
				ID: path + "::owner", Name: "owner", Kind: graph.KindFunction,
				FilePath: path, StartLine: 1, EndLine: 2,
			}},
			Total: limit,
		}, nil
	}
	server := &Server{graph: probe}
	ctx := withLocalizationFileRequestBudget(context.Background())
	first := []astquery.Match{{File: "repo/a.go", Line: 1}, {File: "repo/b.go", Line: 1}, {File: "repo/c.go", Line: 1}}
	second := []astquery.Match{{File: "repo/d.go", Line: 1}, {File: "repo/e.go", Line: 1}}

	server.enrichASTMatchesContext(ctx, first)
	server.enrichASTMatchesContext(ctx, second)

	if len(probe.calls) != localizationFileRequestLimit/localizationFileNodeLimit {
		t.Fatalf("bounded calls = %d, want shared request cap of %d", len(probe.calls), localizationFileRequestLimit/localizationFileNodeLimit)
	}
	if probe.calls[3].path != "repo/d.go" || second[0].SymbolID != "repo/d.go::owner" {
		t.Fatalf("last budgeted match = %#v / %#v, want repo/d.go owner", probe.calls[3], second[0])
	}
	if second[1].SymbolID != "" || second[1].SymbolName != "" {
		t.Fatalf("budget-exhausted match = %#v, want blank fail-closed symbol", second[1])
	}
}

func TestEnrichASTMatchesFailsClosedWithoutFullScan(t *testing.T) {
	t.Run("saturated", func(t *testing.T) {
		const path = "repo/dense.go"
		probe := &astTargetBoundedStore{Store: graph.New()}
		probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{
				Nodes: []*graph.Node{{
					ID: path + "::wrong", Name: "wrong", Kind: graph.KindFunction,
					FilePath: path, StartLine: 1, EndLine: 100,
				}},
				Total: limit, Truncated: true,
			}, nil
		}
		server := &Server{graph: probe}
		matches := []astquery.Match{{File: path, Line: 50, SymbolID: "stale", SymbolName: "stale"}}
		server.enrichASTMatchesContext(context.Background(), matches)
		if matches[0].SymbolID != "" || matches[0].SymbolName != "" {
			t.Fatalf("saturated match = %#v, want fail closed", matches[0])
		}
	})

	t.Run("missing capability", func(t *testing.T) {
		const path = "repo/unsupported.go"
		server := &Server{graph: &astTargetUnboundedStore{Store: graph.New()}}
		matches := []astquery.Match{{File: path, Line: 1, SymbolID: "stale", SymbolName: "stale"}}
		server.enrichASTMatchesContext(context.Background(), matches)
		if matches[0].SymbolID != "" || matches[0].SymbolName != "" {
			t.Fatalf("unsupported match = %#v, want fail closed", matches[0])
		}
	})
}

// TestEnrichASTMatchesReadsOverlayInsteadOfBase pins AST enclosing-symbol
// enrichment to the request reader: a match in a file the session has an
// editor buffer for is attributed to the buffer's owner, not to the durable
// declaration the buffer replaced.
func TestEnrichASTMatchesReadsOverlayInsteadOfBase(t *testing.T) {
	const path = "repo/service.go"
	baseOwner := &graph.Node{
		ID: path + "::base", Name: "base", Kind: graph.KindFunction,
		FilePath: path, StartLine: 1, EndLine: 20,
	}
	overlayOwner := &graph.Node{
		ID: path + "::overlay", Name: "overlay", Kind: graph.KindFunction,
		FilePath: path, StartLine: 1, EndLine: 20,
	}
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, gotPath string, _ graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		if gotPath != path || limit != localizationFileNodeLimit {
			return graph.BoundedNodeProjection{}, fmt.Errorf("unexpected base read %q limit %d", gotPath, limit)
		}
		return graph.BoundedNodeProjection{Nodes: []*graph.Node{baseOwner}, Total: 1}, nil
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile(path, false)
	layer.AddNode(path, overlayOwner)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(probe, layer))
	server := &Server{graph: probe}
	matches := []astquery.Match{{File: path, Line: 10}}

	server.enrichASTMatchesContext(ctx, matches)

	// The overlaid file is answered from the buffer, so the match is
	// attributed to the buffer's owner and base is never projected.
	if matches[0].SymbolID != overlayOwner.ID || matches[0].SymbolName != overlayOwner.Name {
		t.Fatalf("AST match owner = %#v, want overlay owner (%q, %q)", matches[0], overlayOwner.ID, overlayOwner.Name)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("base projection calls = %d, want none for an overlaid file", len(probe.calls))
	}
}

func TestEnrichASTMatchesCapsRefundedEmptyFileReads(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
		// Empty pages refund their entire node reservation. The independent
		// file-call cap must still stop projection after 64 transactions.
		return graph.BoundedNodeProjection{}, nil
	}
	server := &Server{graph: probe}
	matches := make([]astquery.Match, astPostMatchFileLimit+2)
	for index := range matches {
		matches[index] = astquery.Match{File: fmt.Sprintf("repo/%03d.go", len(matches)-index), Line: 1}
	}

	server.enrichASTMatchesContext(context.Background(), matches)

	if len(probe.calls) != astPostMatchFileLimit {
		t.Fatalf("bounded calls = %d, want hard file cap %d despite refunds", len(probe.calls), astPostMatchFileLimit)
	}
	for index := range probe.calls {
		if probe.calls[index].path != matches[index].File {
			t.Fatalf("priority %d call = %#v, want path %q", index, probe.calls[index], matches[index].File)
		}
	}
	for index := range matches {
		if matches[index].SymbolID != "" || matches[index].SymbolName != "" {
			t.Fatalf("empty-page match %d = %#v, want blank symbol", index, matches[index])
		}
	}
}

func TestPrepareReviewSASTMatchesAppliesPriorityBeforeLimit(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, path string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{
			Nodes: []*graph.Node{{
				ID: path + "::owner", Name: "owner", Kind: graph.KindFunction,
				FilePath: path, StartLine: 1, EndLine: 2,
			}}, Total: 1,
		}, nil
	}
	server := &Server{graph: probe}
	matches := make([]astquery.Match, 0, astPostMatchFileLimit+2)
	for index := 0; index <= astPostMatchFileLimit; index++ {
		matches = append(matches, astquery.Match{
			Detector: "z-low", Severity: "low",
			File: fmt.Sprintf("repo/%03d-low.go", index), Line: 1,
		})
	}
	const highPath = "repo/high-priority.go"
	matches = append(matches, astquery.Match{
		Detector: "a-high", Severity: "critical", File: highPath, Line: 1,
	})

	server.prepareReviewSASTMatchesContext(context.Background(), matches, 1, true)

	if matches[0].File != highPath {
		t.Fatalf("first stable match = %q, want high-priority %q", matches[0].File, highPath)
	}
	if len(probe.calls) != 1 || probe.calls[0].path != highPath {
		t.Fatalf("bounded calls = %#v, want one high-priority projection", probe.calls)
	}
	if matches[0].SymbolID != highPath+"::owner" {
		t.Fatalf("high-priority symbol = %q, want owner", matches[0].SymbolID)
	}
	for index := 1; index < len(matches); index++ {
		if matches[index].SymbolID != "" || matches[index].SymbolName != "" {
			t.Fatalf("over-limit match %d = %#v, want blank symbol", index, matches[index])
		}
	}
}

func TestPrepareReviewSASTMatchesKindsOnlySkipsProjection(t *testing.T) {
	probe := &astTargetBoundedStore{Store: graph.New()}
	probe.read = func(_ context.Context, _ string, _ graph.LocalizationNodeScope, _ int) (graph.BoundedNodeProjection, error) {
		t.Fatal("kinds_only must not read enclosing-symbol projections")
		return graph.BoundedNodeProjection{}, nil
	}
	server := &Server{graph: probe}
	matches := []astquery.Match{{
		Detector: "review", Severity: "high", File: "repo/review.go", Line: 1,
	}}

	server.prepareReviewSASTMatchesContext(context.Background(), matches, 1, false)

	if len(probe.calls) != 0 {
		t.Fatalf("bounded calls = %#v, want none for kinds_only", probe.calls)
	}
}
