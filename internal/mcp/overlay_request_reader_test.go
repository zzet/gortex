package mcp

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/trigram"
)

// overlayCtx is the shared seam for handler tests that need an
// overlay-active request. It returns the context a tool handler would
// receive when the calling session pushed `layer`: the context carries
// a view of the layer over the server's own base graph, so
// s.readerFor(ctx) / s.engineFor(ctx) resolve to the overlay while the
// base graph itself stays untouched.
func overlayCtx(t *testing.T, s *Server, layer *graph.OverlayLayer) context.Context {
	t.Helper()
	if s == nil || s.graph == nil {
		t.Fatal("overlayCtx needs a server with a base graph")
	}
	return WithOverlayView(context.Background(), graph.NewOverlaidView(s.graph, layer))
}

const (
	overlayReaderRepo   = "repo"
	overlayReaderKeepFl = "repo/keep.go"
	overlayReaderEditFl = "repo/edit.go"
	overlayReaderKeepID = overlayReaderKeepFl + "::Keeper"
	overlayReaderKeptID = overlayReaderEditFl + "::Kept"
	overlayReaderGoneID = overlayReaderEditFl + "::Gone"
)

// overlayReaderFixture wires a server over a base graph and the layer
// that replaces one covered symbol under the same ID (with a fresh
// payload and a fresh call site) and hides the other.
func overlayReaderFixture(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	base := graph.New()
	base.AddNode(&graph.Node{ID: overlayReaderKeepID, Name: "Keeper", Kind: graph.KindFunction, FilePath: overlayReaderKeepFl, RepoPrefix: overlayReaderRepo})
	base.AddNode(&graph.Node{ID: overlayReaderKeptID, Name: "Kept", Kind: graph.KindFunction, FilePath: overlayReaderEditFl, RepoPrefix: overlayReaderRepo, StartLine: 10})
	base.AddNode(&graph.Node{ID: overlayReaderGoneID, Name: "Gone", Kind: graph.KindFunction, FilePath: overlayReaderEditFl, RepoPrefix: overlayReaderRepo})
	base.AddEdge(&graph.Edge{From: overlayReaderKeepID, To: overlayReaderKeptID, Kind: graph.EdgeCalls, FilePath: overlayReaderKeepFl, Line: 10})
	base.AddEdge(&graph.Edge{From: overlayReaderKeepID, To: overlayReaderGoneID, Kind: graph.EdgeCalls, FilePath: overlayReaderKeepFl, Line: 11})
	base.AddEdge(&graph.Edge{From: overlayReaderKeptID, To: overlayReaderKeepID, Kind: graph.EdgeCalls, FilePath: overlayReaderEditFl, Line: 20})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(overlayReaderEditFl, false)
	layer.AddNode(overlayReaderEditFl, &graph.Node{
		ID: overlayReaderKeptID, Name: "Kept", Kind: graph.KindFunction,
		FilePath: overlayReaderEditFl, RepoPrefix: overlayReaderRepo, StartLine: 40,
	})
	layer.MarkRemoved("Gone", overlayReaderGoneID)
	layer.AddEdge(&graph.Edge{From: overlayReaderKeptID, To: overlayReaderKeepID, Kind: graph.EdgeCalls, FilePath: overlayReaderEditFl, Line: 21})
	return &Server{graph: base}, layer
}

func overlayReaderEdgeKeys(seq func(func(*graph.Edge) bool)) []string {
	var out []string
	for e := range seq {
		out = append(out, fmt.Sprintf("%s->%s|%s:%d", e.From, e.To, e.FilePath, e.Line))
	}
	sort.Strings(out)
	return out
}

func overlayReaderNodeKeys(seq func(func(*graph.Node) bool)) []string {
	var out []string
	for n := range seq {
		out = append(out, fmt.Sprintf("%s:%d", n.ID, n.StartLine))
	}
	sort.Strings(out)
	return out
}

// TestEdgesByKindsReadsThroughRequestReader pins the widened
// edgesByKinds signature: handed the request reader it streams the
// overlay's edge relation — the layer's replacement call site instead of
// base's, and nothing pointing at a symbol the buffer deleted. Handed
// the base store it still streams base's.
func TestEdgesByKindsReadsThroughRequestReader(t *testing.T) {
	server, layer := overlayReaderFixture(t)
	ctx := overlayCtx(t, server, layer)

	wantBase := []string{
		overlayReaderKeepID + "->" + overlayReaderGoneID + "|" + overlayReaderKeepFl + ":11",
		overlayReaderKeepID + "->" + overlayReaderKeptID + "|" + overlayReaderKeepFl + ":10",
		overlayReaderKeptID + "->" + overlayReaderKeepID + "|" + overlayReaderEditFl + ":20",
	}
	sort.Strings(wantBase)
	if got := overlayReaderEdgeKeys(edgesByKinds(server.graph, graph.EdgeCalls)); !equalStringSlices(got, wantBase) {
		t.Fatalf("edgesByKinds over the base store = %v, want %v", got, wantBase)
	}

	wantOverlay := []string{
		overlayReaderKeepID + "->" + overlayReaderKeptID + "|" + overlayReaderKeepFl + ":10",
		overlayReaderKeptID + "->" + overlayReaderKeepID + "|" + overlayReaderEditFl + ":21",
	}
	sort.Strings(wantOverlay)
	got := overlayReaderEdgeKeys(edgesByKinds(server.readerFor(ctx), graph.EdgeCalls))
	if !equalStringSlices(got, wantOverlay) {
		t.Fatalf("edgesByKinds over the request reader = %v, want %v", got, wantOverlay)
	}

	// The base store must not have been touched by the overlay request.
	if got := overlayReaderEdgeKeys(edgesByKinds(server.graph, graph.EdgeCalls)); !equalStringSlices(got, wantBase) {
		t.Fatalf("the overlay request mutated the base store: %v", got)
	}
}

// TestNodesByKindReadsThroughRequestReader is the node-shaped sibling:
// the request reader reports the layer's payload for the re-emitted ID
// exactly once and drops the symbol the buffer deleted.
func TestNodesByKindReadsThroughRequestReader(t *testing.T) {
	server, layer := overlayReaderFixture(t)
	ctx := overlayCtx(t, server, layer)

	wantBase := []string{
		overlayReaderGoneID + ":0",
		overlayReaderKeepID + ":0",
		overlayReaderKeptID + ":10",
	}
	sort.Strings(wantBase)
	if got := overlayReaderNodeKeys(server.graph.NodesByKind(graph.KindFunction)); !equalStringSlices(got, wantBase) {
		t.Fatalf("NodesByKind over the base store = %v, want %v", got, wantBase)
	}

	wantOverlay := []string{
		overlayReaderKeepID + ":0",
		overlayReaderKeptID + ":40",
	}
	sort.Strings(wantOverlay)
	got := overlayReaderNodeKeys(server.readerFor(ctx).NodesByKind(graph.KindFunction))
	if !equalStringSlices(got, wantOverlay) {
		t.Fatalf("NodesByKind over the request reader = %v, want %v", got, wantOverlay)
	}
}

// TestBlameRowsByIDDropsCapabilityUnderOverlay pins the conservative
// contract for the widened blameRowsByID: the sidecar assertion runs on
// the request reader, so an overlay-active request gets no blame at all
// rather than base's rows for symbols the buffer already changed.
func TestBlameRowsByIDDropsCapabilityUnderOverlay(t *testing.T) {
	server, layer := overlayReaderFixture(t)
	writer, ok := server.graph.(graph.BlameEnrichmentWriter)
	if !ok {
		t.Fatal("the in-memory base store lost its blame writer")
	}
	if err := writer.BulkSetBlame(overlayReaderRepo, []graph.BlameEnrichment{
		{NodeID: overlayReaderKeptID, Commit: "c0ffee", Email: "author@example.com", Timestamp: 1},
	}); err != nil {
		t.Fatalf("BulkSetBlame: %v", err)
	}

	if rows := blameRowsByID(server.graph); len(rows) != 1 || rows[overlayReaderKeptID].Commit != "c0ffee" {
		t.Fatalf("blameRowsByID over the base store = %v, want the seeded row", rows)
	}
	if rows := blameRowsByID(server.readerFor(overlayCtx(t, server, layer))); rows != nil {
		t.Fatalf("blameRowsByID served base's sidecar under an overlay: %v", rows)
	}
}

const (
	textAttrRepo    = "textrepo"
	textAttrOldFl   = textAttrRepo + "/old.go"
	textAttrNewFl   = textAttrRepo + "/new.go"
	textAttrMovedID = textAttrNewFl + "::Moved"
)

// TestSearchTextScopeAttributionReadsRequestReader pins search_text's
// scope attribution to the request's reader. The reader is whatever view
// the request carries, and that view keeps its own base — here a session
// that renamed a file, so the file node lives under the new path there
// and under the old one in the server's store. A literal hit in the
// renamed file can only be attributed to an in-scope node through the
// request's reader; attributing it against the server's store finds no
// node and the fail-closed drop empties the result.
func TestSearchTextScopeAttributionReadsRequestReader(t *testing.T) {
	fileNode := func(path string) *graph.Node {
		return &graph.Node{
			ID: path, Name: path, Kind: graph.KindFile, FilePath: path,
			RepoPrefix: textAttrRepo, WorkspaceID: textAttrRepo,
		}
	}
	base := graph.New()
	base.AddNode(fileNode(textAttrOldFl))

	session := graph.New()
	session.AddNode(fileNode(textAttrNewFl))
	layer := graph.NewOverlayLayer()
	layer.MarkFile(textAttrNewFl, false)
	// A covered file's node list carries the file's own node: the parse of
	// the buffer emits one, and the layer owns every node in the file it
	// covers — including the one attribution looks up by path.
	layer.AddNode(textAttrNewFl, fileNode(textAttrNewFl))
	layer.AddNode(textAttrNewFl, &graph.Node{
		ID: textAttrMovedID, Name: "Moved", Kind: graph.KindFunction,
		FilePath: textAttrNewFl, RepoPrefix: textAttrRepo, WorkspaceID: textAttrRepo, StartLine: 3,
	})

	server := &Server{graph: base}
	matches := []trigram.Match{{Path: textAttrNewFl, Line: 3, Text: "// marker"}}
	resolved := ResolvedScope{
		WorkspaceID: textAttrRepo,
		RepoAllow:   map[string]bool{textAttrRepo: true},
	}

	onBase := server.filterTextMatchesByResolvedScope(context.Background(), matches, resolved)
	if len(onBase) != 0 {
		t.Fatalf("plain request kept %d matches, want the fail-closed drop for an unknown path", len(onBase))
	}

	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(session, layer))
	onView := server.filterTextMatchesByResolvedScope(ctx, matches, resolved)
	if len(onView) != 1 || onView[0].Path != textAttrNewFl {
		t.Fatalf("request-reader attribution = %v, want the renamed file's match kept", onView)
	}

	if n := base.GetNode(textAttrNewFl); n != nil {
		t.Fatalf("the request mutated the base store: %v", n)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
