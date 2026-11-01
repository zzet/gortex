package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

type explicitTierWatcher struct {
	watchedTierFixture
	enabled bool
}

func (w *explicitTierWatcher) TierAuthority(prefix string) bool { return w.enabled && prefix == "repo" }

func explicitFreshnessFixture(t *testing.T) (*Server, context.Context, func()) {
	t.Helper()
	stack := newViewStackWithRepos(t, false)
	require.NoError(t, stack.leases.RegisterRepositoryOwner(viewTestOwner()))
	authority := indexer.NewOutputGenerationAuthority(stack.leases)
	mutate := func() {
		t.Helper()
		receipt, err := authority.Begin(t.Context(), indexer.OutputEntryWatcherDirScan, indexer.OutputMutationTarget{
			Kind: indexer.OutputGenerationLegacy, OwnerKey: "root:" + stack.repoRoot, RepoPrefix: "repo", RootPath: stack.repoRoot,
		})
		require.NoError(t, err)
		stack.store.AddNode(viewRepoNode("repo/target.go::Target", "Target", graph.KindFunction, "repo/target.go", 1))
		require.NoError(t, receipt.Complete())
	}
	mutate()
	stack.srv.multiIndexer.CompleteStartupRelations()
	stack.srv.SetWatcher(&explicitTierWatcher{enabled: true})
	ctx := withRequestView(t.Context(), &requestView{reader: newBaseGraphReader(stack.store, "repo"), baseNarrowed: true})
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"require_fresh": true}
	ctx, release := stack.srv.captureExplicitFreshness(ctx, req)
	require.NotNil(t, release)
	t.Cleanup(release)
	return stack.srv, ctx, mutate
}

func TestRequireFreshReportsExplicitGraphTier(t *testing.T) {
	for _, scope := range []string{"file", "relations", "derived"} {
		for _, format := range []string{"json", "toon", "text", "gcx", "compact"} {
			t.Run(scope+"/"+format, func(t *testing.T) {
				s, ctx, _ := explicitFreshnessFixture(t)
				req := mcp.CallToolRequest{}
				req.Params.Name = "get_callers"
				req.Params.Arguments = map[string]any{"id": "repo/target.go::Target", "require_fresh": true, "freshness_scope": scope, "format": format}
				body := "callers[0]{id}:"
				if format == "json" {
					body = `{"callers":[],"complete":false}`
				} else if format == "gcx" {
					body = "GCX1 tool=get_callers\nrow1"
				} else if format == "compact" {
					req.Params.Arguments.(map[string]any)["compact"] = true
				}
				original := mcp.NewToolResultText(body)
				got := s.decorateToolFreshness(ctx, req.Params.Name, req, original)
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

func TestRequireFreshRejectsCompletedMutationDuringRead(t *testing.T) {
	s, ctx, mutate := explicitFreshnessFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": "repo/target.go::Target", "require_fresh": true, "format": "json"}
	before := s.decorateToolFreshness(ctx, req.Params.Name, req, mcp.NewToolResultText(`{"callers":[]}`))
	require.Equal(t, "fresh", resultJSON(t, before)["freshness"].(map[string]any)["graph_freshness"])
	mutate() // The accepted publication completes; no pending path survives.
	after := s.decorateToolFreshness(ctx, req.Params.Name, req, mcp.NewToolResultText(`{"callers":[]}`))
	require.Equal(t, "unknown", resultJSON(t, after)["freshness"].(map[string]any)["graph_freshness"])
}

func TestRequireFreshUsesActualAutoCanonicalReader(t *testing.T) {
	s, _, _ := explicitFreshnessFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": "repo/target.go::Target", "require_fresh": true, "format": "json"}
	got, err := s.wrapToolHandler(s.handleGetCallers)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, got.IsError, resultText(got))
	require.Equal(t, "fresh", resultJSON(t, got)["freshness"].(map[string]any)["graph_freshness"])
	require.NotContains(t, req.GetArguments(), "view", "AUTO must not be replaced by a BASE control")
}

func TestRequireFreshRejectsUntrackedCanonicalWatcher(t *testing.T) {
	s, ctx, _ := explicitFreshnessFixture(t)
	s.SetWatcher(&explicitTierWatcher{enabled: false})
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": "repo/target.go::Target", "require_fresh": true, "format": "json"}
	got := s.decorateToolFreshness(ctx, req.Params.Name, req, mcp.NewToolResultText(`{"callers":[]}`))
	require.Equal(t, "unknown", resultJSON(t, got)["freshness"].(map[string]any)["graph_freshness"])
}

func TestRequireFreshAutoOwnerAddedDuringReadIsUnknown(t *testing.T) {
	s, _, _ := explicitFreshnessFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": "repo/target.go::Target", "require_fresh": true, "format": "json"}
	ctx, release := s.captureExplicitFreshness(t.Context(), req)
	require.NotNil(t, release)
	defer release()
	before := s.decorateToolFreshness(ctx, req.Params.Name, req, mcp.NewToolResultText(`{"callers":[]}`))
	require.Equal(t, "fresh", resultJSON(t, before)["freshness"].(map[string]any)["graph_freshness"])
	_, err := s.multiIndexer.TrackRepoCtx(t.Context(), config.RepoEntry{Name: "added", Path: setupMiniRepo(t, "added")})
	require.NoError(t, err)
	after := s.decorateToolFreshness(ctx, req.Params.Name, req, mcp.NewToolResultText(`{"callers":[]}`))
	require.Equal(t, "unknown", resultJSON(t, after)["freshness"].(map[string]any)["graph_freshness"])
}

func TestRequireFreshRelationPayloadDoesNotUseFileTierAlone(t *testing.T) {
	for _, tool := range []string{"get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			s, ctx, _ := explicitFreshnessFixture(t)
			watcher := s.currentWatcher().(*explicitTierWatcher)
			watcher.paths = map[string]indexer.MutationTierState{
				s.multiIndexer.ResolveFilePath("repo/target.go"): {Generation: 1, AckAt: time.Now(), FileGraphAt: time.Now()},
			}
			req := mcp.CallToolRequest{}
			req.Params.Name = tool
			req.Params.Arguments = map[string]any{"path": "repo/target.go", "require_fresh": true, "format": "json"}
			got := s.decorateToolFreshness(ctx, tool, req, mcp.NewToolResultText(`{"callers":["old"]}`))
			fields := resultJSON(t, got)["freshness"].(map[string]any)
			require.Equal(t, "pending", fields["graph_freshness"])
			require.Equal(t, "relations", fields["freshness_scope"])
		})
	}
}

func TestRequireFreshFacadeForwardsExplicitTierRequest(t *testing.T) {
	for _, format := range []string{"json", "toon"} {
		t.Run(format, func(t *testing.T) {
			s, ctx, _ := explicitFreshnessFixture(t)
			req := mcp.CallToolRequest{}
			req.Params.Name = "relations"
			req.Params.Arguments = map[string]any{
				"operation": "callers", "target": map[string]any{"symbol": "repo/target.go::Target"},
				"options": map[string]any{"require_fresh": true}, "output": map[string]any{"format": format},
			}
			got, err := s.handleFacade(ctx, "relations", req)
			require.NoError(t, err)
			require.False(t, got.IsError, resultText(got))
			if format == "json" {
				require.Equal(t, "fresh", resultJSON(t, got)["freshness"].(map[string]any)["graph_freshness"])
			} else {
				require.True(t, strings.HasPrefix(resultText(got), "relations: fresh (0 files)\n"), resultText(got))
			}
		})
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
