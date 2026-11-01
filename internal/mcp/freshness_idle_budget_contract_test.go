package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

type freshnessIdleReadCounter struct {
	graph.Reader
	nodes, inEdges int
}

func (r *freshnessIdleReadCounter) GetNode(id string) *graph.Node {
	r.nodes++
	return r.Reader.GetNode(id)
}

func (r *freshnessIdleReadCounter) GetInEdges(id string) []*graph.Edge {
	r.inEdges++
	return r.Reader.GetInEdges(id)
}

func TestFreshnessResponseContractIdleWatcherSkipsRelationReads(t *testing.T) {
	s, _ := newRelationFixture(t)
	id := targetID(t, s)
	s.SetWatcher(&watchedTierFixture{})
	counter := &freshnessIdleReadCounter{Reader: s.graph}
	// A narrowed base reader remains canonical: it must neither bypass the
	// watcher nor build the seed's relation scope when no path is pending.
	ctx := withRequestView(t.Context(), &requestView{reader: counter, baseNarrowed: true})
	require.Empty(t, s.freshnessFor(ctx, "relations", []string{id}, nil).state)
	require.Equal(t, [2]int{0, 0}, [2]int{counter.nodes, counter.inEdges},
		"an idle watcher must add no seed lookup or in-edge scan to a read")
}

func TestFreshnessResponseContractFinalPayloadHonorsByteBudget(t *testing.T) {
	for _, name := range []string{"callers_compact", "file_summary_json", "facade_callers_compact"} {
		t.Run(name, func(t *testing.T) {
			s, root := newRelationFixture(t)
			ctx := t.Context()
			if name == "facade_callers_compact" {
				const session = "freshness-budget-facade"
				s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
				ctx = WithSessionID(ctx, session)
			}
			seed := targetID(t, s)
			// Many real graph rows let the existing structured and text budget
			// policies remove rows to make room for the freshness label.
			for i := 0; i < 40; i++ {
				id := fmt.Sprintf("target.go::BudgetCaller%02d", i)
				s.graph.AddNode(&graph.Node{ID: id, Name: fmt.Sprintf("BudgetCaller%02d", i),
					Kind: graph.KindFunction, FilePath: "target.go", StartLine: i + 10, EndLine: i + 10, Language: "go"})
				s.graph.AddEdge(&graph.Edge{From: id, To: seed, Kind: graph.EdgeCalls,
					FilePath: "target.go", Line: i + 10, Origin: graph.OriginASTResolved, Confidence: 1})
			}
			call := func(maxBytes int) *mcp.CallToolResult {
				t.Helper()
				req := mcp.CallToolRequest{}
				var handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
				switch name {
				case "callers_compact":
					req.Params.Name = "get_callers"
					req.Params.Arguments = map[string]any{"id": seed, "compact": true, "max_bytes": maxBytes}
					handler = s.handleGetCallers
				case "file_summary_json":
					req.Params.Name = "get_file_summary"
					req.Params.Arguments = map[string]any{"path": "target.go", "format": "json", "max_bytes": maxBytes}
					handler = s.handleGetFileSummary
				case "facade_callers_compact":
					req.Params.Name = "relations"
					req.Params.Arguments = map[string]any{"operation": "callers", "target": map[string]any{"symbol": seed},
						"output": map[string]any{"compact": true, "max_bytes": maxBytes}}
					handler = func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return s.handleFacade(ctx, "relations", req)
					}
				}
				res, err := s.wrapToolHandler(handler)(ctx, req)
				require.NoError(t, err)
				require.False(t, res.IsError, resultText(res))
				return res
			}
			s.SetWatcher(&watchedTierFixture{})
			// Use the complete fresh answer's byte length as an explicit supported
			// cap. The same rows fit before decoration; a pending answer must now
			// keep its label and fit that cap by returning fewer rows if needed.
			cap := len(resultText(call(0)))
			require.Greater(t, cap, 600, "fixture must leave room for a useful labelled answer")
			require.LessOrEqual(t, len(resultText(call(cap))), cap, "fresh control must honor max_bytes")
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				filepath.Join(root, "target.go"): {Generation: 1, AckAt: time.Now()},
			}})
			res := call(cap)
			text := resultText(res)
			if name == "file_summary_json" {
				fields, ok := resultJSON(t, res)["freshness"].(map[string]any)
				require.True(t, ok, "budget must preserve structured freshness")
				require.Equal(t, "pending", fields["graph_freshness"])
				require.Equal(t, "file", fields["freshness_scope"])
			} else {
				require.True(t, strings.HasPrefix(text, "relations: pending (1 files)\n"),
					"budget must preserve the relations label: %s", text)
			}
			require.LessOrEqual(t, len(text), cap, "max_bytes bounds the final decorated response")
		})
	}
}
