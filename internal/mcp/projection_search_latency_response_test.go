//go:build acceptance

package mcp

import (
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// These are response-contract controls, not latency samples. Start with the
// registered public handlers over an actual local ACK so the validator cannot
// demand false metadata that a complete text response does not emit.
func TestProjectionLatencyResponseRequiresCompleteActualJSON(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := ackModeServer(t, "local")
	line := searchProjectedAck(t, server, root, "CompleteLatencyWitness")
	for _, tool := range []string{"search_symbols", "search_text"} {
		t.Run(tool, func(t *testing.T) {
			probe := projectionLatencyProbe{Tool: tool, Query: "CompleteLatencyWitness", ID: "main.go::CompleteLatencyWitness", File: "main.go", Line: line}
			args := map[string]any{"query": probe.Query, "limit": 1000, "max_bytes": 0, "format": "json"}
			key, count := "results", "total"
			if tool == "search_symbols" {
				args["query_class"], args["expand"] = "symbol", "off"
			} else {
				key, count = "matches", "count"
				probe.Query = "func CompleteLatencyWitness("
				args["query"] = probe.Query
			}
			result := callTool(t, server, tool, args)
			require.False(t, result.IsError, "%v", result.Content)
			body := extractTextResult(t, result)
			require.NotEmpty(t, body[key])
			require.NoError(t, validateProjectionLatencyResult(probe, result), "actual complete public response must remain valid")
			if tool == "search_text" {
				_, present := body["truncated"]
				require.False(t, present, "complete text producer omits this flag; never require fabricated false metadata")
			} else {
				require.Equal(t, false, body["truncated"])
			}
			clone := func() map[string]any {
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				var copied map[string]any
				require.NoError(t, json.Unmarshal(raw, &copied))
				return copied
			}
			reply := func(value map[string]any) *mcplib.CallToolResult {
				raw, err := json.Marshal(value)
				require.NoError(t, err)
				return mcplib.NewToolResultText(string(raw))
			}
			for _, fault := range []struct {
				name   string
				change func(map[string]any)
			}{
				{"missing count", func(v map[string]any) { delete(v, count) }},
				{"null count", func(v map[string]any) { v[count] = nil }},
				{"string count", func(v map[string]any) { v[count] = "1" }},
				{"negative count", func(v map[string]any) { v[count] = -1 }},
				{"fractional count", func(v map[string]any) { v[count] = 1.5 }},
				{"lower count", func(v map[string]any) { v[count] = 0 }},
				{"larger count", func(v map[string]any) { v[count] = len(v[key].([]any)) + 1 }},
				{"missing collection", func(v map[string]any) { delete(v, key) }},
				{"wrong collection type", func(v map[string]any) { v[key] = map[string]any{} }},
				{"unreturned next page", func(v map[string]any) { v["next_cursor"] = "opaque-next-page" }},
				{"invalid cursor declaration", func(v map[string]any) { v["next_cursor"] = 1 }},
				{"inexact count", func(v map[string]any) { v["count_is_exact"] = false }},
				{"invalid exactness declaration", func(v map[string]any) { v["count_is_exact"] = "true" }},
			} {
				t.Run(fault.name, func(t *testing.T) {
					changed := clone()
					fault.change(changed)
					require.Error(t, validateProjectionLatencyResult(probe, reply(changed)), "an exact witness cannot hide missing or contradictory completeness evidence")
				})
			}
			for _, flag := range []string{"truncated", "_truncated_by_limit", "_truncated_by_budget", "_truncated_by_tokens"} {
				t.Run(flag, func(t *testing.T) {
					for _, value := range []any{true, "false"} {
						changed := clone()
						changed[flag] = value
						require.Error(t, validateProjectionLatencyResult(probe, reply(changed)), "disclosed or malformed cuts must remain failed samples")
					}
				})
			}
			t.Run("typed MCP refusal", func(t *testing.T) {
				refused := reply(clone())
				refused.IsError = true
				require.Error(t, validateProjectionLatencyResult(probe, refused))
			})
			t.Run("no substring error heuristics", func(t *testing.T) {
				complete := clone()
				complete["note"] = "budget and error words are ordinary content"
				require.NoError(t, validateProjectionLatencyResult(probe, reply(complete)))
			})
			if tool == "search_symbols" {
				t.Run("missing declared symbol completeness", func(t *testing.T) {
					changed := clone()
					delete(changed, "truncated")
					require.Error(t, validateProjectionLatencyResult(probe, reply(changed)))
				})
			}
		})
	}
}
