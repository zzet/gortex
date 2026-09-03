package mcp

import (
	"context"
	"strings"
	"testing"

	wire "github.com/gortexhq/gcx-go"
	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// The rider a routed request must be able to report, whatever wire format the
// caller reads it in.
var routedViewName = "worktree:" + viewTestWorktree

// riderListTools are the list-shaped read tools a routed session drives most:
// each one answers out of the view, so each one has to say which view answered.
var riderListTools = []struct {
	tool string
	args map[string]any
}{
	{"search_symbols", map[string]any{"query": "Keeper"}},
	{"find_usages", map[string]any{"id": searchKeeperID}},
	{"get_callers", map[string]any{"id": searchKeeperID}},
	{"get_dependencies", map[string]any{"id": searchKeeperID}},
}

// callRegisteredWithView drives one request through a tool's registered
// handler — the production wrapper chain with the real leaf handler, not the
// leaf stub callWithView installs — from a session that names the routed
// worktree.
func (v *viewStack) callRegisteredWithView(t *testing.T, tool string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	registered := v.srv.MCPServer().GetTool(tool)
	if registered == nil {
		t.Fatalf("tool %q is not registered", tool)
	}
	merged := map[string]any{}
	for name, value := range args {
		merged[name] = value
	}
	merged[viewArgName] = map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree}

	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = merged
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), v.repoRoot)
	res, err := registered.Handler(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s failed: %s", tool, viewResultText(t, res))
	}
	return res
}

// metaFreshness reads the rider off the response envelope, the carrier for a
// payload whose wire format has no structural home for one.
func metaFreshness(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	if res.Meta == nil {
		return nil
	}
	rider, _ := res.Meta.AdditionalFields["freshness"].(map[string]any)
	return rider
}

// gcxHeaderMeta decodes an encoded GCX payload's header meta.
func gcxHeaderMeta(t *testing.T, res *mcplib.CallToolResult) map[string]string {
	t.Helper()
	text := viewResultText(t, res)
	if !strings.HasPrefix(text, wire.Tag) {
		t.Fatalf("the response is not GCX: %.80s", text)
	}
	header, err := wire.NewDecoder(strings.NewReader(text)).Header()
	if err != nil {
		t.Fatalf("decode the GCX header: %v\n%.200s", err, text)
	}
	return header.Meta
}

// TestRoutedListToolsCarryTheViewRider holds every list-shaped read tool to the
// provenance search_symbols already reported in JSON: a caller reading through
// a routed view must be able to tell which view answered.
func TestRoutedListToolsCarryTheViewRider(t *testing.T) {
	for _, tc := range riderListTools {
		t.Run(tc.tool, func(t *testing.T) {
			stack := newSearchViewStack(t)
			res := stack.callRegisteredWithView(t, tc.tool, tc.args)
			rider := resultFreshness(t, res)
			if rider == nil {
				t.Fatalf("%s carries no view rider: %s", tc.tool, viewResultText(t, res))
			}
			if rider["actual_view"] != routedViewName {
				t.Errorf("actual_view = %v, want %q", rider["actual_view"], routedViewName)
			}
			if rider["requested_view"] != routedViewName {
				t.Errorf("requested_view = %v, want %q", rider["requested_view"], routedViewName)
			}
			if rider["exact"] != true {
				t.Errorf("exact = %v, want true", rider["exact"])
			}
		})
	}
}

// TestRoutedListToolsCarryTheViewRiderInGCX is the same guarantee in the wire
// format every known agent client actually gets: gcx is their session default,
// so a rider that only merges into a JSON object never reaches them.
func TestRoutedListToolsCarryTheViewRiderInGCX(t *testing.T) {
	for _, tc := range riderListTools {
		t.Run(tc.tool, func(t *testing.T) {
			stack := newSearchViewStack(t)
			args := map[string]any{"format": "gcx"}
			for name, value := range tc.args {
				args[name] = value
			}
			res := stack.callRegisteredWithView(t, tc.tool, args)
			meta := gcxHeaderMeta(t, res)
			if meta["actual_view"] != routedViewName {
				t.Errorf("header actual_view = %q, want %q", meta["actual_view"], routedViewName)
			}
			if meta["requested_view"] != routedViewName {
				t.Errorf("header requested_view = %q, want %q", meta["requested_view"], routedViewName)
			}
			if meta["exact"] != "true" {
				t.Errorf("header exact = %q, want \"true\"", meta["exact"])
			}
		})
	}
}

// TestRoutedCompactFormatsCarryTheViewRider covers the formats with nowhere in
// the payload to put a rider — TOON, the one-line text shape, a diagram. They
// carry it on the response envelope, so no routed answer is mistakable for a
// base one.
func TestRoutedCompactFormatsCarryTheViewRider(t *testing.T) {
	shapes := []struct {
		name string
		args map[string]any
	}{
		{"toon", map[string]any{"id": searchKeeperID, "format": "toon"}},
		{"text", map[string]any{"id": searchKeeperID, "compact": true}},
		{"mermaid", map[string]any{"id": searchKeeperID, "format": "mermaid"}},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			stack := newSearchViewStack(t)
			res := stack.callRegisteredWithView(t, "find_usages", shape.args)
			rider := metaFreshness(t, res)
			if rider == nil {
				t.Fatalf("the %s shape carries no view rider: %s", shape.name, viewResultText(t, res))
			}
			if rider["actual_view"] != routedViewName {
				t.Errorf("actual_view = %v, want %q", rider["actual_view"], routedViewName)
			}
		})
	}
}

// TestRoutedJSONRiderAlsoRidesTheEnvelope keeps the two channels in agreement:
// a JSON answer merges the rider into its body and still reports it on the
// envelope, so a client reading either one sees the same view.
func TestRoutedJSONRiderAlsoRidesTheEnvelope(t *testing.T) {
	stack := newSearchViewStack(t)
	res := stack.callRegisteredWithView(t, "find_usages", map[string]any{"id": searchKeeperID})
	body := resultFreshness(t, res)
	envelope := metaFreshness(t, res)
	if body == nil || envelope == nil {
		t.Fatalf("body rider = %v, envelope rider = %v — both are required", body, envelope)
	}
	if body["actual_view"] != envelope["actual_view"] {
		t.Errorf("body actual_view = %v, envelope actual_view = %v", body["actual_view"], envelope["actual_view"])
	}
}
