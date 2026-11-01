package mcp

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestRequireFreshReportsExplicitGraphTier(t *testing.T) {
	for _, scope := range []string{"file", "relations", "derived"} {
		for _, format := range []string{"json", "toon", "text", "gcx", "compact"} {
			t.Run(scope+"/"+format, func(t *testing.T) {
				s, _ := newRelationFixture(t)
				s.SetWatcher(&watchedTierFixture{})
				req := mcp.CallToolRequest{}
				req.Params.Name = "get_callers"
				req.Params.Arguments = map[string]any{"id": targetID(t, s), "require_fresh": true, "freshness_scope": scope, "format": format}
				body := "callers[0]{id}:"
				if format == "json" {
					body = `{"callers":[],"complete":false}`
				} else if format == "gcx" {
					body = "GCX1 tool=get_callers\nrow1"
				} else if format == "compact" {
					req.Params.Arguments.(map[string]any)["compact"] = true
				}
				original := mcp.NewToolResultText(body)
				got := s.decorateToolFreshness(t.Context(), req.Params.Name, req, original)
				if format == "compact" {
					require.Same(t, original, got, "compact has no new explicit-fresh contract")
					return
				}
				if format == "json" {
					payload := resultJSON(t, got)
					fields, ok := payload["freshness"].(map[string]any)
					require.True(t, ok, resultText(got))
					require.Equal(t, "fresh", fields["graph_freshness"])
					require.Equal(t, scope, fields["freshness_scope"])
					require.EqualValues(t, 0, fields["pending_file_count"])
					require.Equal(t, false, payload["complete"], "freshness must preserve an answer's completeness")
				} else if format == "gcx" {
					require.Contains(t, resultText(got), "graph_freshness=fresh")
					require.Contains(t, resultText(got), "# "+scope+": fresh (0 files)\n")
				} else {
					require.True(t, strings.HasPrefix(resultText(got), scope+": fresh (0 files)\n"), resultText(got))
				}
			})
		}
	}
}

func TestRequireFreshDoesNotInventGraphAuthority(t *testing.T) {
	for _, route := range []string{"unwatched", "checkout", "source", "error"} {
		t.Run(route, func(t *testing.T) {
			s, _ := newRelationFixture(t)
			ctx := t.Context()
			if route != "unwatched" {
				s.SetWatcher(&watchedTierFixture{})
			}
			if route == "checkout" || route == "source" {
				view := &requestView{reader: s.graph}
				if route == "source" {
					view.sourceScope = "file"
				}
				ctx = withRequestView(ctx, view)
			}
			req := mcp.CallToolRequest{}
			req.Params.Name = "get_callers"
			req.Params.Arguments = map[string]any{"id": targetID(t, s), "require_fresh": true, "format": "json"}
			original := mcp.NewToolResultText(`{"callers":[]}`)
			if route == "error" {
				original.IsError = true
			}
			require.Same(t, original, s.decorateToolFreshness(ctx, req.Params.Name, req, original))
		})
	}
}

func TestRequireFreshKeepsPreReadPublicationGap(t *testing.T) {
	s, root := newRelationFixture(t)
	watcher := &watchedTierFixture{paths: map[string]indexer.MutationTierState{
		filepath.Join(root, "target.go"): {Generation: 1, AckAt: time.Now()},
	}}
	s.SetWatcher(watcher)
	ctx := s.captureToolFreshness(t.Context())
	watcher.mu.Lock()
	watcher.paths = nil
	watcher.mu.Unlock()
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": targetID(t, s), "require_fresh": true, "format": "json"}
	got := s.decorateToolFreshness(ctx, req.Params.Name, req, mcp.NewToolResultText(`{"callers":[]}`))
	fields := resultJSON(t, got)["freshness"].(map[string]any)
	require.Equal(t, "pending", fields["graph_freshness"], "a completed gap cannot certify rows read before publication")
}
