package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type hotspotScopeReader struct {
	graph.Reader
	lookedUp []string
}

func (r *hotspotScopeReader) GetNode(id string) *graph.Node {
	r.lookedUp = append(r.lookedUp, id)
	return r.Reader.GetNode(id)
}

// Real indexing supplies the repo registry and verifies the source-ID invariant
// for both callable kinds. Extra ranked rows exercise exact totals past the cap.
func hotspotScopeFixture(t *testing.T, sqlite bool) (*Server, []analysis.HotspotEntry) {
	t.Helper()
	body := "package main\ntype T struct{}\nfunc F() {}\nfunc (T) M() {}\n"
	s, _ := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "gortex", workspace: "shared", body: body},
		analyzeRepoSpec{name: "gortex2", workspace: "shared", body: body},
		analyzeRepoSpec{name: "gortex@feature", workspace: "feature", body: body},
	)
	kinds := map[graph.NodeKind]bool{}
	for _, n := range s.graph.AllNodes() {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			require.Equal(t, n.RepoPrefix, graph.RepoPrefixOfID(n.ID), n.ID)
			kinds[n.Kind] = true
		}
	}
	require.True(t, kinds[graph.KindFunction])
	require.True(t, kinds[graph.KindMethod])
	if sqlite {
		store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "hotspots.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		store.AddBatch(s.graph.AllNodes(), s.graph.AllEdges())
		s.graph = store
	}
	var entries []analysis.HotspotEntry
	for i := range 23 {
		for _, repo := range []string{"gortex2", "gortex@feature", "gortex"} {
			ws := "shared"
			if repo == "gortex@feature" {
				ws = "feature"
			}
			kind := graph.KindFunction
			if i%2 != 0 {
				kind = graph.KindMethod
			}
			n := &graph.Node{ID: fmt.Sprintf("%s/pkg/hot.go::F%02d", repo, i), Name: fmt.Sprintf("F%02d", i), Kind: kind,
				FilePath: repo + "/pkg/hot.go", RepoPrefix: repo, WorkspaceID: ws}
			s.graph.AddNode(n)
			entries = append(entries, analysis.HotspotEntry{ID: n.ID, Name: n.Name, Kind: string(kind), FilePath: n.FilePath, ComplexityScore: float64(100 - len(entries))})
		}
	}
	// Stub and unknown-prefix rows cannot safely use a slash-derived repo key.
	for _, n := range []*graph.Node{
		{ID: "gortex@feature::stdlib::net/http::Get", Kind: graph.KindFunction, RepoPrefix: "gortex@feature", WorkspaceID: "feature", FilePath: "external::net/http"},
		{ID: "unknown/pkg/legacy.go::F", Kind: graph.KindFunction, RepoPrefix: "gortex", WorkspaceID: "shared", FilePath: "unknown/pkg/legacy.go"},
	} {
		s.graph.AddNode(n)
		entries = append(entries, analysis.HotspotEntry{ID: n.ID, Kind: string(n.Kind), FilePath: n.FilePath})
	}
	for repo, callers := range map[string]int{"gortex": 22, "gortex2": 20, "gortex@feature": 18} {
		for i := 1; i <= callers; i++ {
			s.graph.AddEdge(&graph.Edge{From: fmt.Sprintf("%s/pkg/hot.go::F%02d", repo, i), To: repo + "/pkg/hot.go::F00", Kind: graph.EdgeCalls})
		}
	}
	installCommunitiesForTest(s, &analysis.CommunityResult{})
	s.hotspots = entries
	s.hotspotsReady = true
	return s, entries
}

func TestHotspotsScopeSkipsInvisibleRepoReads(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s, cached := hotspotScopeFixture(t, backend == "sqlite")
			for _, tc := range []struct {
				name, workspace, repo, mode string
				bound                       bool
				threshold                   float64
				reads, total                int
			}{
				{name: "repo", repo: "gortex", reads: 25, total: 24},
				{name: "workspace", workspace: "shared", bound: true, reads: 48, total: 47},
				{name: "workspace_repo", workspace: "shared", repo: "gortex2", bound: true, reads: 25, total: 23},
				{name: "worktree", repo: "gortex@feature", reads: 25, total: 24},
				{name: "unbound", reads: 0, total: 71},
				{name: "deny_all", workspace: "missing", bound: true, reads: 2, total: 0},
				{name: "novelty", repo: "gortex", mode: "novelty", reads: 25, total: 24},
				{name: "threshold", repo: "gortex", threshold: 50, reads: 1, total: 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := WithSessionID(context.Background(), backend+"-"+tc.name)
					if tc.bound {
						ss := s.sessionFor(ctx)
						ss.scopeResolved, ss.scopeBound, ss.scopeWorkspaceID = true, true, tc.workspace
					}
					if tc.repo != "" {
						ctx = withRepoAllow(ctx, map[string]bool{tc.repo: true})
					}
					args := map[string]any{"kind": "hotspots", "format": "json", "max_bytes": float64(0), "mode": tc.mode, "threshold": tc.threshold}
					req := makeReq("analyze", args)
					entries := cached
					if tc.threshold != 0 {
						entries = analysis.FindHotspots(s.graph, s.getCommunities(), tc.threshold)
						require.NotEmpty(t, entries)
					}
					if tc.mode != "" {
						entries = rerankHotspots(entries, s.graph, tc.mode, "adds", 30)
					}
					// The base implementation hydrates every entry before filtering.
					kept := entries
					if s.scopeFiltersActive(ctx) {
						kept = make([]analysis.HotspotEntry, 0, len(entries))
						for _, e := range entries {
							if s.analyzeNodeVisible(ctx, s.graph.GetNode(e.ID)) {
								kept = append(kept, e)
							}
						}
					}
					total := len(kept)
					require.Equal(t, tc.total, total)
					if total > 20 {
						kept = kept[:20]
					}
					want, err := s.respondJSONOrTOON(ctx, req, map[string]any{"hotspots": kept, "total": total, "truncated": total > 20})
					require.NoError(t, err)
					reader := &hotspotScopeReader{Reader: s.graph}
					callCtx := withRequestView(ctx, &requestView{reader: reader})
					got, err := s.handleFindHotspots(callCtx, req)
					require.NoError(t, err)
					require.False(t, got.IsError, toolResultText(got))
					require.Equal(t, toolResultText(want), toolResultText(got), "complete response must match the base filter")
					require.Len(t, reader.lookedUp, tc.reads)
					for _, id := range reader.lookedUp {
						if id == "gortex@feature::stdlib::net/http::Get" || id == "unknown/pkg/legacy.go::F" {
							continue
						}
						require.True(t, s.analyzeNodeVisible(ctx, s.graph.GetNode(id)), "invisible source entry was hydrated: %s", id)
					}
					if tc.threshold == 0 && tc.name != "unbound" {
						require.Contains(t, reader.lookedUp, "gortex@feature::stdlib::net/http::Get")
						require.Contains(t, reader.lookedUp, "unknown/pkg/legacy.go::F")
					}
				})
			}
		})
	}
}

func TestHotspotsScopeSingleRepoKeepsUnprefixedReads(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s, _ := newAnalyzeServer(t, true, analyzeRepoSpec{name: "solo", workspace: "solo", body: structuralBody("solo")})
			require.False(t, s.multiIndexer.IsMultiRepo())
			if backend == "sqlite" {
				store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "solo.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				store.AddBatch(s.graph.AllNodes(), s.graph.AllEdges())
				s.graph = store
			}
			n := &graph.Node{ID: "internal/pkg.go::F", Kind: graph.KindFunction, WorkspaceID: "solo"}
			s.graph.AddNode(n)
			require.NotEqual(t, n.RepoPrefix, graph.RepoPrefixOfID(n.ID), "single-repo IDs do not encode repo ownership")
			installCommunitiesForTest(s, &analysis.CommunityResult{})
			s.hotspots = []analysis.HotspotEntry{{ID: n.ID}}
			s.hotspotsReady = true
			ctx := outlineBoundContext(s, "solo", "solo", nil)
			req := makeReq("analyze", map[string]any{"format": "json"})
			want, err := s.respondJSONOrTOON(ctx, req, map[string]any{"hotspots": s.hotspots, "total": 1, "truncated": false})
			require.NoError(t, err)
			reader := &hotspotScopeReader{Reader: s.graph}
			ctx = withRequestView(ctx, &requestView{reader: reader})
			result, err := s.handleFindHotspots(ctx, req)
			require.NoError(t, err)
			require.False(t, result.IsError)
			require.Equal(t, toolResultText(want), toolResultText(result))
			require.Equal(t, []string{n.ID}, reader.lookedUp)
		})
	}
}
