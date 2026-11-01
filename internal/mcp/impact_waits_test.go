package mcp

import (
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reach"
)

func TestImpactCoordinationMetadataPreservesPayloadAndMissingStates(t *testing.T) {
	waits := new(impactCoordinationWaits)
	waits.observe(reach.CoordinationWait{Phase: "gate_wait", Duration: 12 * time.Millisecond, Attempted: true, Acquired: true})
	waits.observe(reach.CoordinationWait{Phase: "resolve_mutex_wait", Acquired: true})
	original := `{"complete":true,"by_depth":{}}`
	result := attachImpactCoordinationWaits(mcp.NewToolResultText(original), waits)
	if got := result.Content[0].(mcp.TextContent).Text; got != original {
		t.Fatalf("wait metadata changed semantic payload: %s", got)
	}
	meta := result.Meta.AdditionalFields["reach_coordination"].(map[string]any)
	gate := meta["gate_wait"].(map[string]any)
	resolve := meta["resolve_mutex_wait"].(map[string]any)
	if gate["state"] != "observed" || gate["total_ms"] != float64(12) {
		t.Fatalf("gate wait lost: %v", gate)
	}
	if resolve["state"] != "not_applicable_detached" || resolve["total_ms"] != nil {
		t.Fatalf("detached mutex became a measured zero: %v", resolve)
	}
	missing := impactWaitFields(impactWaitPhase{})
	if missing["state"] != "not_observed" || missing["total_ms"] != nil {
		t.Fatalf("unobserved wait became zero: %v", missing)
	}
	interrupted := impactWaitFields(impactWaitPhase{observed: 1, attempted: 1, total: time.Second})
	if interrupted["state"] != "censored" {
		t.Fatalf("interrupted wait claimed complete: %v", interrupted)
	}
}

func TestImpactCoordinationHandlerForwardsRealLookupWaits(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go", RepoPrefix: "repo"})
	s := &Server{graph: g, engine: query.NewEngine(g), session: newSessionState()}
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "explain_change_impact", Arguments: map[string]any{"ids": "repo/a.go::A", "format": "json"}}}
	result, err := s.handleEnhancedChangeImpact(t.Context(), req)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("impact failed: %v %+v", err, result)
	}
	if result.Meta == nil {
		t.Fatal("handler dropped actual wait observations")
	}
	fields, _ := result.Meta.AdditionalFields["reach_coordination"].(map[string]any)
	for _, name := range []string{"gate_wait", "resolve_mutex_wait"} {
		phase, _ := fields[name].(map[string]any)
		if phase["state"] != "observed" || phase["attempted"] != 1 || phase["acquired"] != 1 {
			t.Fatalf("%s did not reflect actual lookup: %v", name, phase)
		}
	}
}
