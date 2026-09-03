package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// The change-gate engines (verify_change, check_guards, change_contract's
// rule families), the wakeup digest, and the graph_completion retriever all
// take a graph.Reader now, and every handler hands them s.readerFor(ctx).
// These tests drive the real handlers with an installed overlay so a revert
// to the base store fails on the answer, not only on the type.

// gateServer wires a fully constructed server over a base graph — the gate
// handlers walk callers through the query engine, so the engine has to be
// real.
func gateServer(t *testing.T, g *graph.Graph) *Server {
	t.Helper()
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}

// ─── verify_change (analysis.VerifyChanges) ─────────────────────────────────

const (
	vcFile     = "p/api.go"
	vcID       = vcFile + "::Handle"
	vcCallerID = "p/call.go::Caller"
)

// verifyChangeFixture indexes Handle with a one-parameter signature and one
// caller. The buffer re-emits Handle under the same id with the two-parameter
// signature the caller is about to verify.
func verifyChangeFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: vcID, Name: "Handle", Kind: graph.KindFunction, FilePath: vcFile, Language: "go",
		Meta: map[string]any{"signature": "func(a int)"},
	})
	g.AddNode(&graph.Node{ID: vcCallerID, Name: "Caller", Kind: graph.KindFunction, FilePath: "p/call.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: vcCallerID, To: vcID, Kind: graph.EdgeCalls, FilePath: "p/call.go", Line: 4})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(vcFile, false)
	layer.AddNode(vcFile, &graph.Node{
		ID: vcID, Name: "Handle", Kind: graph.KindFunction, FilePath: vcFile, Language: "go",
		Meta: map[string]any{"signature": "func(a int, b int)"},
	})
	return gateServer(t, g), layer
}

func verifyChangeViolations(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	res, err := s.handleVerifyChange(ctx, makeReq("verify_change", map[string]any{
		"changes": `[{"symbol_id":"` + vcID + `","new_signature":"func(a int, b int)"}]`,
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "verify_change errored: %s", toolResultText(res))

	var out struct {
		Violations []struct {
			SymbolID string `json:"symbol_id"`
		} `json:"violations"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	ids := make([]string, 0, len(out.Violations))
	for _, v := range out.Violations {
		ids = append(ids, v.SymbolID)
	}
	return ids
}

// TestVerifyChangeReflectsOverlay pins verify_change's old-signature read to
// the request reader: the buffer already carries the two-parameter form, so
// the proposed change breaks nobody — while the index still shows the
// one-parameter form and every caller as broken.
func TestVerifyChangeReflectsOverlay(t *testing.T) {
	srv, layer := verifyChangeFixture(t)

	onBase := verifyChangeViolations(t, srv, context.Background())
	assert.Contains(t, onBase, vcCallerID,
		"the indexed one-parameter signature must report the caller as broken")

	onView := verifyChangeViolations(t, srv, overlayCtx(t, srv, layer))
	assert.Empty(t, onView,
		"the buffer's signature already matches the proposed one — nothing breaks")

	assert.Equal(t, "func(a int)", srv.graph.GetNode(vcID).Meta["signature"],
		"the overlay request must not mutate the base store")
}

// ─── check_guards (analysis.EvaluateGuards / EvaluateArchitecture) ──────────

const (
	cgAPIFile = "internal/api/h.go"
	cgAPIID   = cgAPIFile + "::Handle"
	cgDBID    = "internal/db/q.go::Query"
)

// guardsOverlayFixture indexes an api → db call the buffer has since
// removed: the file is re-parsed with the symbol still present but the call
// gone.
func guardsOverlayFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: cgAPIID, Name: "Handle", Kind: graph.KindFunction, FilePath: cgAPIFile, Language: "go"})
	g.AddNode(&graph.Node{ID: cgDBID, Name: "Query", Kind: graph.KindFunction, FilePath: "internal/db/q.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: cgAPIID, To: cgDBID, Kind: graph.EdgeCalls, FilePath: cgAPIFile, Line: 7})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(cgAPIFile, false)
	layer.AddNode(cgAPIFile, &graph.Node{
		ID: cgAPIID, Name: "Handle", Kind: graph.KindFunction, FilePath: cgAPIFile, Language: "go",
	})
	return gateServer(t, g), layer
}

func checkGuardsViolationKinds(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	res, err := s.handleCheckGuards(ctx, makeReq("check_guards", map[string]any{"ids": cgAPIID}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "check_guards errored: %s", toolResultText(res))

	var out struct {
		Violations []struct {
			Kind string `json:"kind"`
		} `json:"violations"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	kinds := make([]string, 0, len(out.Violations))
	for _, v := range out.Violations {
		kinds = append(kinds, v.Kind)
	}
	return kinds
}

// TestCheckGuardsBoundaryRuleReflectsOverlay pins EvaluateGuards' out-edge
// walk to the request reader: the boundary rule fires on the indexed call and
// stays quiet once the buffer has dropped it.
func TestCheckGuardsBoundaryRuleReflectsOverlay(t *testing.T) {
	srv, layer := guardsOverlayFixture(t)
	srv.guardRules = []config.GuardRule{{
		Name:   "api-must-not-call-db",
		Kind:   "boundary",
		Source: "internal/api",
		Target: "internal/db",
	}}

	assert.Equal(t, []string{"boundary"}, checkGuardsViolationKinds(t, srv, context.Background()),
		"the indexed call must trip the boundary rule")
	assert.Empty(t, checkGuardsViolationKinds(t, srv, overlayCtx(t, srv, layer)),
		"the buffer removed the call, so the rule must not fire")

	assert.Len(t, srv.graph.GetOutEdges(cgAPIID), 1,
		"the overlay request must not mutate the base store")
}

// TestCheckGuardsArchitectureReflectsOverlay pins EvaluateArchitecture's
// layer walk to the request reader: the deny rule fires on the indexed
// cross-layer edge and stays quiet once the buffer has dropped it.
func TestCheckGuardsArchitectureReflectsOverlay(t *testing.T) {
	srv, layer := guardsOverlayFixture(t)
	srv.SetArchitecture(config.ArchitectureConfig{
		Layers: map[string]config.LayerRule{
			"api": {Paths: []string{"internal/api/**"}, Deny: []string{"*"}},
			"db":  {Paths: []string{"internal/db/**"}},
		},
	})

	assert.Equal(t, []string{"layer"}, checkGuardsViolationKinds(t, srv, context.Background()),
		"the indexed cross-layer call must trip the deny rule")
	assert.Empty(t, checkGuardsViolationKinds(t, srv, overlayCtx(t, srv, layer)),
		"the buffer removed the cross-layer call, so the rule must not fire")
}

// ─── change_contract (analysis.RuleFamily.Evaluate) ─────────────────────────

func changeContractArchitectureFamilies(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	res, err := s.handleChangeContract(ctx, makeReq("change_contract", map[string]any{
		"source":  "symbols",
		"symbols": cgAPIID,
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "change_contract errored: %s", toolResultText(res))

	var env changeEnvelope
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &env))
	var out []string
	for _, r := range env.Reasons {
		if r.Family == "architecture" {
			out = append(out, r.Message)
		}
	}
	return out
}

// TestChangeContractRuleFamiliesReflectOverlay pins the rule-family loop to
// the request reader: every registered family is handed the caller's view, so
// the architecture family gates on the buffer's edges rather than the index's.
func TestChangeContractRuleFamiliesReflectOverlay(t *testing.T) {
	srv, layer := guardsOverlayFixture(t)
	srv.SetArchitecture(config.ArchitectureConfig{
		Layers: map[string]config.LayerRule{
			"api": {Paths: []string{"internal/api/**"}, Deny: []string{"*"}},
			"db":  {Paths: []string{"internal/db/**"}},
		},
	})

	assert.NotEmpty(t, changeContractArchitectureFamilies(t, srv, context.Background()),
		"the indexed cross-layer call must reach the change-contract verdict")
	assert.Empty(t, changeContractArchitectureFamilies(t, srv, overlayCtx(t, srv, layer)),
		"the buffer removed the cross-layer call, so no family may report it")
}

// ─── gortex_wakeup (BuildWakeup) ────────────────────────────────────────────

const (
	wkBufferFile = "w/buffer.go"
	wkSinkID     = "w/sink.go::Sink"
	wkDroppedID  = wkBufferFile + "::Dropped"
	wkFreshID    = wkBufferFile + "::Fresh"
)

// wakeupOverlayFixture indexes one entry point (no callers, one callee) in a
// file the buffer has since rewritten to declare a different entry point.
func wakeupOverlayFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: wkSinkID, Name: "Sink", Kind: graph.KindFunction, FilePath: "w/sink.go", Language: "go"})
	g.AddNode(&graph.Node{ID: wkDroppedID, Name: "Dropped", Kind: graph.KindFunction, FilePath: wkBufferFile, Language: "go"})
	g.AddEdge(&graph.Edge{From: wkDroppedID, To: wkSinkID, Kind: graph.EdgeCalls, FilePath: wkBufferFile, Line: 3})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(wkBufferFile, false)
	layer.MarkRemoved("Dropped", wkDroppedID)
	layer.AddNode(wkBufferFile, &graph.Node{
		ID: wkFreshID, Name: "Fresh", Kind: graph.KindFunction, FilePath: wkBufferFile, Language: "go",
	})
	layer.AddEdge(&graph.Edge{From: wkFreshID, To: wkSinkID, Kind: graph.EdgeCalls, FilePath: wkBufferFile, Line: 3})
	return gateServer(t, g), layer
}

func wakeupMarkdown(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	res, err := s.handleGortexWakeup(ctx, makeReq("gortex_wakeup", nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "gortex_wakeup errored: %s", toolResultText(res))
	return toolResultText(res)
}

// TestWakeupDigestReflectsOverlay pins the wakeup digest to the request
// reader: the entry-point roll-up names the symbol the buffer declares, not
// the one it deleted.
func TestWakeupDigestReflectsOverlay(t *testing.T) {
	srv, layer := wakeupOverlayFixture(t)

	onBase := wakeupMarkdown(t, srv, context.Background())
	assert.Contains(t, onBase, "Dropped", "the indexed entry point must be listed")
	assert.NotContains(t, onBase, "Fresh", "a buffer-only symbol must not be invented")

	onView := wakeupMarkdown(t, srv, overlayCtx(t, srv, layer))
	assert.NotContains(t, onView, "Dropped", "a symbol the buffer deleted must drop out")
	assert.Contains(t, onView, "Fresh", "the buffer's entry point must be listed")

	assert.NotNil(t, srv.graph.GetNode(wkDroppedID),
		"the overlay request must not mutate the base store")
}

// ─── graph_completion (rerank.GraphCompletion.Retrieve) ─────────────────────

const (
	gcHubFile = "p/hub.go"
	gcHubID   = gcHubFile + "::Hub"
	gcOldID   = "p/old.go::Old"
	gcNewID   = "p/new.go::New"
)

// graphCompletionOverlayFixture indexes Hub calling Old; the buffer re-emits
// Hub calling New instead. Both callees exist outside the buffered file, so
// only the edge relation moves.
func graphCompletionOverlayFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: gcHubID, Name: "Hub", Kind: graph.KindFunction, FilePath: gcHubFile, Language: "go"})
	g.AddNode(&graph.Node{ID: gcOldID, Name: "Old", Kind: graph.KindFunction, FilePath: "p/old.go", Language: "go"})
	g.AddNode(&graph.Node{ID: gcNewID, Name: "New", Kind: graph.KindFunction, FilePath: "p/new.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: gcHubID, To: gcOldID, Kind: graph.EdgeCalls, FilePath: gcHubFile, Line: 4})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(gcHubFile, false)
	layer.AddNode(gcHubFile, &graph.Node{ID: gcHubID, Name: "Hub", Kind: graph.KindFunction, FilePath: gcHubFile, Language: "go"})
	layer.AddEdge(&graph.Edge{From: gcHubID, To: gcNewID, Kind: graph.EdgeCalls, FilePath: gcHubFile, Line: 4})
	return gateServer(t, g), layer
}

func graphCompletionIDs(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "graph_completion"
	req.Params.Arguments = map[string]any{"query": "Hub"}
	res, err := s.handleGraphCompletionSearch(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "graph_completion errored: %s", toolResultText(res))

	var out struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	ids := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestGraphCompletionReflectsOverlay pins the retriever's seed + 1-hop
// expansion to the request reader: the expansion follows the buffer's call
// edge instead of the indexed one.
func TestGraphCompletionReflectsOverlay(t *testing.T) {
	srv, layer := graphCompletionOverlayFixture(t)

	onBase := graphCompletionIDs(t, srv, context.Background())
	assert.Contains(t, onBase, gcOldID, "the indexed callee must be expanded into")
	assert.NotContains(t, onBase, gcNewID, "the buffer's callee must not appear on a plain request")

	onView := graphCompletionIDs(t, srv, overlayCtx(t, srv, layer))
	assert.Contains(t, onView, gcNewID, "the buffer's callee must be expanded into")
	assert.NotContains(t, onView, gcOldID, "the edge the buffer replaced must drop out")

	edges := srv.graph.GetOutEdges(gcHubID)
	require.Len(t, edges, 1, "the overlay request must not mutate the base store")
	assert.Equal(t, gcOldID, edges[0].To)
}
