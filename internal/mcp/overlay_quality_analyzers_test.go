package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The quality analyzers (untested symbols, clones, dead code, the
// coverage / release sidecars) all read the whole graph. These tests pin
// that they read it through the request's reader: an overlay-active call
// grades the caller's buffers, and the base store is left untouched.

const (
	qualityRepo    = "q"
	qualitySvcFl   = "q/svc.go"
	qualityTestFl  = "q/svc_test.go"
	qualityOtherFl = "q/other.go"
	qualityAlphaID = qualitySvcFl + "::alpha"
	qualityBetaID  = qualitySvcFl + "::beta"
	qualityGammaID = qualityOtherFl + "::gamma"
	qualityDeltaID = qualityOtherFl + "::delta"
	qualityTestID  = qualityTestFl + "::TestGamma"
)

// overlayQualityFixture wires a handler-capable server over a base graph
// plus the layer an editor session would push for two of its files:
// svc.go is re-parsed with alpha moved down the file and beta deleted,
// and svc_test.go is re-parsed with its call into gamma gone.
//
// Base wiring, chosen so each read path has one observable consequence:
//   - beta calls alpha, so alpha is live and has fan_in 1
//   - TestGamma calls gamma, so gamma counts as tested
//   - alpha ~ gamma and beta ~ delta are clone pairs
func overlayQualityFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	fn := func(id, name, file string, line int) {
		g.AddNode(&graph.Node{
			ID: id, Name: name, Kind: graph.KindFunction,
			FilePath: file, RepoPrefix: qualityRepo, Language: "go", StartLine: line,
		})
	}
	fn(qualityAlphaID, "alpha", qualitySvcFl, 10)
	fn(qualityBetaID, "beta", qualitySvcFl, 20)
	fn(qualityGammaID, "gamma", qualityOtherFl, 30)
	fn(qualityDeltaID, "delta", qualityOtherFl, 40)
	fn(qualityTestID, "TestGamma", qualityTestFl, 5)

	g.AddEdge(&graph.Edge{From: qualityBetaID, To: qualityAlphaID, Kind: graph.EdgeCalls, FilePath: qualitySvcFl, Line: 21})
	g.AddEdge(&graph.Edge{From: qualityTestID, To: qualityGammaID, Kind: graph.EdgeCalls, FilePath: qualityTestFl, Line: 6})
	sim := func(a, b string, score float64) {
		for _, pair := range [][2]string{{a, b}, {b, a}} {
			g.AddEdge(&graph.Edge{
				From: pair[0], To: pair[1], Kind: graph.EdgeSimilarTo,
				Confidence: score, Meta: map[string]any{"similarity": score},
			})
		}
	}
	sim(qualityAlphaID, qualityGammaID, 0.9)
	sim(qualityBetaID, qualityDeltaID, 0.9)

	layer := graph.NewOverlayLayer()
	layer.MarkFile(qualitySvcFl, false)
	layer.AddNode(qualitySvcFl, &graph.Node{
		ID: qualityAlphaID, Name: "alpha", Kind: graph.KindFunction,
		FilePath: qualitySvcFl, RepoPrefix: qualityRepo, Language: "go", StartLine: 100,
	})
	layer.MarkRemoved("beta", qualityBetaID)
	layer.MarkFile(qualityTestFl, false)
	layer.AddNode(qualityTestFl, &graph.Node{
		ID: qualityTestID, Name: "TestGamma", Kind: graph.KindFunction,
		FilePath: qualityTestFl, RepoPrefix: qualityRepo, Language: "go", StartLine: 5,
	})

	srv := &Server{
		graph:      g,
		session:    newSessionState(),
		sessions:   newSessionMap(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		toolScopes: newScopeRegistry(),
	}
	return srv, layer
}

// untestedRowsFor drives get_untested_symbols and indexes its rows by ID.
func untestedRowsFor(t *testing.T, s *Server, ctx context.Context) map[string]map[string]any {
	t.Helper()
	res, err := s.handleGetUntestedSymbols(ctx, makeReq("get_untested_symbols", nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "get_untested_symbols errored: %s", toolResultText(res))
	var out struct {
		Untested []map[string]any `json:"untested"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	rows := make(map[string]map[string]any, len(out.Untested))
	for _, row := range out.Untested {
		id, _ := row["id"].(string)
		rows[id] = row
	}
	return rows
}

// TestUntestedSymbolsReflectsOverlay covers the whole untested path:
// the candidate scan, the test-reachability BFS (reachableFromTests) and
// the fan-in tally (collectFanInByKind). Reverting any of them to the
// base store keeps grading the indexed state — gamma stays "tested"
// after the buffer dropped its only test call, alpha keeps the indexed
// line and its fan-in from a symbol the buffer deleted, and the deleted
// symbol itself keeps being reported as untested code to write tests for.
func TestUntestedSymbolsReflectsOverlay(t *testing.T) {
	srv, layer := overlayQualityFixture(t)

	onBase := untestedRowsFor(t, srv, context.Background())
	require.Contains(t, onBase, qualityAlphaID)
	assert.Equal(t, float64(10), onBase[qualityAlphaID]["line"], "a plain request reports the indexed line")
	assert.Equal(t, float64(1), onBase[qualityAlphaID]["fan_in"], "beta's indexed call counts")
	assert.Contains(t, onBase, qualityBetaID, "a plain request still sees the indexed symbol")
	assert.NotContains(t, onBase, qualityGammaID, "the indexed test call covers gamma")

	onView := untestedRowsFor(t, srv, overlayCtx(t, srv, layer))
	require.Contains(t, onView, qualityAlphaID)
	assert.Equal(t, float64(100), onView[qualityAlphaID]["line"], "the row must carry the buffer's payload")
	assert.Equal(t, float64(0), onView[qualityAlphaID]["fan_in"], "the deleted caller's call must not count")
	assert.NotContains(t, onView, qualityBetaID, "a symbol the buffer deleted must not be listed")
	assert.Contains(t, onView, qualityGammaID, "the buffer dropped gamma's only test call")

	assert.Len(t, srv.graph.AllNodes(), 5, "the overlay request must not mutate the base store")
}

// cloneClustersFor drives find_clones and returns its cluster list.
func cloneClustersFor(t *testing.T, s *Server, ctx context.Context) []map[string]any {
	t.Helper()
	res, err := s.handleFindClones(ctx, makeReq("find_clones", nil))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "find_clones errored: %s", toolResultText(res))
	var out struct {
		Clusters []map[string]any `json:"clusters"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	return out.Clusters
}

// cloneMemberIn finds one member row inside a decoded cluster list.
func cloneMemberIn(clusters []map[string]any, id string) (map[string]any, bool) {
	for _, c := range clusters {
		members, _ := c["members"].([]any)
		for _, raw := range members {
			m, _ := raw.(map[string]any)
			if m["id"] == id {
				return m, true
			}
		}
	}
	return nil, false
}

// TestFindClonesReflectsOverlay covers the clone surface: the similarity
// edge stream, the endpoint lookups and the dead-code flag. On the base
// store the pair built on a deleted symbol survives, the member row
// carries the indexed line, and alpha looks live because of a call site
// the buffer removed.
func TestFindClonesReflectsOverlay(t *testing.T) {
	srv, layer := overlayQualityFixture(t)

	onBase := cloneClustersFor(t, srv, context.Background())
	assert.Len(t, onBase, 2, "a plain request sees both indexed clone pairs")
	alpha, ok := cloneMemberIn(onBase, qualityAlphaID)
	require.True(t, ok, "alpha is in an indexed cluster")
	assert.Equal(t, float64(10), alpha["start_line"], "a plain request reports the indexed line")
	assert.Equal(t, false, alpha["is_dead"], "beta's indexed call keeps alpha live")
	_, ok = cloneMemberIn(onBase, qualityBetaID)
	assert.True(t, ok, "a plain request still clusters the indexed symbol")

	onView := cloneClustersFor(t, srv, overlayCtx(t, srv, layer))
	assert.Len(t, onView, 1, "the pair built on the deleted symbol must drop out")
	alpha, ok = cloneMemberIn(onView, qualityAlphaID)
	require.True(t, ok, "alpha's pair survives the buffer")
	assert.Equal(t, float64(100), alpha["start_line"], "the member must carry the buffer's payload")
	assert.Equal(t, true, alpha["is_dead"], "the buffer deleted alpha's only caller")
	_, ok = cloneMemberIn(onView, qualityBetaID)
	assert.False(t, ok, "a symbol the buffer deleted must not be clustered")

	assert.Len(t, srv.graph.AllEdges(), 6, "the overlay request must not mutate the base store")
}

const (
	rollupFile  = "svc/handler.go"
	rollupSymID = rollupFile + "::Handle"
)

// rollupKeys drives analyze health_score with a repo rollup and returns
// the rollup keys it reported.
func rollupKeys(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	res, err := s.handleAnalyzeHealthScore(ctx, makeReq("analyze", map[string]any{"roll_up": "repo"}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "analyze health_score errored: %s", toolResultText(res))
	var payload struct {
		Rollup []struct {
			Key     string `json:"key"`
			Symbols int    `json:"symbols"`
		} `json:"rollup"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &payload))
	out := make([]string, 0, len(payload.Rollup))
	for _, row := range payload.Rollup {
		require.Equal(t, 1, row.Symbols, "the fixture grades exactly one symbol per key")
		out = append(out, row.Key)
	}
	return out
}

// TestHealthScoreRepoRollupReadsRequestReader pins the repo rollup key to
// the request's reader. The key is the repo prefix stamped on the file
// node that owns a row's path, and the request's reader — not the
// server's store — is the authority on which repo owns that path: a
// session view keeps its own base, where the file has already been
// re-homed. Reading the key off the server's store keys the aggregate off
// an attribution the caller never asked about.
func TestHealthScoreRepoRollupReadsRequestReader(t *testing.T) {
	store := func(filePrefix string) *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{
			ID: rollupFile, Name: "handler.go", Kind: graph.KindFile,
			FilePath: rollupFile, RepoPrefix: filePrefix,
		})
		g.AddNode(&graph.Node{
			ID: rollupSymID, Name: "Handle", Kind: graph.KindFunction,
			FilePath: rollupFile, RepoPrefix: "svc", StartLine: 10,
		})
		return g
	}
	base := store("svc")
	srv := &Server{
		graph:      base,
		session:    newSessionState(),
		sessions:   newSessionMap(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		toolScopes: newScopeRegistry(),
	}

	assert.Equal(t, []string{"svc"}, rollupKeys(t, srv, context.Background()),
		"a plain request keys the rollup off the indexed file node")

	viewCtx := WithOverlayView(
		context.Background(),
		graph.NewOverlaidView(store("billing"), graph.NewOverlayLayer()),
	)
	assert.Equal(t, []string{"billing"}, rollupKeys(t, srv, viewCtx),
		"the rollup must key off the file node the request's reader owns")

	require.NotNil(t, base.GetNode(rollupFile))
	assert.Equal(t, "svc", base.GetNode(rollupFile).RepoPrefix,
		"the request must not mutate the base store")
}

// TestSidecarRowsDropCapabilityUnderOverlay pins the conservative
// contract for the coverage and release sidecars: the capability
// assertion runs on the request reader, so an overlay-active request
// gets no rows at all rather than the indexed numbers for symbols its
// buffers already changed. Each row then falls back to the node's meta.
func TestSidecarRowsDropCapabilityUnderOverlay(t *testing.T) {
	srv, layer := overlayQualityFixture(t)
	covWriter, ok := srv.graph.(graph.CoverageEnrichmentWriter)
	require.True(t, ok, "the in-memory base store lost its coverage writer")
	require.NoError(t, covWriter.BulkSetCoverage(qualityRepo, []graph.CoverageEnrichment{
		{NodeID: qualityAlphaID, RepoPrefix: qualityRepo, CoveragePct: 42},
	}))
	relWriter, ok := srv.graph.(graph.ReleaseEnrichmentWriter)
	require.True(t, ok, "the in-memory base store lost its release writer")
	require.NoError(t, relWriter.BulkSetReleases(qualityRepo, []graph.ReleaseEnrichment{
		{NodeID: qualityAlphaID, RepoPrefix: qualityRepo, AddedIn: "v1.4"},
	}))

	cov := coverageRowsByID(srv.graph)
	require.Contains(t, cov, qualityAlphaID)
	assert.Equal(t, 42.0, cov[qualityAlphaID].CoveragePct)
	assert.Equal(t, "v1.4", releaseRowsByID(srv.graph)[qualityAlphaID])

	viewCtx := overlayCtx(t, srv, layer)
	assert.Nil(t, coverageRowsByID(srv.readerFor(viewCtx)), "coverage sidecar served base's rows under an overlay")
	assert.Nil(t, releaseRowsByID(srv.readerFor(viewCtx)), "release sidecar served base's rows under an overlay")
}
