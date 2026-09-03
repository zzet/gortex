package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// The review family (pr_review_context, suggested_review_questions, the
// review-pack classification) reads the graph through the per-request
// reader, so a session with pushed buffers is reviewed on what it is
// about to commit rather than on what is still on disk. These tests pin
// that: reverting a site to s.graph serves the indexed payload and keeps
// reporting a symbol the buffer deleted.

// The fixture keeps the two path vocabularies apart, because the review
// handlers are the seam between them: rf*File is the repo-relative,
// '/'-spelled path a forge or `git diff` hands the caller, and rf*Key is the
// key the store actually holds — the remainder in the indexing machine's
// native separators (see internal/graphpath), with no repo prefix here. They
// coincide on POSIX and diverge on Windows, where analysis.JoinFileNodes
// converts the caller's path before the lookup. Keying the graph with the
// '/'-joined form describes a file a Windows daemon never indexes, and the
// converted query then misses it.
const (
	rfKeptFile = "repo/edit.go"
	rfGoneFile = "repo/gone.go"
)

var (
	rfKeptKey = filepath.FromSlash(rfKeptFile)
	rfGoneKey = filepath.FromSlash(rfGoneFile)
	rfKeptID  = rfKeptKey + "::Kept"
	rfGoneID  = rfGoneKey + "::Gone"
)

// reviewFamilyServer wires a fully constructed server (engine included —
// the diff-context section walks callers through it) over a base graph.
func reviewFamilyServer(t *testing.T, g *graph.Graph) *Server {
	t.Helper()
	return NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
}

// reviewFamilyFixture is the two-file changeset shape the review handlers
// see: one file the buffer re-parsed (Kept moved down the file) and one
// file the buffer emptied (Gone deleted).
func reviewFamilyFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: rfKeptKey, Language: "go", StartLine: 10, EndLine: 14,
	})
	g.AddNode(&graph.Node{
		ID: rfGoneID, Name: "Gone", Kind: graph.KindFunction,
		FilePath: rfGoneKey, Language: "go", StartLine: 3, EndLine: 6,
	})

	// A layer covers a file under the key the store spells it with — the
	// same key the base graph is indexed by, so the buffer shadows the
	// indexed file instead of sitting beside it.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(rfKeptKey, false)
	layer.AddNode(rfKeptKey, &graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: rfKeptKey, Language: "go", StartLine: 40, EndLine: 44,
	})
	layer.MarkFile(rfGoneKey, false)
	layer.MarkRemoved("Gone", rfGoneID)

	return reviewFamilyServer(t, g), layer
}

// reviewFamilyPRReview is the slice of the pr_review_context envelope
// these tests read: the changed-file roll-up and the per-symbol rows,
// each carrying the start line whose provenance is under test.
type reviewFamilyPRReview struct {
	ChangedFiles []string `json:"changed_files"`
	DiffContext  []struct {
		ID   string `json:"id"`
		Line int    `json:"start_line"`
	} `json:"diff_context"`
}

// prReviewDiffContextFor drives pr_review_context over an explicit id set
// (no working tree needed) and decodes the diff_context section.
func prReviewDiffContextFor(t *testing.T, s *Server, ctx context.Context) reviewFamilyPRReview {
	t.Helper()
	res, err := s.handlePRReviewContext(ctx, makeReq("pr_review_context", map[string]any{
		"ids":      rfKeptID + "," + rfGoneID,
		"sections": "diff_context",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "pr_review_context errored: %s", toolResultText(res))
	var out reviewFamilyPRReview
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	return out
}

// TestPRReviewContextReflectsOverlay is the handler-level proof for the
// prReviewDiffFromIDs → buildDiffContextSection path: the changed-file
// roll-up and every enriched symbol row come from the caller's buffers.
func TestPRReviewContextReflectsOverlay(t *testing.T) {
	srv, layer := reviewFamilyFixture(t)

	lineByID := func(out reviewFamilyPRReview) map[string]int {
		m := make(map[string]int, len(out.DiffContext))
		for _, d := range out.DiffContext {
			m[d.ID] = d.Line
		}
		return m
	}

	// The roll-up is built from the enriched nodes' own file paths, so it
	// speaks the store's vocabulary rather than the caller's.
	onBase := prReviewDiffContextFor(t, srv, context.Background())
	assert.ElementsMatch(t, []string{rfKeptKey, rfGoneKey}, onBase.ChangedFiles,
		"a plain request reports both indexed files")
	baseLines := lineByID(onBase)
	assert.Equal(t, 10, baseLines[rfKeptID], "a plain request reports the indexed line")
	assert.Contains(t, baseLines, rfGoneID, "a plain request still enriches the indexed symbol")

	onView := prReviewDiffContextFor(t, srv, overlayCtx(t, srv, layer))
	assert.Equal(t, []string{rfKeptKey}, onView.ChangedFiles,
		"the file the buffer emptied must drop out of the changeset")
	viewLines := lineByID(onView)
	assert.Equal(t, 40, viewLines[rfKeptID], "the buffer's payload must replace the indexed one")
	assert.NotContains(t, viewLines, rfGoneID, "a symbol the buffer deleted must not be enriched")

	assert.Len(t, srv.graph.AllNodes(), 2, "the overlay request must not mutate the base store")
}

// TestClassifyChangedSymbolsReadsThroughRequestReader pins the review
// package's Reader widening (SymbolHunk + ClassifyChange): the change
// class is decided on the buffer's node kind, not the indexed one.
func TestClassifyChangedSymbolsReadsThroughRequestReader(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: rfKeptKey, Language: "go", StartLine: 10, EndLine: 14,
	})
	srv := reviewFamilyServer(t, g)

	// The buffer turned the function into a constant under the same id.
	layer := graph.NewOverlayLayer()
	layer.MarkFile(rfKeptKey, false)
	layer.AddNode(rfKeptKey, &graph.Node{
		ID: rfKeptID, Name: "Kept", Kind: graph.KindConstant,
		FilePath: rfKeptKey, Language: "go", StartLine: 10, EndLine: 10,
	})

	// A DiffResult carries both vocabularies: ChangedFiles keeps git's
	// repo-relative spelling, while a changed symbol's FilePath is copied
	// off the graph node it joined to.
	diff := &analysis.DiffResult{
		ChangedFiles:   []string{rfKeptFile},
		ChangedSymbols: []analysis.ChangedSymbol{{ID: rfKeptID, Name: "Kept", FilePath: rfKeptKey}},
	}

	onBase := srv.classifyChangedSymbols(context.Background(), diff, nil)
	require.Len(t, onBase, 1)
	assert.NotEqual(t, "config", onBase[0].Class,
		"a plain request classifies against the indexed function node")

	onView := srv.classifyChangedSymbols(overlayCtx(t, srv, layer), diff, nil)
	require.Len(t, onView, 1)
	assert.Equal(t, "config", onView[0].Class,
		"the buffer's node kind must decide the change class")
}

const (
	rqHubID      = "p/hub.go::Hub"
	rqCallerAID  = "p/a.go::A"
	rqCallerBID  = "p/b.go::B"
	rqTestFile   = "p/hub_test.go"
	rqTestSymbol = rqTestFile + "::TestHub"
)

// reviewQuestionsFixture wires a load-bearing symbol (two callers) that
// the index shows is covered by a test, plus the layer for a session
// whose buffer deleted that test.
func reviewQuestionsFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: rqHubID, Name: "Hub", Kind: graph.KindFunction, FilePath: "p/hub.go", Language: "go", StartLine: 5})
	g.AddNode(&graph.Node{ID: rqCallerAID, Name: "A", Kind: graph.KindFunction, FilePath: "p/a.go", Language: "go", StartLine: 3})
	g.AddNode(&graph.Node{ID: rqCallerBID, Name: "B", Kind: graph.KindFunction, FilePath: "p/b.go", Language: "go", StartLine: 3})
	g.AddNode(&graph.Node{ID: rqTestSymbol, Name: "TestHub", Kind: graph.KindFunction, FilePath: rqTestFile, Language: "go", StartLine: 7})
	g.AddEdge(&graph.Edge{From: rqCallerAID, To: rqHubID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rqCallerBID, To: rqHubID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rqTestSymbol, To: rqHubID, Kind: graph.EdgeTests})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(rqTestFile, false)
	layer.MarkRemoved("TestHub", rqTestSymbol)

	return reviewFamilyServer(t, g), layer
}

// untestedHotspotSymbols drives suggested_review_questions narrowed to the
// untested-hotspot category and returns the symbol ids it flagged.
func untestedHotspotSymbols(t *testing.T, s *Server, ctx context.Context) []string {
	t.Helper()
	res, err := s.handleSuggestedReviewQuestions(ctx, makeReq("suggested_review_questions", map[string]any{
		"categories":    "untested_hotspot",
		"hub_threshold": float64(2),
	}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "suggested_review_questions errored: %s", toolResultText(res))

	var out struct {
		Questions []struct {
			SymbolID string `json:"symbol_id"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	ids := make([]string, 0, len(out.Questions))
	for _, q := range out.Questions {
		ids = append(ids, q.SymbolID)
	}
	return ids
}

// TestSuggestedReviewQuestionsReflectsOverlay is the handler-level proof
// for the fan-count / inbound-test walk: once the buffer deletes the only
// covering test, the hub is reported as an untested hotspot even though
// the index still carries the tests edge.
func TestSuggestedReviewQuestionsReflectsOverlay(t *testing.T) {
	srv, layer := reviewQuestionsFixture(t)

	onBase := untestedHotspotSymbols(t, srv, context.Background())
	assert.NotContains(t, onBase, rqHubID,
		"the indexed tests edge must keep the hub off the untested list")

	onView := untestedHotspotSymbols(t, srv, overlayCtx(t, srv, layer))
	assert.Contains(t, onView, rqHubID,
		"deleting the covering test in the buffer must surface the hub")

	assert.Len(t, srv.graph.AllNodes(), 4, "the overlay request must not mutate the base store")
}

// TestChangedSymbolsForFilesReflectsOverlay pins the forge-file → symbol
// join (analysis.JoinFileNodes) to the request reader. It is the read every
// PR-shaped handler starts from, so reverting it to s.graph reports a symbol
// the buffer deleted as changed and dates the surviving one to the index.
func TestChangedSymbolsForFilesReflectsOverlay(t *testing.T) {
	srv, layer := reviewFamilyFixture(t)
	// A forge / git changed-file list is repo-relative and '/'-spelled in
	// every daemon; the join converts it to the store's key. Passing the
	// keys here instead would skip that conversion and stop covering it.
	files := []string{rfKeptFile, rfGoneFile}

	lineByID := func(nodes []*graph.Node) map[string]int {
		m := make(map[string]int, len(nodes))
		for _, n := range nodes {
			m[n.ID] = n.StartLine
		}
		return m
	}

	baseFiles, baseNodes := srv.changedSymbolsForFiles(context.Background(), "", files)
	assert.Equal(t, files, baseFiles, "the reported file list keeps the caller's paths")
	base := lineByID(baseNodes)
	assert.Equal(t, 10, base[rfKeptID], "a plain request joins the indexed payload")
	assert.Contains(t, base, rfGoneID, "a plain request still joins the indexed symbol")

	viewFiles, viewNodes := srv.changedSymbolsForFiles(overlayCtx(t, srv, layer), "", files)
	assert.Equal(t, files, viewFiles, "the file list is the caller's either way")
	view := lineByID(viewNodes)
	assert.Equal(t, 40, view[rfKeptID], "the buffer's payload must replace the indexed one")
	assert.NotContains(t, view, rfGoneID, "a symbol the buffer deleted must not join")

	assert.Len(t, srv.graph.AllNodes(), 2, "the overlay request must not mutate the base store")
}

// TestReviewReceiptReflectsOverlay pins the PR-risk scorer
// (analysis.ScorePRRisk and its covering-test walk) to the request reader.
// Reverting it to s.graph reads the indexed tests edge and calls a change
// merge-ready whose buffer has already deleted the only covering test.
func TestReviewReceiptReflectsOverlay(t *testing.T) {
	srv, layer := reviewQuestionsFixture(t)
	diff := &analysis.DiffResult{
		ChangedFiles:   []string{"p/hub.go"},
		ChangedSymbols: []analysis.ChangedSymbol{{ID: rqHubID, Name: "Hub", FilePath: "p/hub.go"}},
	}
	ids := []string{rqHubID}

	onBase := srv.reviewReceipt(context.Background(), ids, diff, nil, nil, false)
	assert.Equal(t, 0, onBase.UncoveredCount, "the indexed tests edge covers the hub")
	assert.Equal(t, "merge-ready", onBase.NextSafeAction)

	onView := srv.reviewReceipt(overlayCtx(t, srv, layer), ids, diff, nil, nil, false)
	assert.Equal(t, 1, onView.UncoveredCount,
		"deleting the covering test in the buffer must leave the hub uncovered")
	assert.Equal(t, "add-tests", onView.NextSafeAction,
		"the receipt must ask for tests the buffer no longer has")

	assert.Len(t, srv.graph.AllNodes(), 4, "the overlay request must not mutate the base store")
}
