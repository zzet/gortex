package mcp

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// Compact text has no freshness field: it carries no graph_freshness key and
// no edge identity, so a probe cannot score a stale-claimed-fresh answer in
// it. Its silent-stale count is reported unobservable rather than zero. That
// report holds only while two product facts hold, and these witnesses pin
// both on the registered tool handlers, through the same facade requests the
// edit-and-probe harness sends:
//
//   - no compact answer ever claims a tier is fresh, even on a request and a
//     graph state for which the JSON twin does claim it;
//   - while relations in a compact answer's scope are pending, the answer
//     says so on its first line, so compact is never a silent answer.

// compactTierLine matches any in-band tier label the shared renderer writes.
var compactTierLine = regexp.MustCompile(`^(file|text|declarations|relations|derived): [a-z_]+ \([0-9]+ files\)$`)

// compactPendingFirstLine is the only label a pending compact answer may lead with.
var compactPendingFirstLine = regexp.MustCompile(`^relations: pending \([1-9][0-9]* files\)$`)

type compactWitnessCall struct {
	facade string
	args   map[string]any
	// explicit marks operations whose JSON twin carries the explicit tier
	// answer under require_fresh, so the compact twin is asked the same
	// question the JSON one answers with "fresh".
	explicit bool
	// relations marks operations whose answer scope is the edited symbol's
	// relations, so a pending receipt on its file is in scope.
	relations bool
}

func compactWitnessCalls() map[string]compactWitnessCall {
	target := map[string]any{"symbol": "repo/target.go::Target"}
	return map[string]compactWitnessCall{
		"read.summary": {facade: "read", args: map[string]any{"operation": "summary", "target": map[string]any{"file": "repo/target.go"}}, relations: true, explicit: true},
		"search.symbols": {facade: "search", args: map[string]any{"operation": "symbols", "query": "Target",
			"options": map[string]any{"query_class": "symbol", "expand": "off", "limit": 1000, "max_per_file": 0}}, explicit: true},
		"relations.callers": {facade: "relations", args: map[string]any{"operation": "callers", "target": target,
			"options": map[string]any{"depth": 1, "limit": 1000}}, explicit: true, relations: true},
		"trace.call_chain": {facade: "trace", args: map[string]any{"operation": "call_chain", "target": target,
			"options": map[string]any{"depth": 1, "limit": 1000}}, explicit: true, relations: true},
		"change.verify": {facade: "change", args: map[string]any{"operation": "verify",
			"source": map[string]any{"changes": []map[string]any{{"symbol_id": "repo/target.go::Target", "new_signature": "func Target(x int)"}}}},
			explicit: true, relations: true},
		"analyze.dead_code": {facade: "analyze", args: map[string]any{"kind": "dead_code", "options": map[string]any{"limit": 50}}},
	}
}

// compactWitnessRequest copies the call's arguments and sets the output the
// harness sends: compact text, or plain JSON for the control twin.
func compactWitnessRequest(call compactWitnessCall, compact, requireFresh bool) map[string]any {
	args := make(map[string]any, len(call.args)+1)
	for key, value := range call.args {
		args[key] = value
	}
	options := map[string]any{}
	if existing, ok := args["options"].(map[string]any); ok {
		for key, value := range existing {
			options[key] = value
		}
	}
	if requireFresh {
		options["require_fresh"] = true
	}
	if len(options) > 0 {
		args["options"] = options
	}
	output := map[string]any{"format": "json"}
	if compact {
		output["compact"], output["max_bytes"] = true, 0
	}
	args["output"] = output
	return args
}

// compactWitnessPayload is the answer's first text content: the payload a
// client reads, without the auxiliary session notes that may follow it.
func compactWitnessPayload(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "first content is not text: %#v", res.Content[0])
	return text.Text
}

// requireNoCompactFreshClaim fails when a compact payload states any tier is
// fresh, in a label line or as a JSON-style field.
func requireNoCompactFreshClaim(t *testing.T, text string) {
	t.Helper()
	require.NotContains(t, text, "graph_freshness", "compact text has no freshness field: %q", text)
	for _, line := range strings.Split(text, "\n") {
		if compactTierLine.MatchString(line) {
			require.NotContains(t, line, ": fresh (", "compact answer claims a fresh tier: %q", text)
		}
	}
}

func TestCompactAnswersNeverClaimFreshness(t *testing.T) {
	for name, call := range compactWitnessCalls() {
		t.Run(name, func(t *testing.T) {
			// The harness talks to the facade surface; register it as a client would.
			t.Setenv("GORTEX_TOOLS", "facade-v1")
			s, _, _ := explicitFreshnessFixture(t)
			if call.explicit {
				// Control: the same request in JSON on the same graph state is
				// answered with the explicit fresh tier, so the compact twin
				// below is asked a question that has a fresh answer.
				control := callToolByName(t, s, t.Context(), call.facade, compactWitnessRequest(call, false, true))
				require.False(t, control.IsError, compactWitnessPayload(t, control))
				fields, ok := resultJSON(t, control)["freshness"].(map[string]any)
				require.True(t, ok, "JSON control carries no freshness: %s", compactWitnessPayload(t, control))
				require.Equal(t, "fresh", fields["graph_freshness"], compactWitnessPayload(t, control))
			}
			for _, requireFresh := range []bool{false, true} {
				res := callToolByName(t, s, t.Context(), call.facade, compactWitnessRequest(call, true, requireFresh))
				text := compactWitnessPayload(t, res)
				require.False(t, res.IsError, text)
				requireNoCompactFreshClaim(t, text)
				// Nothing is pending, so compact has nothing to disclose: an
				// in-band label here could only be a tier claim compact has no
				// contract for.
				first, _, _ := strings.Cut(text, "\n")
				require.False(t, compactTierLine.MatchString(first), "idle compact answer carries a tier label (require_fresh=%v): %q", requireFresh, text)
			}
		})
	}
}

func TestCompactAnswersLeadWithPendingWhileRelationsArePending(t *testing.T) {
	for name, call := range compactWitnessCalls() {
		if !call.relations {
			continue
		}
		t.Run(name, func(t *testing.T) {
			// The harness talks to the facade surface; register it as a client would.
			t.Setenv("GORTEX_TOOLS", "facade-v1")
			s, _, _ := explicitFreshnessFixture(t)
			// The edit is acknowledged and its file graph published; its
			// relations are not. Relations-scoped answers are therefore pending.
			watcher := s.currentWatcher().(*explicitTierWatcher)
			watcher.mu.Lock()
			watcher.paths = map[string]indexer.MutationTierState{
				s.multiIndexer.ResolveFilePath("repo/target.go"): {Generation: 1, AckAt: time.Now(), FileGraphAt: time.Now()},
			}
			watcher.mu.Unlock()
			// Control: the JSON twin labels the same state pending.
			control := callToolByName(t, s, t.Context(), call.facade, compactWitnessRequest(call, false, false))
			require.False(t, control.IsError, compactWitnessPayload(t, control))
			fields, ok := resultJSON(t, control)["freshness"].(map[string]any)
			require.True(t, ok, "JSON control carries no freshness: %s", compactWitnessPayload(t, control))
			require.Equal(t, "pending", fields["graph_freshness"], compactWitnessPayload(t, control))

			res := callToolByName(t, s, t.Context(), call.facade, compactWitnessRequest(call, true, false))
			text := compactWitnessPayload(t, res)
			require.False(t, res.IsError, text)
			first, _, _ := strings.Cut(text, "\n")
			require.True(t, compactPendingFirstLine.MatchString(first), "pending compact answer is silent: %q", text)
			requireNoCompactFreshClaim(t, text)
		})
	}
}
