package mcp

import (
	"context"
	"path"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

const (
	navRepo      = "repo"
	navStableFl  = "repo/stable.go"
	navBufferFl  = "repo/buffer.go"
	navTargetID  = navStableFl + "::Target"
	navKeptID    = navBufferFl + "::Kept"
	navDroppedID = navBufferFl + "::Dropped"
	navFreshID   = navBufferFl + "::Fresh"
)

// navOverlayServer wires a base graph plus the layer one editor buffer
// would push for repo/buffer.go: Kept is re-emitted under the same ID
// with a fresh payload, Dropped is gone from the buffer, and Fresh is a
// symbol that exists only in the buffer.
func navOverlayServer(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	base := graph.New()
	base.AddNode(&graph.Node{ID: navTargetID, Name: "Target", Kind: graph.KindInterface, FilePath: navStableFl, RepoPrefix: navRepo})
	base.AddNode(&graph.Node{ID: navKeptID, Name: "Kept", Kind: graph.KindType, FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 10})
	base.AddNode(&graph.Node{ID: navDroppedID, Name: "Dropped", Kind: graph.KindType, FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 20})
	base.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeImplements, FilePath: navBufferFl, Line: 10})
	base.AddEdge(&graph.Edge{From: navDroppedID, To: navTargetID, Kind: graph.EdgeImplements, FilePath: navBufferFl, Line: 20})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(navBufferFl, false)
	layer.AddNode(navBufferFl, &graph.Node{
		ID: navKeptID, Name: "Kept", Kind: graph.KindType,
		FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 40,
	})
	layer.AddNode(navBufferFl, &graph.Node{
		ID: navFreshID, Name: "Fresh", Kind: graph.KindType,
		FilePath: navBufferFl, RepoPrefix: navRepo, StartLine: 60,
	})
	layer.MarkRemoved("Dropped", navDroppedID)
	layer.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeImplements, FilePath: navBufferFl, Line: 41})

	return &Server{graph: base, engine: query.NewEngine(base)}, layer
}

// TestSymbolTargetResolutionReadsThroughRequestReader pins id_resolve's
// name/id resolution to the request reader: an overlay-active call
// resolves a name the buffer introduced, refuses a definition the buffer
// deleted, and reports the buffer's payload for a re-emitted ID — while
// the same calls against a plain context still answer from base.
func TestSymbolTargetResolutionReadsThroughRequestReader(t *testing.T) {
	server, layer := navOverlayServer(t)
	plain := context.Background()
	overlaid := overlayCtx(t, server, layer)

	// Deleted symbol absent: base resolves "Dropped", the overlay does not.
	if got := server.resolveNameToIDs(plain, "Dropped"); len(got) != 1 || got[0] != navDroppedID {
		t.Fatalf("resolveNameToIDs over the base store = %v, want [%s]", got, navDroppedID)
	}
	if got := server.resolveNameToIDs(overlaid, "Dropped"); len(got) != 0 {
		t.Fatalf("resolveNameToIDs served a symbol the buffer deleted: %v", got)
	}
	if id, cands := server.resolveSymbolTarget(overlaid, "Dropped"); id != "Dropped" || cands != nil {
		t.Fatalf("resolveSymbolTarget(%q) = (%q, %v), want the unresolved target back", "Dropped", id, cands)
	}

	// Buffer-only symbol visible: base cannot name it, the overlay can.
	if got := server.resolveNameToIDs(plain, "Fresh"); len(got) != 0 {
		t.Fatalf("base resolved a buffer-only symbol: %v", got)
	}
	if id, cands := server.resolveSymbolTarget(overlaid, "Fresh"); id != navFreshID || cands != nil {
		t.Fatalf("resolveSymbolTarget(%q) = (%q, %v), want %q", "Fresh", id, cands, navFreshID)
	}

	// Replaced payload visible: the re-emitted ID keeps resolving, and the
	// node behind it is the buffer's, not base's.
	if id, _ := server.resolveSymbolTarget(overlaid, "Kept"); id != navKeptID {
		t.Fatalf("resolveSymbolTarget(%q) = %q, want %q", "Kept", id, navKeptID)
	}
	if n := server.readerFor(overlaid).GetNode(navKeptID); n == nil || n.StartLine != 40 {
		t.Fatalf("the request reader served base's payload for %s: %+v", navKeptID, n)
	}

	// The base store must not have been touched by the overlay request.
	if n := server.graph.GetNode(navKeptID); n == nil || n.StartLine != 10 {
		t.Fatalf("the overlay request mutated the base store: %+v", n)
	}
}

// TestDispatchImplementorCountReadsThroughRequestReader pins the
// find_usages dispatch cue to the request reader: the implementor count
// drops the buffer-deleted implementation and keeps the re-emitted one,
// so the cue never advertises dispatch the buffer already removed.
func TestDispatchImplementorCountReadsThroughRequestReader(t *testing.T) {
	server, layer := navOverlayServer(t)

	if got := server.dispatchImplementorCount(context.Background(), navTargetID); got != 2 {
		t.Fatalf("dispatchImplementorCount over the base store = %d, want 2", got)
	}
	if got := server.dispatchImplementorCount(overlayCtx(t, server, layer), navTargetID); got != 1 {
		t.Fatalf("dispatchImplementorCount over the request reader = %d, want 1", got)
	}
}

// TestWhyEntriesReadThroughRequestReader pins the why handler's rationale
// projection to the request reader: the entry for a re-emitted rationale
// carries the buffer's text, and a rationale the buffer deleted no longer
// motivates the symbol.
func TestWhyEntriesReadThroughRequestReader(t *testing.T) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: navTargetID, Name: "Target", Kind: graph.KindFunction, FilePath: navStableFl, RepoPrefix: navRepo})
	base.AddNode(&graph.Node{
		ID: navKeptID, Name: "Kept", Kind: graph.KindRationale, FilePath: navBufferFl, RepoPrefix: navRepo,
		Meta: map[string]any{"rationale_kind": "decision", "section_text": "base text"},
	})
	base.AddNode(&graph.Node{
		ID: navDroppedID, Name: "Dropped", Kind: graph.KindRationale, FilePath: navBufferFl, RepoPrefix: navRepo,
		Meta: map[string]any{"rationale_kind": "decision", "section_text": "stale text"},
	})
	base.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeMotivates, FilePath: navBufferFl, Line: 3})
	base.AddEdge(&graph.Edge{From: navDroppedID, To: navTargetID, Kind: graph.EdgeMotivates, FilePath: navBufferFl, Line: 9})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(navBufferFl, false)
	layer.AddNode(navBufferFl, &graph.Node{
		ID: navKeptID, Name: "Kept", Kind: graph.KindRationale, FilePath: navBufferFl, RepoPrefix: navRepo,
		Meta: map[string]any{"rationale_kind": "decision", "section_text": "buffer text"},
	})
	layer.MarkRemoved("Dropped", navDroppedID)
	layer.AddEdge(&graph.Edge{From: navKeptID, To: navTargetID, Kind: graph.EdgeMotivates, FilePath: navBufferFl, Line: 4})

	server := &Server{graph: base, engine: query.NewEngine(base)}

	baseEntries := server.whyEntriesFor(context.Background(), navTargetID)
	if len(baseEntries) != 2 {
		t.Fatalf("whyEntriesFor over the base store returned %d entries, want 2: %+v", len(baseEntries), baseEntries)
	}

	entries := server.whyEntriesFor(overlayCtx(t, server, layer), navTargetID)
	if len(entries) != 1 {
		t.Fatalf("whyEntriesFor over the request reader returned %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].SourceID != navKeptID {
		t.Fatalf("whyEntriesFor kept the wrong rationale: %+v", entries[0])
	}
	if entries[0].Text != "buffer text" {
		t.Fatalf("whyEntriesFor served base's rationale text %q, want the buffer's", entries[0].Text)
	}
}

const (
	navBarrelFl  = "src/index.ts"
	navFileFwdFl = "src/barrel.ts"
	navImplFl    = "src/impl.ts"
	navAltFl     = "src/alt.ts"
	navBarrelID  = navBarrelFl + "::persist"
	navFileFwdID = navFileFwdFl + "::persist"
	navImplID    = navImplFl + "::persist"
	navAltID     = navAltFl + "::persist"

	// The two consumer sites that make the canonicalization observable in a
	// find_usages answer: one calls the indexed declaration, the other calls
	// the declaration the buffer forwards to.
	navUseImplFl = "src/use_impl.ts"
	navUseAltFl  = "src/use_alt.ts"
	navUseImplID = navUseImplFl + "::useImpl"
	navUseAltID  = navUseAltFl + "::useAlt"
)

// navBarrelServer wires the two shapes find_usages canonicalizes through
// before it answers: a barrel binding that exists as its own re-export
// node (src/index.ts::persist) and a file-level forward that mints no
// node of its own (src/barrel.ts::persist). The layer is what one editor
// session would push: the barrel buffer now forwards to src/alt.ts, and
// the src/impl.ts buffer no longer declares persist at all.
func navBarrelServer(t *testing.T) (*Server, *graph.OverlayLayer) {
	t.Helper()
	tsFile := func(p string) *graph.Node {
		return &graph.Node{ID: p, Name: path.Base(p), Kind: graph.KindFile, FilePath: p, RepoPrefix: navRepo, Language: "typescript"}
	}
	reExport := func(id, file string) *graph.Node {
		return &graph.Node{
			ID: id, Name: "persist", Kind: graph.KindFunction, FilePath: file, RepoPrefix: navRepo,
			Meta: map[string]any{"reexport": true},
		}
	}

	base := graph.New()
	base.AddNode(tsFile(navBarrelFl))
	base.AddNode(tsFile(navFileFwdFl))
	base.AddNode(tsFile(navImplFl))
	base.AddNode(tsFile(navAltFl))
	base.AddNode(tsFile(navUseImplFl))
	base.AddNode(tsFile(navUseAltFl))
	base.AddNode(&graph.Node{ID: navImplID, Name: "persist", Kind: graph.KindFunction, FilePath: navImplFl, RepoPrefix: navRepo})
	base.AddNode(&graph.Node{ID: navAltID, Name: "persist", Kind: graph.KindFunction, FilePath: navAltFl, RepoPrefix: navRepo})
	base.AddNode(reExport(navBarrelID, navBarrelFl))
	base.AddEdge(&graph.Edge{From: navBarrelID, To: navImplID, Kind: graph.EdgeReExports, FilePath: navBarrelFl, Line: 1})
	// One consumer per declaration, both outside the buffered files so only
	// the canonicalization target decides which one an answer carries.
	base.AddNode(&graph.Node{ID: navUseImplID, Name: "useImpl", Kind: graph.KindFunction, FilePath: navUseImplFl, RepoPrefix: navRepo})
	base.AddNode(&graph.Node{ID: navUseAltID, Name: "useAlt", Kind: graph.KindFunction, FilePath: navUseAltFl, RepoPrefix: navRepo})
	base.AddEdge(&graph.Edge{From: navUseImplID, To: navImplID, Kind: graph.EdgeCalls, FilePath: navUseImplFl, Line: 5})
	base.AddEdge(&graph.Edge{From: navUseAltID, To: navAltID, Kind: graph.EdgeCalls, FilePath: navUseAltFl, Line: 5})
	// The file-level forward carries only the pre-resolution import target,
	// so the binding id src/barrel.ts::persist has no node behind it.
	base.AddEdge(&graph.Edge{
		From: navFileFwdFl, To: "unresolved::import::./impl::persist",
		Kind: graph.EdgeReExports, FilePath: navFileFwdFl, Line: 1,
	})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(navBarrelFl, false)
	layer.AddNode(navBarrelFl, reExport(navBarrelID, navBarrelFl))
	layer.AddEdge(&graph.Edge{From: navBarrelID, To: navAltID, Kind: graph.EdgeReExports, FilePath: navBarrelFl, Line: 1})
	layer.MarkFile(navImplFl, false)
	layer.MarkRemoved("persist", navImplID)

	eng := query.NewEngine(base)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, base, nil, nil, zap.NewNop(), nil), layer
}

// barrelUsageIDs drives find_usages over one barrel id and returns the
// consumer ids the answer carries.
func barrelUsageIDs(t *testing.T, s *Server, ctx context.Context, id string) []string {
	t.Helper()
	res, err := s.handleFindUsages(ctx, makeReq("find_usages", map[string]any{
		"id": id, "min_tier": "text_matched",
	}))
	if err != nil {
		t.Fatalf("find_usages(%s): %v", id, err)
	}
	if res.IsError {
		// A binding that canonicalizes onto nothing is a clean not-found,
		// not a failure — it carries no usages, which is the answer.
		return nil
	}
	text := toolResultText(res)
	var out []string
	for _, candidate := range []string{navUseImplID, navUseAltID} {
		if strings.Contains(text, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

// TestReExportCanonicalizationReadsThroughRequestReader drives find_usages
// itself over the two barrel resolve-through shapes: an overlay-active call
// answers with the consumers of the buffer's forward instead of the indexed
// one, and stops canonicalizing onto a declaration the buffer deleted rather
// than answering with the usages of a symbol that is gone. Driving the
// handler is what makes a revert to the base store fail here — calling the
// helpers with an explicit reader would only catch a type change.
func TestReExportCanonicalizationReadsThroughRequestReader(t *testing.T) {
	server, layer := navBarrelServer(t)
	plain := context.Background()
	overlaid := overlayCtx(t, server, layer)

	// Re-export node: base merges impl's usages, the buffer forwards to alt.
	if got := barrelUsageIDs(t, server, plain, navBarrelID); !reflect.DeepEqual(got, []string{navUseImplID}) {
		t.Fatalf("find_usages(%s) over the base store = %v, want [%s]", navBarrelID, got, navUseImplID)
	}
	if got := barrelUsageIDs(t, server, overlaid, navBarrelID); !reflect.DeepEqual(got, []string{navUseAltID}) {
		t.Fatalf("find_usages(%s) over the request reader = %v, want the buffer's forward's consumer [%s]", navBarrelID, got, navUseAltID)
	}

	// File-level forward: base canonicalizes onto impl's declaration and
	// answers with its consumer; the buffer deleted that declaration, so the
	// overlay canonicalizes onto nothing and answers with no usages at all.
	if got := barrelUsageIDs(t, server, plain, navFileFwdID); !reflect.DeepEqual(got, []string{navUseImplID}) {
		t.Fatalf("find_usages(%s) over the base store = %v, want [%s]", navFileFwdID, got, navUseImplID)
	}
	if got := barrelUsageIDs(t, server, overlaid, navFileFwdID); len(got) != 0 {
		t.Fatalf("find_usages(%s) answered with the usages of a declaration the buffer deleted: %v", navFileFwdID, got)
	}

	// The base store must not have been touched by the overlay request.
	if n := server.graph.GetNode(navImplID); n == nil {
		t.Fatal("the overlay request removed the declaration from the base store")
	}
	if edges := server.graph.GetOutEdges(navBarrelID); len(edges) != 1 || edges[0].To != navImplID {
		t.Fatalf("the overlay request rewrote the base store's forward: %+v", edges)
	}
}

// TestGraphExportReadsThroughRequestReader drives the export_graph handler:
// an overlay-active export writes the buffer's symbols and omits the ones the
// buffer deleted, instead of shipping a snapshot of the indexed tree the
// caller has already moved past. Going through the handler is what makes a
// revert of its reader wiring fail here.
func TestGraphExportReadsThroughRequestReader(t *testing.T) {
	server, layer := navOverlayServer(t)

	render := func(ctx context.Context, format string) string {
		t.Helper()
		res, err := server.handleExportGraph(ctx, makeReq("export_graph", map[string]any{"format": format}))
		if err != nil {
			t.Fatalf("export_graph(%s): %v", format, err)
		}
		if res.IsError {
			t.Fatalf("export_graph(%s) errored: %s", format, toolResultText(res))
		}
		return toolResultText(res)
	}

	for _, format := range []string{"cypher", "graphml"} {
		base := render(context.Background(), format)
		if !strings.Contains(base, navDroppedID) {
			t.Fatalf("%s export over the base store omitted %s:\n%s", format, navDroppedID, base)
		}
		if strings.Contains(base, navFreshID) {
			t.Fatalf("%s export over the base store invented %s:\n%s", format, navFreshID, base)
		}

		overlaid := render(overlayCtx(t, server, layer), format)
		if strings.Contains(overlaid, navDroppedID) {
			t.Fatalf("%s export shipped a symbol the buffer deleted:\n%s", format, overlaid)
		}
		if !strings.Contains(overlaid, navFreshID) {
			t.Fatalf("%s export omitted the buffer-only symbol %s:\n%s", format, navFreshID, overlaid)
		}
	}

	// The base store must not have been touched by the overlay request.
	if n := server.graph.GetNode(navFreshID); n != nil {
		t.Fatalf("the overlay export wrote a buffer symbol into the base store: %+v", n)
	}
	if n := server.graph.GetNode(navDroppedID); n == nil {
		t.Fatal("the overlay export removed a symbol from the base store")
	}
}
