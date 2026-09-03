package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func scopeGateFixture(cwd string, bound bool) (*Server, context.Context) {
	ctx := context.Background()
	if cwd != "" {
		ctx = WithSessionCWD(ctx, cwd)
	}
	return &Server{
		multiIndexer: &indexer.MultiIndexer{},
		session: &sessionState{
			scopeResolved:    true,
			scopeCWD:         cwd,
			scopeBound:       bound,
			scopeWorkspaceID: unresolvedWorkspacePrefix + cwd,
		},
	}, ctx
}

func TestRepoPrefixInSessionScopeBoundary(t *testing.T) {
	t.Run("unbound remains unrestricted", func(t *testing.T) {
		srv, ctx := scopeGateFixture("", false)
		repos, bound := srv.sessionWorkspaceRepoSet(ctx)
		if bound || len(repos) != 0 {
			t.Fatalf("fixture = repos:%v bound:%v", repos, bound)
		}
		if err := srv.repoPrefixInSessionScope(ctx, "not-registered", "graph-unbound"); err != nil {
			t.Fatalf("unbound selector was clamped: %v", err)
		}
	})

	t.Run("bound empty rejects explicit selector", func(t *testing.T) {
		const cwd = "/unresolved-cwd"
		srv, ctx := scopeGateFixture(cwd, true)
		repos, bound := srv.sessionWorkspaceRepoSet(ctx)
		if !bound || len(repos) != 0 {
			t.Fatalf("fixture = repos:%v bound:%v", repos, bound)
		}
		err := srv.repoPrefixInSessionScope(ctx, "repo", "graph-repo")
		if !errors.Is(err, graphview.ErrSelectorOutOfScope) {
			t.Fatalf("error = %v, want %s", err, graphview.CodeSelectorOutOfScope)
		}
	})
}

func TestBoundEmptySessionClampsWholeGraphSurfaces(t *testing.T) {
	const cwd = "/unresolved-cwd"
	srv, ctx := scopeGateFixture(cwd, true)

	communities := srv.communitiesInSessionScope(ctx, &analysis.CommunityResult{
		Communities: []analysis.Community{{
			ID: "community", Members: []string{"repo/file.go::Fn"}, Files: []string{"repo/file.go"},
		}},
	})
	if len(communities.Communities) != 0 {
		t.Fatalf("bound-empty communities leaked rows: %#v", communities.Communities)
	}
	if len(communities.NodeToComm) != 0 || communities.Modularity != 0 {
		t.Fatalf("bound-empty community metadata leaked: map=%v modularity=%v",
			communities.NodeToComm, communities.Modularity)
	}

	processes := srv.processesInSessionScope(ctx, &analysis.ProcessResult{
		Processes: []analysis.Process{{
			ID: "process", EntryPoint: "repo/file.go::Fn",
			Steps: []analysis.Step{{ID: "repo/file.go::Fn"}},
		}},
	})
	if len(processes.Processes) != 0 {
		t.Fatalf("bound-empty processes leaked rows: %#v", processes.Processes)
	}
	if srv.repoPrefixVisible(ctx, "repo") || srv.repoPrefixVisible(ctx, "") {
		t.Fatal("bound-empty prefix visibility admitted a row")
	}

	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/file.go::Fn", RepoPrefix: "repo", FilePath: "repo/file.go"})
	g.AddEdge(&graph.Edge{
		From: "repo/file.go::Fn", To: "repo/file.go::Other", Kind: graph.EdgeCalls,
		Meta: map[string]any{"synthesized_by": "test-synth", "provenance": "test"},
	})
	srv.graph = g
	if _, err := srv.refViewGraphID(ctx, graphview.Selector{}); !errors.Is(err, graphview.ErrInvalidViewSelector) {
		t.Fatalf("bound-empty ref inference error = %v, want %s", err, graphview.CodeInvalidViewSelector)
	}

	res, err := srv.handleAnalyzeSynthesizers(ctx, mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if text := toolResultText(res); containsAny(text, "test-synth", "repo/file.go::Fn") {
		t.Fatalf("bound-empty synthesizer analysis leaked graph rows: %s", text)
	}
}

func BenchmarkRepoPrefixInSessionScope(b *testing.B) {
	for _, tc := range []struct {
		name  string
		cwd   string
		bound bool
	}{
		{name: "unbound", bound: false},
		{name: "bound_empty", cwd: "/unresolved-cwd", bound: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			srv, ctx := scopeGateFixture(tc.cwd, tc.bound)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = srv.repoPrefixInSessionScope(ctx, "repo", "graph-repo")
			}
		})
	}
}
