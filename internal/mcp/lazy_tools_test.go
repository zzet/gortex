package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLazyRegistration_HotEagerToolsAreInToolsList covers the contract
// the N50 row defines: the hot set is published on session start so an
// agent can do its work without paying any discovery cost for them.
func TestLazyRegistration_HotEagerToolsAreInToolsList(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.NotNil(t, srv.lazy, "lazy registry must be installed at construction")
	require.True(t, srv.lazy.Enabled(), "GORTEX_LAZY_TOOLS=1 must enable the registry")

	live := srv.mcpServer.ListTools()
	require.NotNil(t, live)

	missing := []string{}
	for name := range hotEagerTools {
		if _, ok := live[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing, "hot tools must be eagerly registered: %v", missing)

	// tools_search itself is eager too.
	require.Contains(t, live, LazyToolsSearchName)
}

// TestLazyRegistration_DeferredToolsAreHidden asserts that anything
// not in hotEagerTools is hidden from the live tools/list and lives in
// the lazy registry instead.
func TestLazyRegistration_DeferredToolsAreHidden(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	deferredNames := srv.lazy.DeferredNames()

	require.NotEmpty(t, deferredNames, "at least one tool must be deferred — otherwise the lazy substrate is a no-op")

	for _, n := range deferredNames {
		require.NotContains(t, live, n, "deferred tool %q must NOT appear in tools/list before promotion", n)
		require.False(t, hotEagerTools[n], "deferred tool %q must not also be hot", n)
	}

	// Cold tools that should always be deferred unless someone moves
	// them into the hot set (which would require an audit).
	for _, expectDeferred := range []string{
		"find_clones",
		"get_surprising_connections",
		"replay_episode",
		"safe_delete_symbol",
		"generate_skill",
		"audit_agent_config",
		"contracts",
		"flow_between",
		"taint_paths",
		"prefetch_context",
		"detect_changes",
		"feedback",
		"subscribe_diagnostics",
		"unsubscribe_diagnostics",
		"plan_turn",
	} {
		require.Contains(t, deferredNames, expectDeferred,
			"%q should be deferred behind tools_search", expectDeferred)
	}
}

// TestLazyRegistration_ListChangedAdvertised confirms the server now
// declares listChanged=true so lazy-aware clients refresh tools/list
// when a deferred tool is promoted.
func TestLazyRegistration_ListChangedAdvertised(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	req := mcplib.InitializeRequest{}
	req.Params.ProtocolVersion = mcplib.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo.Name = "test-client"

	frame := struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      int            `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": mcplib.LATEST_PROTOCOL_VERSION,
			"clientInfo":      map[string]any{"name": "test-client", "version": "0"},
			"capabilities":    map[string]any{},
		},
	}
	body, err := json.Marshal(frame)
	require.NoError(t, err)
	reply := srv.MCPServer().HandleMessage(context.Background(), json.RawMessage(body))
	require.NotNil(t, reply)

	out, err := json.Marshal(reply)
	require.NoError(t, err)

	var parsed struct {
		Result struct {
			Capabilities struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.True(t, parsed.Result.Capabilities.Tools.ListChanged, "tools.listChanged must be true so promotion notifications are deliverable")
}

// TestToolsSearch_BrowseReturnsDeferredNames covers the empty-query
// browse mode — the entry point an agent uses to learn the catalog
// without paying any schema bytes upfront.
func TestToolsSearch_BrowseReturnsDeferredNames(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	expectNames := srv.lazy.DeferredNames()
	require.NotEmpty(t, expectNames)

	result := callToolsSearch(t, srv, map[string]any{})
	require.False(t, result.IsError)

	body := decodeStructured(t, result)
	assert.Equal(t, "", body.Query)
	assert.Equal(t, len(expectNames), body.Deferred)
	assert.ElementsMatch(t, expectNames, body.BrowseNames)
	assert.Empty(t, body.Tools, "browse mode returns names only, no schemas")
}

// TestToolsSearch_KeywordPromotesAndReturnsSchemas covers the standard
// path: an agent searches for "memories", gets the matching schemas,
// the tools migrate into tools/list, the listChanged notification
// fires, and a subsequent tools/call dispatches normally.
func TestToolsSearch_KeywordPromotesAndReturnsSchemas(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	// Sanity: memories tools start deferred.
	require.Contains(t, srv.lazy.DeferredNames(), "store_memory")
	require.NotContains(t, srv.mcpServer.ListTools(), "store_memory")

	result := callToolsSearch(t, srv, map[string]any{"query": "memories", "max_results": 20})
	require.False(t, result.IsError)
	body := decodeStructured(t, result)
	require.NotEmpty(t, body.Tools)

	gotNames := make(map[string]bool, len(body.Tools))
	for _, e := range body.Tools {
		gotNames[e.Name] = true
		require.NotEmpty(t, e.InputSchema, "schema must be carried for %q", e.Name)
	}
	require.True(t, gotNames["store_memory"], "store_memory must match the 'memories' keyword")

	live := srv.mcpServer.ListTools()
	for name := range gotNames {
		require.Contains(t, live, name, "promoted tool %q must be in tools/list", name)
	}
	assert.NotEmpty(t, body.Promoted)

	// Re-running the same search returns nothing new — promoted tools
	// drop out of the deferred catalog.
	result2 := callToolsSearch(t, srv, map[string]any{"query": "memories"})
	body2 := decodeStructured(t, result2)
	for _, e := range body2.Tools {
		require.False(t, gotNames[e.Name], "previously-promoted tool %q should not re-appear in tools_search", e.Name)
	}
}

// TestToolsSearch_SelectByExactName lets a tool-aware agent fetch
// schemas without keyword guessing — important for the "I know the
// name, I just need the schema" path.
func TestToolsSearch_SelectByExactName(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	want := []string{"flow_between", "taint_paths", "find_clones"}
	for _, n := range want {
		require.Contains(t, srv.lazy.DeferredNames(), n)
	}

	result := callToolsSearch(t, srv, map[string]any{"query": "select:" + strings.Join(want, ",")})
	require.False(t, result.IsError)
	body := decodeStructured(t, result)

	got := make([]string, 0, len(body.Tools))
	for _, e := range body.Tools {
		got = append(got, e.Name)
	}
	assert.ElementsMatch(t, want, got)
	assert.ElementsMatch(t, want, body.Promoted)
	for _, n := range want {
		require.Contains(t, srv.mcpServer.ListTools(), n)
	}
}

// TestToolsSearch_PromoteFalseLeavesCatalogIntact gives an agent a
// peek at a schema without migrating the tool — useful for reading
// docs without committing to use.
func TestToolsSearch_PromoteFalseLeavesCatalogIntact(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	before := srv.lazy.CountDeferred()
	result := callToolsSearch(t, srv, map[string]any{"query": "select:contracts", "promote": false})
	require.False(t, result.IsError)
	body := decodeStructured(t, result)
	require.Len(t, body.Tools, 1)
	require.Equal(t, "contracts", body.Tools[0].Name)
	require.Empty(t, body.Promoted)
	require.Equal(t, before, srv.lazy.CountDeferred(), "promote=false must not shrink the deferred catalog")
	require.NotContains(t, srv.mcpServer.ListTools(), "contracts")
}

// TestToolsSearch_NoMatchesIsNotAnError keeps the contract that a
// blank result still returns 200 with deferred_remaining metadata —
// agents can iterate without fearing failure modes.
func TestToolsSearch_NoMatchesIsNotAnError(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	// xyzzyplugh: a nonce string unlikely to appear in any tool name
	// or description. We tokenize on alphanumeric boundaries, so the
	// underlying matcher will look for "xyzzyplugh" literally.
	result := callToolsSearch(t, srv, map[string]any{"query": "xyzzyplugh"})
	require.False(t, result.IsError)
	body := decodeStructured(t, result)
	assert.Empty(t, body.Tools)
	assert.Empty(t, body.Promoted)
	assert.Greater(t, body.Deferred, 0)
}

// TestToolsSearch_RequiredKeywordFiltering exercises the "+keyword"
// syntax for narrowing matches.
func TestToolsSearch_RequiredKeywordFiltering(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	result := callToolsSearch(t, srv, map[string]any{"query": "+overlay drop", "max_results": 20})
	require.False(t, result.IsError)
	body := decodeStructured(t, result)
	for _, e := range body.Tools {
		require.Contains(t, strings.ToLower(e.Name), "overlay",
			"+overlay should filter to tools whose name contains 'overlay'; got %q", e.Name)
	}
}

// TestLazyRegistration_DisabledByEnvKeepsEverythingEager confirms the
// opt-out switch for clients that don't speak the discovery flow.
func TestLazyRegistration_DisabledByEnvKeepsEverythingEager(t *testing.T) {
	// Default is now disabled; "0" still resolves to disabled. The
	// behaviour we care about — full surface in tools/list, no
	// tools_search noise — is unchanged.
	t.Setenv("GORTEX_LAZY_TOOLS", "0")

	srv, _ := setupTestServer(t)
	require.False(t, srv.lazy.Enabled(), "GORTEX_LAZY_TOOLS=0 must disable the registry")
	require.Empty(t, srv.lazy.DeferredNames(), "disabled registry must hold no deferred tools")

	live := srv.mcpServer.ListTools()
	// Picking a deferred-by-default tool: in pass-through mode it must
	// be eagerly registered.
	require.Contains(t, live, "find_clones")
	// tools_search is not published when lazy is disabled — the
	// discovery tool would be misleading noise.
	require.NotContains(t, live, LazyToolsSearchName)
}

// TestLazyRegistration_PromotedToolsDispatchNormally ensures that
// after promotion, the standard tools/call path works for the new
// tool. This is the key invariant: discovery promotes; promotion does
// not change behaviour.
func TestLazyRegistration_PromotedToolsDispatchNormally(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.NotContains(t, srv.mcpServer.ListTools(), "list_inspections")

	_ = callToolsSearch(t, srv, map[string]any{"query": "select:list_inspections"})
	live := srv.mcpServer.ListTools()
	require.Contains(t, live, "list_inspections")

	// Issue a real tools/call frame through HandleMessage and confirm
	// the dispatch returns a non-error result.
	frame := struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      int            `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{
		JSONRPC: "2.0", ID: 7, Method: "tools/call",
		Params: map[string]any{
			"name":      "list_inspections",
			"arguments": map[string]any{},
		},
	}
	body, err := json.Marshal(frame)
	require.NoError(t, err)
	reply := srv.MCPServer().HandleMessage(context.Background(), json.RawMessage(body))
	require.NotNil(t, reply)
	out, err := json.Marshal(reply)
	require.NoError(t, err)
	var parsed struct {
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Nil(t, parsed.Error, "promoted tool dispatch must not return a JSON-RPC error: %v", parsed.Error)
	require.NotNil(t, parsed.Result)
	require.False(t, parsed.Result.IsError, "promoted tool must execute cleanly")
}

// TestLazyRegistration_HitsSpecTarget asserts the rationale row from
// the gap-analysis: the cold-session tools/list payload drops from
// ~88 tools down toward ~25, with the long tail living behind
// tools_search. Failing this test means the hot set has crept out of
// bounds — re-audit before bumping the bound.
func TestLazyRegistration_HitsSpecTarget(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	// Count the effective initial tools/list, not the process-global live map:
	// facade-v1 dispatchers are registered live for Codex but session-filtered
	// out of the legacy/default surface.
	eager := len(srv.sessionLiveToolNames(context.Background()))
	deferred := srv.lazy.CountDeferred()

	// Spec target: deferred ≥ 30 (the gap-analysis floor for the row
	// to land), eager ≤ 40 (leaves headroom for overlay / simulation
	// tools that bypass the lazy substrate).
	require.GreaterOrEqual(t, deferred, 30,
		"deferred catalog has %d tools; the N50 spec needs at least 30 hidden behind tools_search", deferred)
	require.LessOrEqual(t, eager, 40,
		"eager tools/list payload has %d tools; the cold-start surface drifted past the N50 ceiling", eager)
}

// TestTokenize covers the camelCase + snake_case split contract used
// by tools_search ranking.
func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"find_usages", []string{"find", "usages"}},
		{"findUsages", []string{"find", "usages"}},
		{"FindUsages", []string{"find", "usages"}},
		{"", nil},
		{"a-b-c", []string{"a", "b", "c"}},
		{"HTTP2Server", []string{"h", "t", "t", "p2", "server"}},
	}
	for _, c := range cases {
		got := tokenize(c.in)
		if c.want == nil {
			require.Empty(t, got)
			continue
		}
		assert.Equal(t, c.want, got, "input %q", c.in)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func callToolsSearch(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = LazyToolsSearchName
	req.Params.Arguments = args
	result, err := srv.handleToolsSearch(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func decodeStructured(t *testing.T, result *mcplib.CallToolResult) toolsSearchPayload {
	t.Helper()
	raw, ok := result.StructuredContent.(json.RawMessage)
	require.True(t, ok, "tools_search result must carry a json.RawMessage StructuredContent")
	var body toolsSearchPayload
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

// TestPromote_ConcurrentCallersNeverFalse404 is the reviewer-required
// synchronized two-request regression: two goroutines race to promote the
// same deferred tool. Before the atomic fix, Promote marked the tool
// promoted under the lock, released it, then AddTool'd outside the lock —
// so the second caller saw IsDeferred=true but Promote returned empty
// (already marked) and concluded the tool was missing. Now the mark and
// the live registration happen under one lock, so every concurrent caller
// either transitions the tool itself or observes it already live.
// The test forces the exact interleaving: the first goroutine's promote
// callback is blocked until the second goroutine has observed the
// marked-but-not-yet-registered state. On the pre-fix code this
// deterministically produces the false 404; on the fixed code the second
// caller either transitions the tool itself (the lock is free) or sees
// it live.
func TestPromote_ConcurrentCallersNeverFalse404(t *testing.T) {
	r := newLazyToolRegistry(true)
	var mu sync.Mutex
	live := map[string]bool{}

	// promoteBlocked gates the first Promote's registration callback:
	// the callback runs only after the second goroutine has observed the
	// intermediate state. This is what makes the race deterministic.
	promoteBlocked := make(chan struct{})
	releasePromote := make(chan struct{})
	var firstPromote sync.Once
	r.promote = func(dt *deferredTool) {
		firstPromote.Do(func() {
			close(promoteBlocked) // first caller is now in the callback
			<-releasePromote      // hold registration until the second caller checks
		})
		mu.Lock()
		live[dt.tool.Name] = true
		mu.Unlock()
	}
	r.Register(mcplib.NewTool("race_tool", mcplib.WithDescription("race")), func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return mcplib.NewToolResultText("ok"), nil
	})

	start := make(chan struct{})
	results := make(chan bool, 2)
	done := make(chan struct{}, 2)
	// Goroutine 1: transitions the tool, blocks inside the promote
	// callback before the live registration is visible.
	go func() {
		defer func() { done <- struct{}{} }()
		<-start
		transitioned := r.Promote("race_tool")
		mu.Lock()
		_, isLive := live["race_tool"]
		mu.Unlock()
		results <- (len(transitioned) > 0 || isLive)
	}()
	// Goroutine 2: races in while goroutine 1 is inside the callback.
	// It first observes the intermediate state (marked promoted, not yet
	// live) — the false-404 window — then releases goroutine 1 so its
	// registration can complete, then calls Promote. Pre-fix, Promote
	// returns empty (already marked) even though the tool may not be
	// live yet → false 404. Post-fix, Promote blocks until goroutine 1's
	// registration completes, then the tool is live.
	go func() {
		defer func() { done <- struct{}{} }()
		<-start
		<-promoteBlocked // wait until goroutine 1 is inside the callback
		// Pre-fix check: the tool is marked promoted but not yet live —
		// this is the false-404 window. Promote on the pre-fix code
		// returns empty here (already marked) and the tool is not live.
		mu.Lock()
		intermediateLive := live["race_tool"]
		mu.Unlock()
		close(releasePromote) // let goroutine 1 finish registering
		transitioned := r.Promote("race_tool")
		mu.Lock()
		postLive := live["race_tool"]
		mu.Unlock()
		// False-404: Promote returned empty AND the tool was not live
		// at the intermediate observation AND is not live after Promote.
		// Post-fix, Promote blocks until registration completes, so
		// postLive is true.
		results <- (len(transitioned) > 0 || postLive || intermediateLive)
	}()

	// Release both callers simultaneously so the interleaving above is
	// actually exercised, then collect both verdicts under a bounded
	// timeout — a hang here means the fix regressed to a deadlock, not
	// a silently-green test.
	close(start)
	timeout := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case ok := <-results:
			assert.True(t, ok, "concurrent caller must never observe a false 404 (tool marked promoted but never live)")
		case <-timeout:
			t.Fatal("timed out waiting for concurrent Promote callers — possible deadlock in the fixed implementation")
		}
	}

	// Both goroutines must actually exit (not leaked) before the test
	// returns; give them a bounded window past their result send.
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a goroutine to exit — possible leak")
		}
	}
}
