package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// readBootstrapResource drives a bootstrap resource through its real
// production wrapper — boundResourceHandler, which is where requestScoped
// binds the session's view to a resources/read — and decodes the JSON body.
func readBootstrapResource(t *testing.T, srv *Server, uri, cwd string) map[string]any {
	t.Helper()
	req := mcplib.ReadResourceRequest{}
	req.Params.URI = uri
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd)
	contents, err := srv.boundResourceHandler(uri, srv.handleResourceIndexHealth)(ctx, req)
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	if len(contents) != 1 {
		t.Fatalf("read %s returned %d contents, want 1", uri, len(contents))
	}
	text, ok := contents[0].(mcplib.TextResourceContents)
	if !ok {
		t.Fatalf("read %s returned %T, want TextResourceContents", uri, contents[0])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatalf("decode %s: %v\n%s", uri, err, text.Text)
	}
	return payload
}

// wantedIndexHealthScope is what the payload must name: the capabilities the
// index_health tool already declares base-scoped on its rider.
func wantedIndexHealthScope() []string {
	return sortedCapabilityNames(baseScopedEngineCapabilities["index_health"])
}

func payloadScope(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, present := payload[indexHealthScopeField]
	if !present {
		return nil
	}
	entries, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s = %v, want a list of capability names", indexHealthScopeField, raw)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, _ := entry.(string)
		out = append(out, name)
	}
	return out
}

// gortex://index-health answers out of the indexed corpus whatever view
// the session is bound to, and until now it had no way to say so.
//
// gortex://stats is view-scoped — handleResourceStats -> buildGraphStatsPayload
// reads s.engineFor(ctx) / s.readerFor(ctx), and requestScoped puts the
// session's view on that ctx for every resources/read. handleResourceIndexHealth
// has the same view on the same ctx and ignores it: the payload comes from
// s.graph.Stats() plus a NodesByKind walk of the corpus and the indexer's mtime
// ledger, all generation zero. The index_health TOOL already declares that on
// its rider (baseScopedEngineCapabilities); a resource carries no rider at all,
// so an agent bound to a worktree read corpus-wide numbers as if they described
// its own checkout, with nothing in the answer to contradict that reading.
func TestIndexHealthResourceNamesTheCorpusItDescribes(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.indexer = rootOnlyIndexer(stack.repoRoot)

	t.Run("an unrouted read is unchanged", func(t *testing.T) {
		payload := readBootstrapResource(t, stack.srv, "gortex://index-health", stack.repoRoot)
		if got := payloadScope(t, payload); got != nil {
			t.Errorf("%s = %v on an unrouted read: reading the corpus IS the answer there",
				indexHealthScopeField, got)
		}
		if _, present := payload["health_score"]; !present {
			t.Fatalf("the probe returned no health payload at all: %v", payload)
		}
	})

	t.Run("a routed read says which corpus it describes", func(t *testing.T) {
		payload := readBootstrapResource(t, stack.srv, "gortex://index-health", stack.worktreeRoot)
		got := payloadScope(t, payload)
		if got == nil {
			t.Fatalf("a routed read carries no %s: %v", indexHealthScopeField, payload)
		}
		want := wantedIndexHealthScope()
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", indexHealthScopeField, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", indexHealthScopeField, got, want)
			}
		}
		for _, capability := range []graphview.CapabilityID{
			graphview.CapSyntaxGraph, graphview.CapSourceSnapshot,
		} {
			found := false
			for _, name := range got {
				found = found || name == string(capability)
			}
			if !found {
				t.Errorf("%s = %v, want it to name %s", indexHealthScopeField, got, capability)
			}
		}
	})
}

// The stamp must not be charged to the probe. index_health is the call an agent
// makes to find out whether the daemon is up, so it must stay cheap under a
// view too: the scope statement is read off the request's view and costs no
// whole-generation COUNT(*) of its own.
func TestIndexHealthResourceStaysACheapProbeUnderAView(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.indexer = rootOnlyIndexer(stack.repoRoot)
	counting := &countingReader{Store: stack.store}
	stack.srv.graph = counting
	t.Cleanup(func() { stack.srv.graph = stack.store })

	payload := readBootstrapResource(t, stack.srv, "gortex://index-health", stack.worktreeRoot)
	if got := payloadScope(t, payload); got == nil {
		t.Fatalf("the routed read carries no %s, so this measured the wrong path: %v",
			indexHealthScopeField, payload)
	}
	if n := counting.counts(); n != 0 {
		t.Errorf("one routed index-health resource read made %d whole-generation count scans, want 0", n)
	}
}

// The resource's description promises "Same payload as the `index_health`
// tool", so the statement lands on both surfaces or the promise stops being
// true. On the tool it is a second copy of what the rider already carries; on
// the resource it is the only copy there is.
func TestIndexHealthToolCarriesTheSameScopeStatementAsTheResource(t *testing.T) {
	stack := newViewStack(t)
	stack.srv.indexer = rootOnlyIndexer(stack.repoRoot)
	// A primed cache makes the tool answer from the snapshot rather than from
	// the "refreshing" placeholder, which is the shape the resource mirrors.
	stack.srv.indexHealth.mu.Lock()
	stack.srv.indexHealth.payload = map[string]any{"health_score": 100.0, "node_count": 7}
	stack.srv.indexHealth.updatedAt = time.Now()
	stack.srv.indexHealth.mu.Unlock()

	res, err := stack.callHandler(t, stack.worktreeRoot, "index_health",
		map[string]any{"format": "json"}, stack.srv.handleIndexHealth)
	if err != nil {
		t.Fatalf("index_health: %v", err)
	}
	if res.IsError {
		t.Fatalf("index_health: %s", viewResultText(t, res))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &payload); err != nil {
		t.Fatalf("decode the tool payload: %v\n%s", err, viewResultText(t, res))
	}
	got := payloadScope(t, payload)
	if got == nil {
		t.Fatalf("the routed tool payload carries no %s: %v", indexHealthScopeField, payload)
	}
	want := wantedIndexHealthScope()
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", indexHealthScopeField, got, want)
		}
	}

	// The control: an unrouted tool call is byte-unchanged, so the statement
	// costs nothing on the shape most calls have.
	plain, err := stack.callHandler(t, stack.repoRoot, "index_health",
		map[string]any{"format": "json"}, stack.srv.handleIndexHealth)
	if err != nil {
		t.Fatalf("index_health (unrouted): %v", err)
	}
	var plainPayload map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, plain)), &plainPayload); err != nil {
		t.Fatalf("decode the unrouted tool payload: %v", err)
	}
	if got := payloadScope(t, plainPayload); got != nil {
		t.Errorf("%s = %v on an unrouted tool call", indexHealthScopeField, got)
	}
}

// The cached snapshot is shared by every session, so the stamp is applied to a
// clone at read time. A routed read must never leave its label where an
// unrouted one will find it.
func TestIndexHealthScopeStampNeverReachesTheSharedCache(t *testing.T) {
	cached := map[string]any{"health_score": 100.0, "node_count": 7}
	routed := withIndexHealthCorpusScope(
		withRequestView(context.Background(), &requestView{reader: graph.New()}), cached)
	if _, present := cached[indexHealthScopeField]; present {
		t.Fatalf("the routed read stamped the shared payload: %v", cached)
	}
	if _, present := routed[indexHealthScopeField]; !present {
		t.Fatalf("the routed copy was not stamped: %v", routed)
	}
	if same := withIndexHealthCorpusScope(context.Background(), cached); len(same) != len(cached) {
		t.Fatalf("an unrouted read changed the payload: %v", same)
	}
}
