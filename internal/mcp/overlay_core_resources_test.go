package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// These tests pin the two halves of the core/resource threading: the
// request handlers in this area read the caller's buffers rather than the
// base store, and a resource read gets the same view installed that a tool
// call gets.

const (
	coreServeID  = "p/api.go::Serve"
	coreCallID   = "p/client.go::Call"
	coreLegacyID = "p/client.go::Legacy"
	coreClientFl = "p/client.go"
)

// overlayCoreFixture wires a handler-capable server over a two-file base
// graph plus the layer an editor session would push: client.go is re-parsed
// with Call moved down the file (so its call site into Serve moves with it)
// and Legacy deleted.
func overlayCoreFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: coreServeID, Name: "Serve", Kind: graph.KindFunction, FilePath: "p/api.go", Language: "go", StartLine: 5})
	g.AddNode(&graph.Node{ID: coreCallID, Name: "Call", Kind: graph.KindFunction, FilePath: coreClientFl, Language: "go", StartLine: 7})
	g.AddNode(&graph.Node{ID: coreLegacyID, Name: "Legacy", Kind: graph.KindFunction, FilePath: coreClientFl, Language: "go", StartLine: 30})
	g.AddEdge(&graph.Edge{From: coreCallID, To: coreServeID, Kind: graph.EdgeCalls, FilePath: coreClientFl, Line: 20})
	g.AddEdge(&graph.Edge{From: coreLegacyID, To: coreServeID, Kind: graph.EdgeCalls, FilePath: coreClientFl, Line: 33})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(coreClientFl, false)
	layer.AddNode(coreClientFl, &graph.Node{
		ID: coreCallID, Name: "Call", Kind: graph.KindFunction,
		FilePath: coreClientFl, Language: "go", StartLine: 70,
	})
	layer.MarkRemoved("Legacy", coreLegacyID)
	layer.AddEdge(&graph.Edge{From: coreCallID, To: coreServeID, Kind: graph.EdgeCalls, FilePath: coreClientFl, Line: 83})

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

// checkReferencesFor drives check_references and decodes its payload.
func checkReferencesFor(t *testing.T, s *Server, ctx context.Context, symbolID string) map[string]any {
	t.Helper()
	res, err := s.handleCheckReferences(ctx, makeReq("check_references", map[string]any{"symbol_id": symbolID}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "check_references errored: %s", toolResultText(res))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	return out
}

// evidenceLineFrom returns the recorded call-site line for one origin symbol.
func evidenceLineFrom(t *testing.T, payload map[string]any, fromID string) (int, bool) {
	t.Helper()
	rows, _ := payload["evidence"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row == nil {
			continue
		}
		if id, _ := row["from_id"].(string); id != fromID {
			continue
		}
		line, _ := row["line"].(float64)
		return int(line), true
	}
	return 0, false
}

// TestCheckReferencesReflectsOverlay is the handler-level proof for the
// check_references read path: the in-edge walk, the batched origin fetch,
// and the same-name scan all run over the caller's buffers. Reverting any
// of them to s.graph re-reports the deleted caller and the on-disk line.
func TestCheckReferencesReflectsOverlay(t *testing.T) {
	srv, layer := overlayCoreFixture(t)

	onBase := checkReferencesFor(t, srv, context.Background(), coreServeID)
	assert.Equal(t, float64(2), onBase["total_references"], "a plain request counts both indexed callers")
	line, ok := evidenceLineFrom(t, onBase, coreCallID)
	require.True(t, ok)
	assert.Equal(t, 20, line, "a plain request reports the indexed call site")
	_, ok = evidenceLineFrom(t, onBase, coreLegacyID)
	assert.True(t, ok, "a plain request still sees the indexed caller")

	onView := checkReferencesFor(t, srv, overlayCtx(t, srv, layer), coreServeID)
	assert.Equal(t, float64(1), onView["total_references"], "the deleted caller must drop out of the count")
	line, ok = evidenceLineFrom(t, onView, coreCallID)
	require.True(t, ok)
	assert.Equal(t, 83, line, "the evidence row must carry the buffer's call site")
	_, ok = evidenceLineFrom(t, onView, coreLegacyID)
	assert.False(t, ok, "a caller the buffer deleted must not be reported as evidence")

	assert.Len(t, srv.graph.AllNodes(), 3, "the overlay request must not mutate the base store")
}

// replayEpisodeFor drives replay_episode and returns the radius size the
// caller walk produced plus the caller rows it resolved, keyed by ID.
func replayEpisodeFor(t *testing.T, s *Server, ctx context.Context, anchor string) (int, map[string]struct{}) {
	t.Helper()
	res, err := s.handleReplayEpisode(ctx, makeReq("replay_episode", map[string]any{"anchor_symbol": anchor}))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "replay_episode errored: %s", toolResultText(res))
	var out struct {
		RadiusSize int `json:"radius_size"`
		Callers    []struct {
			ID string `json:"id"`
		} `json:"callers"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &out))
	ids := make(map[string]struct{}, len(out.Callers))
	for _, c := range out.Callers {
		ids[c.ID] = struct{}{}
	}
	return out.RadiusSize, ids
}

// TestReplayEpisodeReflectsOverlay pins both graph reads behind
// replay_episode to the request reader — the bounded caller walk (radius_size)
// and the batched row fetch (callers). Reverting either one puts the caller
// the buffer deleted back into the episode's blast radius.
func TestReplayEpisodeReflectsOverlay(t *testing.T) {
	srv, layer := overlayCoreFixture(t)

	baseRadius, onBase := replayEpisodeFor(t, srv, context.Background(), coreServeID)
	assert.Equal(t, 3, baseRadius, "a plain walk reaches the anchor and both indexed callers")
	assert.Contains(t, onBase, coreCallID, "a plain request walks the indexed callers")
	assert.Contains(t, onBase, coreLegacyID, "a plain request still sees the indexed caller")

	viewRadius, onView := replayEpisodeFor(t, srv, overlayCtx(t, srv, layer), coreServeID)
	assert.Equal(t, 2, viewRadius, "the buffer's deletion must shrink the walked radius")
	assert.Contains(t, onView, coreCallID, "the surviving caller stays in the radius")
	assert.NotContains(t, onView, coreLegacyID, "a caller the buffer deleted must not be in the radius")
}

// readResourceThroughFirewall drives a resource handler through the same
// registration wrapper addResource installs, so the test exercises the real
// overlay installation rather than calling the handler directly.
func readResourceThroughFirewall(t *testing.T, s *Server, ctx context.Context, uri string, h func(context.Context, mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error)) map[string]any {
	t.Helper()
	req := mcplib.ReadResourceRequest{}
	req.Params.URI = uri
	contents, err := s.boundResourceHandler(uri, h)(ctx, req)
	require.NoError(t, err)
	require.Len(t, contents, 1)
	text, ok := contents[0].(mcplib.TextResourceContents)
	require.True(t, ok, "resource contents were not text: %T", contents[0])
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &out))
	return out
}

// overlayResourceFixture indexes two files that each carry a TODO and wires
// the server with a live overlay manager, so a pushed buffer for one of them
// is the only difference between a plain read and an overlay-active one.
func overlayResourceFixture(t *testing.T) (srv *Server, editedFile string) {
	t.Helper()
	dir := t.TempDir()
	editedFile = filepath.Join(dir, "edited.go")
	require.NoError(t, os.WriteFile(editedFile,
		[]byte("package main\n\n// TODO: indexed question\nfunc Edited() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kept.go"),
		[]byte("package main\n\n// TODO: kept question\nfunc Kept() {}\n"), 0o644))

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	srv = NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	srv.SetOverlayManager(daemon.NewOverlayManager(time.Minute))
	return srv, editedFile
}

// questionTexts indexes the gortex://questions payload by TODO text.
func questionTexts(payload map[string]any) map[string]bool {
	out := map[string]bool{}
	rows, _ := payload["questions"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row == nil {
			continue
		}
		if text, _ := row["text"].(string); text != "" {
			out[text] = true
		}
	}
	return out
}

// TestResourceReadReflectsOverlay is the structural regression: resource
// handlers never had the per-request overlay view installed, so every
// resource read answered from the base graph while the session's buffers
// were live — gortex://report and gortex://questions disagreed with every
// tool call in the same session. The bound resource handler now prepares
// the request the way the tool wrapper does. Dropping that wrapper puts
// both payloads back on base and fails here.
func TestResourceReadReflectsOverlay(t *testing.T) {
	srv, editedFile := overlayResourceFixture(t)
	const sessionID = "resource-overlay"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, ""))

	baseReport := readResourceThroughFirewall(t, srv, context.Background(),
		"gortex://report", srv.handleResourceReport)
	baseNodes, ok := baseReport["total_nodes"].(float64)
	require.True(t, ok, "report payload has no total_nodes: %v", baseReport)

	baseQuestions := questionTexts(readResourceThroughFirewall(t, srv, context.Background(),
		"gortex://questions", srv.handleResourceQuestions))
	assert.True(t, baseQuestions["indexed question"], "a plain read reports the indexed TODO")
	assert.True(t, baseQuestions["kept question"], "a plain read reports the untouched file's TODO")

	// The buffer drops the TODO and replaces the one function with three,
	// so the buffer's file holds more nodes than the indexed one — a net
	// gain the count assertion below can see. (The file node itself is a
	// node in both, and the layer's copy replaces base's rather than
	// adding to it.)
	require.NoError(t, srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
		Path:    editedFile,
		Content: "package main\n\nfunc Alpha() {}\n\nfunc Beta() {}\n\nfunc Gamma() {}\n",
	}, nil))
	sessionCtx := WithSessionID(context.Background(), sessionID)

	overlaidReport := readResourceThroughFirewall(t, srv, sessionCtx,
		"gortex://report", srv.handleResourceReport)
	overlaidNodes, ok := overlaidReport["total_nodes"].(float64)
	require.True(t, ok)
	assert.Greater(t, overlaidNodes, baseNodes,
		"the resource read must count the symbols the buffer added")

	overlaidQuestions := questionTexts(readResourceThroughFirewall(t, srv, sessionCtx,
		"gortex://questions", srv.handleResourceQuestions))
	assert.False(t, overlaidQuestions["indexed question"],
		"a TODO the buffer removed must not survive the resource read")
	assert.True(t, overlaidQuestions["kept question"],
		"an untouched file keeps its TODO — the read is overlaid, not empty")

	// The on-disk file is untouched, so a request with no buffers still
	// reads base: the overlay never leaks across sessions.
	onDisk, err := os.ReadFile(editedFile)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "func Edited()")
	againQuestions := questionTexts(readResourceThroughFirewall(t, srv, context.Background(),
		"gortex://questions", srv.handleResourceQuestions))
	assert.True(t, againQuestions["indexed question"], "a request with no buffers still reads base")
}
