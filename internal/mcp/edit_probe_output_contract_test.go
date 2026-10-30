package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"
)

// Exercise the actual public edit and seven probe producers. TOON is the text
// contract, not permission to consume a structured JSON sibling or invent the
// metadata that its compact node rows deliberately omit.
func TestEditProbeActualJSONAndTOONWireContracts(t *testing.T) {
	for _, format := range []string{"json", "toon"} {
		t.Run(format, func(t *testing.T) {
			original := "package main\n\nfunc Example() int { return 1 }\nfunc Caller() int { return Example() }\n"
			s, root := setupMoveInlineRepo(t, map[string]string{"main.go": original})
			t.Cleanup(s.DrainBackground)
			s.ToolCallTimeout = -1
			const session = "edit-output-contract"
			s.NoteSessionToolPolicy(session, "facade-v1", "hide")
			ctx := WithSessionCWD(WithSessionID(t.Context(), session), root)
			output := map[string]any{"format": format, "max_bytes": 0}
			ack := callEditOperationFrame(t, s, ctx, "edit", map[string]any{"operation": "file", "target": map[string]any{"file": "main.go"}, "match": "func Caller() int { return Example() }", "replacement": "func Caller() int { return Example() }\nfunc Added() int { return Example() }", "options": map[string]any{"mutation_id": "output-" + format}, "output": output})
			decode := func(result any) map[string]any {
				raw, err := json.Marshal(result)
				require.NoError(t, err)
				var envelope struct {
					IsError    bool `json:"isError"`
					Structured any  `json:"structuredContent"`
					Content    []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				}
				require.NoError(t, json.Unmarshal(raw, &envelope))
				require.False(t, envelope.IsError, "%s", raw)
				require.Len(t, envelope.Content, 1)
				require.Equal(t, "text", envelope.Content[0].Type)
				text := envelope.Content[0].Text
				var body map[string]any
				if format == "json" {
					require.NoError(t, json.Unmarshal([]byte(text), &body))
				} else {
					require.Nil(t, envelope.Structured, "TOON text must be the evaluated wire")
					require.False(t, json.Valid([]byte(text)), "requested TOON silently fell back to JSON")
					decoded, err := toon.Decode([]byte(text), toon.WithStrictMode(true))
					require.NoError(t, err)
					var ok bool
					body, ok = decoded.(map[string]any)
					require.True(t, ok)
				}
				return body
			}
			require.NotEmpty(t, decode(ack)["mutation_receipt"], "source ACK must survive the selected output format")
			calls := []struct {
				name, operation string
				target          map[string]any
				query           string
			}{
				{"read", "source", map[string]any{"symbol": "main.go::Added"}, ""},
				{"read", "summary", map[string]any{"file": "main.go"}, ""},
				{"read", "editing_context", map[string]any{"file": "main.go"}, ""},
				{"search", "symbols", nil, "Added"},
				{"search", "text", nil, "Added"},
				{"relations", "callers", map[string]any{"symbol": "main.go::Example"}, ""},
				{"trace", "call_chain", map[string]any{"symbol": "main.go::Added"}, ""},
			}
			for _, call := range calls {
				args := map[string]any{"operation": call.operation, "output": output, "options": map[string]any{"limit": 1000, "max_per_file": 0, "expand": "off", "depth": 1, "context_lines": 0}}
				if call.target != nil {
					args["target"] = call.target
				}
				if call.query != "" {
					args["query"] = call.query
				}
				body := decode(callEditOperationFrame(t, s, ctx, call.name, args))
				switch call.operation {
				case "source":
					require.Equal(t, "main.go::Added", body["id"])
					require.Contains(t, body["source"], "return Example()")
				case "summary", "callers", "call_chain":
					require.Equal(t, false, body["truncated"])
					require.Contains(t, body, "nodes")
					require.Contains(t, body, "edges")
					if format == "toon" {
						require.Contains(t, body, "total")
						require.NotContains(t, body, "total_nodes", "TOON does not promise a JSON total_nodes field")
						require.NotContains(t, body, "total_edges", "complete TOON edge rows omit this optional full-count field")
						for _, raw := range body["nodes"].([]any) {
							node := raw.(map[string]any)
							require.NotContains(t, node, "meta", "compact rows do not expose signature metadata")
							require.Contains(t, node, "id")
							require.Contains(t, node, "file_path")
							require.Contains(t, node, "start_line")
						}
					} else {
						require.Contains(t, body, "total_nodes")
						require.Contains(t, body, "total_edges")
					}
				case "editing_context":
					require.NotEmpty(t, body["defines"])
				case "symbols":
					require.NotEmpty(t, body["results"])
					require.Contains(t, body, "total")
				case "text":
					require.NotEmpty(t, body["matches"])
				}
			}
		})
	}
}
