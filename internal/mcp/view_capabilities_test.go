package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reach"
)

// declareProducer states one producer's contribution for an already published
// generation.
//
// Publishing seals the payload, so the store's own writer refuses the row —
// a real build declares its producers before it publishes. The completeness
// union reads the table directly, so a second connection is how a test says
// what a build declared without unsealing anything.
func (v *viewStack) declareProducer(
	t *testing.T,
	generation int64,
	id graphview.CapabilityID,
	state store_sqlite.ProducerState,
) {
	t.Helper()
	raw, err := sql.Open("sqlite", v.dbPath)
	if err != nil {
		t.Fatalf("open the store for a producer row: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(
		`INSERT OR REPLACE INTO generation_producer_completeness (view_gen, producer, state, reason)
		 VALUES (?, ?, ?, '')`,
		generation, string(id), string(state)); err != nil {
		t.Fatalf("declare %s as %s: %v", id, state, err)
	}
}

// callHandler drives one request through the whole middleware into a real
// handler, so a leaf that reads its own arguments sees them.
func (v *viewStack) callHandler(
	t *testing.T,
	cwd, tool string,
	args map[string]any,
	handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error),
) (*mcplib.CallToolResult, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd)
	return v.srv.wrapToolHandler(handler)(ctx, req)
}

// stubLeaf is a handler that does nothing but answer, for the cases where the
// gate must refuse before any handler runs.
func stubLeaf(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	return mcplib.NewToolResultText(`{"ok":true}`), nil
}

func TestCapabilityDefaultsFollowTheToolFamily(t *testing.T) {
	cases := []struct {
		tool string
		want []graphview.CapabilityID
	}{
		// Search, navigation and reads take their family's defaults.
		{"search_symbols", searchCapabilities},
		{"find_usages", navigationCapabilities},
		{"get_callers", navigationCapabilities},
		{"read_file", readCapabilities},
		{"get_symbol_source", readCapabilities},
		{"get_communities", analysisCapabilities},
		// Overrides: the family is the wrong answer for these.
		{"search_text", []graphview.CapabilityID{graphview.CapSearchText}},
		{"find_clones", []graphview.CapabilityID{graphview.CapSyntaxGraph, graphview.CapSimilarity}},
		{"get_code_actions", []graphview.CapabilityID{graphview.CapLSPCodeActions}},
		// A tool that reads no view content requires nothing.
		{"save_note", nil},
		{"index_repository", nil},
	}
	for _, tc := range cases {
		got := capabilityDefaultsFor(tc.tool)
		if len(got) != len(tc.want) {
			t.Errorf("%s defaults = %v, want %v", tc.tool, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s defaults = %v, want %v", tc.tool, got, tc.want)
				break
			}
		}
	}
}

// A facade call names its operation, not the handler behind it, so the
// capability lookup has to resolve the operation first — otherwise every
// operation of one facade would inherit the same requirement.
func TestFacadeOperationResolvesToItsHandlersCapabilities(t *testing.T) {
	stack := newViewStack(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "change"
	req.Params.Arguments = map[string]any{"operation": "diagnostics"}
	if got := stack.srv.capabilityToolName(&req); got != "get_diagnostics" {
		t.Fatalf("change(operation=diagnostics) resolved to %q, want get_diagnostics", got)
	}
	defaults := capabilityDefaultsFor(stack.srv.capabilityToolName(&req))
	if len(defaults) != 1 || defaults[0] != graphview.CapLSPDiagnostics {
		t.Errorf("defaults = %v, want %v", defaults, graphview.CapLSPDiagnostics)
	}
}

// A search under a view whose content search is partial must refuse and name
// the capability that is partial — and only that one.
func TestSearchRefusesAViewWithPartialContentSearch(t *testing.T) {
	stack := newViewStack(t)
	stack.declareProducer(t, stack.dirty, graphview.CapSearchContent, store_sqlite.ProducerStateIncomplete)

	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols",
		map[string]any{requireCompleteArgName: true}, captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeRequiredCapabilityIncomplete)
	text := viewResultText(t, res)
	if !strings.Contains(text, string(graphview.CapSearchContent)) {
		t.Errorf("the refusal does not name the failing capability:\n%s", text)
	}
	if strings.Contains(text, string(graphview.CapSearchSymbols)) {
		t.Errorf("the refusal names a capability the view serves:\n%s", text)
	}
}

// The structurally complete view is the ordinary case: every capability the
// operation needs is served, so require_complete changes nothing about the
// answer and adds nothing to the rider.
func TestRequireCompleteOnACompleteViewSucceeds(t *testing.T) {
	stack := newViewStack(t)
	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols",
		map[string]any{requireCompleteArgName: true}, captureReader(stack.srv, &reader))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("a complete view refused a require_complete search: %s", viewResultText(t, res))
	}
	if !hasNode(reader, "repo/added.go::Fresh") {
		t.Error("the request did not read through the routed view")
	}
	rider := resultFreshness(t, res)
	if _, present := rider["degraded_capabilities"]; present {
		t.Errorf("a complete view reported a degraded capability: %v", rider)
	}
}

// An optional capability never fails the request; it rides back with the
// state it was found in so a thin answer is legible as one.
func TestOptionalCapabilitiesAnnotateWithoutFailing(t *testing.T) {
	stack := newViewStack(t)
	stack.declareProducer(t, stack.dirty, graphview.CapSimilarity, store_sqlite.ProducerStateIncomplete)

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
		map[string]any{optionalCapabilitiesArgName: []any{string(graphview.CapSimilarity)}},
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("an optional capability failed the request: %s", viewResultText(t, res))
	}
	rider := resultFreshness(t, res)
	rows, _ := rider["degraded_capabilities"].([]any)
	if len(rows) != 1 {
		t.Fatalf("degraded_capabilities = %v, want one entry", rider["degraded_capabilities"])
	}
	row, _ := rows[0].(map[string]any)
	if row["capability"] != string(graphview.CapSimilarity) || row["state"] != string(graphview.StateIncomplete) {
		t.Errorf("degraded entry = %v, want %s (%s)", row, graphview.CapSimilarity, graphview.StateIncomplete)
	}
}

// A capability name outside the vocabulary is a typo the caller must see: a
// silently ignored requirement would read as a requirement that was met.
func TestUnknownCapabilityNameIsRejected(t *testing.T) {
	stack := newViewStack(t)
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol",
		map[string]any{requiredCapabilitiesArgName: []any{"graph.nonsense"}},
		captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeInvalidViewSelector)
	if text := viewResultText(t, res); !strings.Contains(text, "graph.nonsense") {
		t.Errorf("the refusal does not name the unknown capability:\n%s", text)
	}
}

// The base corpus is assumed complete this round: it carries no producer rows
// to evaluate, so a base request must answer exactly as it did before any of
// this existed — including a request that declares a requirement.
func TestBaseRequestsCarryNoCapabilityRider(t *testing.T) {
	stack := newViewStack(t)
	args := map[string]any{
		"source_id":            "repo/edit.go::Old",
		"sink_id":              "repo/keep.go::Keeper",
		requireCompleteArgName: true,
	}
	res, err := stack.callHandler(t, stack.repoRoot, "flow_between", args, stack.srv.handleFlowBetween)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("a base request was refused: %s", viewResultText(t, res))
	}
	if rider := resultFreshness(t, res); rider != nil {
		t.Fatalf("a base answer carries a view rider: %v", rider)
	}
	text := viewResultText(t, res)
	for _, field := range []string{"base_scoped", "degraded_capabilities"} {
		if strings.Contains(text, field) {
			t.Errorf("a base answer carries %s:\n%s", field, text)
		}
	}
}

// flow_between builds its traversal state over the base corpus, so under a
// routed view the paths describe the base and the response has to say so.
func TestBaseScopedEngineAnnotatesOnlyUnderAView(t *testing.T) {
	stack := newViewStack(t)
	args := func() map[string]any {
		return map[string]any{"source_id": "repo/edit.go::New", "sink_id": "repo/keep.go::Dirty"}
	}

	res, err := stack.callHandler(t, stack.worktreeRoot, "flow_between", args(), stack.srv.handleFlowBetween)
	if err != nil {
		t.Fatalf("call under the view: %v", err)
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatalf("a routed answer carries no rider: %s", viewResultText(t, res))
	}
	named := map[string]bool{}
	for _, entry := range rider["base_scoped"].([]any) {
		named[entry.(string)] = true
	}
	for _, want := range []graphview.CapabilityID{graphview.CapSyntaxGraph, graphview.CapResolutionLocal} {
		if !named[string(want)] {
			t.Errorf("base_scoped = %v, want it to name %s", rider["base_scoped"], want)
		}
	}

	// The control: the same engine on a base request is not a base-scoped
	// answer, it IS the answer.
	plain, err := stack.callHandler(t, stack.repoRoot, "flow_between", args(), stack.srv.handleFlowBetween)
	if err != nil {
		t.Fatalf("call on the base: %v", err)
	}
	if strings.Contains(viewResultText(t, plain), "base_scoped") {
		t.Errorf("a base answer was annotated as base-scoped:\n%s", viewResultText(t, plain))
	}
}

// A view of committed state runs no language server, so an operation that
// needs one cannot be served by waiting — it is refused as unavailable, not
// as incomplete.
func TestRefViewRefusesAnLSPRequirement(t *testing.T) {
	stack := newRefStack(t)
	res, err := stack.call(t, "get_diagnostics", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"path": "repo/edit.go", requireCompleteArgName: true}, stubLeaf)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertToolError(t, res, graphview.CodeCapabilityUnavailable)
	if text := viewResultText(t, res); !strings.Contains(text, string(graphview.CapLSPDiagnostics)) {
		t.Errorf("the refusal does not name the language-server capability:\n%s", text)
	}
}

// The withdrawal case: a capability a view served can stop being servable.
// The operations that need it fail; the ones that do not keep answering.
func TestWithdrawnSourceSnapshotFailsReadsNotSearches(t *testing.T) {
	stack := newViewStack(t)
	stack.declareProducer(t, stack.dirty, graphview.CapSourceSnapshot, store_sqlite.ProducerStateUnavailable)

	read, err := stack.callHandler(t, stack.worktreeRoot, "read_file",
		map[string]any{"path": "repo/keep.go", requireCompleteArgName: true}, stubLeaf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	assertToolError(t, read, graphview.CodeCapabilityUnavailable)
	if text := viewResultText(t, read); !strings.Contains(text, string(graphview.CapSourceSnapshot)) {
		t.Errorf("the refusal does not name the withdrawn capability:\n%s", text)
	}

	search, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols",
		map[string]any{requireCompleteArgName: true}, captureReader(stack.srv, new(graph.Reader)))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if search.IsError {
		t.Fatalf("a search that never reads file bytes was refused: %s", viewResultText(t, search))
	}
}

// impactRow runs the repo-wide impact ranking and returns the row it scored
// for id. The session names no working directory, so the ranking's candidate
// set is the whole reader and the two calls differ only in which reader
// answers: the base corpus, or the view the selector names.
func (v *viewStack) impactRow(t *testing.T, view map[string]any, id string) map[string]any {
	t.Helper()
	args := map[string]any{"refresh_cochange": false}
	if view != nil {
		args[viewArgName] = view
	}
	res, err := v.callHandler(t, "", "analyze", args, v.srv.handleAnalyzeImpactComposite)
	if err != nil {
		t.Fatalf("analyze impact: %v", err)
	}
	if res.IsError {
		t.Fatalf("analyze impact: %s", viewResultText(t, res))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &payload); err != nil {
		t.Fatalf("unmarshal the impact response: %v", err)
	}
	rows, _ := payload["symbols"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("the ranking scored no row for %s: %s", id, viewResultText(t, res))
	return nil
}

// The impact ranking blends an already-published transitive reach index into
// its reach axis. Those records are built over the base corpus and stamped on
// base node metadata, so under a routed view they describe a corpus the caller
// did not ask about — the ranking has to fall back to the view's own depth-1
// fan-in, which is what it does whenever nothing has been published at all.
func TestImpactReachIndexIsNotBlendedIntoAViewRanking(t *testing.T) {
	stack := newViewStack(t)
	// Two callers chained into Keeper, both in the file the commit
	// generation replaces: the published record therefore reaches further
	// than the direct fan-in, and neither caller nor call edge survives into
	// the view.
	stack.store.AddBatch([]*graph.Node{
		viewRepoNode("repo/edit.go::CallerInner", "CallerInner", graph.KindFunction, "repo/edit.go", 6),
		viewRepoNode("repo/edit.go::CallerOuter", "CallerOuter", graph.KindFunction, "repo/edit.go", 9),
	}, []*graph.Edge{
		{
			From: "repo/edit.go::CallerInner", To: "repo/keep.go::Keeper",
			Kind: graph.EdgeCalls, FilePath: "repo/edit.go", Line: 7,
		},
		{
			From: "repo/edit.go::CallerOuter", To: "repo/edit.go::CallerInner",
			Kind: graph.EdgeCalls, FilePath: "repo/edit.go", Line: 10,
		},
	})
	reach.BuildIndex(stack.store)

	// The control, and the proof the fixture is not vacuous: on the base the
	// record is consumed and carries the ranking past the direct fan-in.
	base := stack.impactRow(t, nil, "repo/keep.go::Keeper")
	baseReach, baseFanIn := base["reach_count"], base["fan_in"]
	if toFloat(t, baseReach) <= toFloat(t, baseFanIn) {
		t.Fatalf("the base ranking consumed no reach record: reach_count=%v fan_in=%v", baseReach, baseFanIn)
	}

	view := stack.impactRow(t,
		map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
		"repo/keep.go::Keeper")
	viewReach, viewFanIn := view["reach_count"], view["fan_in"]
	if toFloat(t, viewReach) != toFloat(t, viewFanIn) {
		t.Errorf("the routed ranking blended base reach records in: reach_count=%v, view fan_in=%v",
			viewReach, viewFanIn)
	}
}

// Three of the architecture snapshot's sections — communities, hotspots and
// processes — are served out of the server-wide analysis caches rather than
// through the request's reader. Under a view they describe the base, and a
// snapshot that did not say so would read as a wholly view-scoped answer.
func TestArchitectureAnnotatesItsCacheFedSections(t *testing.T) {
	stack := newViewStack(t)
	view := map[string]any{
		viewArgName: map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree},
	}
	res, err := stack.callHandler(t, "", "get_architecture", view, stack.srv.handleGetArchitecture)
	if err != nil {
		t.Fatalf("call under the view: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_architecture under a view: %s", viewResultText(t, res))
	}
	rider := resultFreshness(t, res)
	if rider == nil {
		t.Fatalf("a routed answer carries no rider: %s", viewResultText(t, res))
	}
	named := map[string]bool{}
	entries, _ := rider["base_scoped"].([]any)
	for _, entry := range entries {
		name, _ := entry.(string)
		named[name] = true
	}
	if !named[string(graphview.CapSyntaxGraph)] {
		t.Errorf("base_scoped = %v, want it to name %s", rider["base_scoped"], graphview.CapSyntaxGraph)
	}

	// The control: on a base request the caches ARE the answer.
	plain, err := stack.callHandler(t, "", "get_architecture", nil, stack.srv.handleGetArchitecture)
	if err != nil {
		t.Fatalf("call on the base: %v", err)
	}
	if strings.Contains(viewResultText(t, plain), "base_scoped") {
		t.Errorf("a base answer was annotated as base-scoped:\n%s", viewResultText(t, plain))
	}
}

// toFloat reads one numeric field out of a decoded response row.
func toFloat(t *testing.T, raw any) float64 {
	t.Helper()
	value, ok := raw.(float64)
	if !ok {
		t.Fatalf("expected a number, got %#v", raw)
	}
	return value
}
