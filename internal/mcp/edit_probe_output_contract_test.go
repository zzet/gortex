package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
			logCore, captured := observer.New(zap.DebugLevel)
			s.logger = zap.New(logCore)
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
				if envelope.IsError {
					for _, entry := range captured.All() {
						if entry.Message != "mcp: handler panic recovered" && entry.Message != "tool handler panic recovered" {
							continue
						}
						t.Logf("actual handler panic stack: %v", entry.ContextMap()["stack"])
					}
				}
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
					if format == "toon" && call.operation != "summary" {
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
						if call.operation == "summary" {
							for _, raw := range body["nodes"].([]any) {
								node := raw.(map[string]any)
								require.Contains(t, node, "end_line")
								require.Contains(t, node, "meta")
							}
						}
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

// This pins a possible visible TOON witness before enabling the signature
// adapter: the unchanged following caller and target share the same graph
// response, so upstream line shifts cancel and no end_line/signature is made up.
func TestEditProbeTOONSignatureFollowingCallerWireWitness(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			file, header := "main.go", "package main\n\n"
			example, before := "func Example() int { return 1 }", "func Target() int { return Example() }"
			after, following := "func Target(optional ...int) int {\n return Example()\n}", "func Following() int { return Target() }"
			if language == "typescript" {
				file, header = "main.ts", ""
				example, before = "export function Example(): number { return 1; }", "export function Target(): number { return Example(); }"
				after, following = "export function Target(optional?: number): number {\n return Example();\n}", "export function Following(): number { return Target(); }"
			}
			if language == "python" {
				file, header = "main.py", ""
				example, before = "def Example():\n    return 1", "def Target():\n    return Example()"
				after, following = "def Target(optional=0):\n\n    return Example()", "def Following():\n    return Target()"
			}
			source := header + example + "\n\n" + before + "\n\n" + following + "\n"
			s, root := setupMoveInlineRepo(t, map[string]string{file: source})
			t.Cleanup(s.DrainBackground)
			s.ToolCallTimeout = -1
			logCore, captured := observer.New(zap.DebugLevel)
			s.logger = zap.New(logCore)
			const session = "toon-signature-witness"
			s.NoteSessionToolPolicy(session, "facade-v1", "hide")
			ctx := WithSessionCWD(WithSessionID(t.Context(), session), root)
			output := map[string]any{"format": "toon", "max_bytes": 0}
			decode := func(result *mcpgo.CallToolResult) map[string]any {
				if result.IsError {
					for _, entry := range captured.All() {
						if entry.Message != "mcp: handler panic recovered" && entry.Message != "tool handler panic recovered" {
							continue
						}
						t.Logf("actual handler panic stack: %v", entry.ContextMap()["stack"])
					}
				}
				require.False(t, result.IsError)
				text := toolText(result)
				require.False(t, json.Valid([]byte(text)), "requested TOON fell back to JSON")
				decoded, err := toon.Decode([]byte(text), toon.WithStrictMode(true))
				require.NoError(t, err)
				body, ok := decoded.(map[string]any)
				require.True(t, ok)
				return body
			}
			summaryArgs := map[string]any{"operation": "summary", "target": map[string]any{"file": file}, "output": output}
			gap := func(body map[string]any, compact bool) int {
				positions := map[string]int{}
				for _, raw := range body["nodes"].([]any) {
					node := raw.(map[string]any)
					if compact {
						require.NotContains(t, node, "end_line")
						require.NotContains(t, node, "meta")
					} else {
						require.Contains(t, node, "end_line")
						require.Contains(t, node, "meta")
					}
					id, _ := node["id"].(string)
					if id == file+"::Target" || id == file+"::Following" {
						number, ok := node["start_line"].(float64)
						require.True(t, ok)
						require.NotContains(t, positions, id, "duplicate scoped fixture node")
						positions[id] = int(number)
					}
				}
				require.Len(t, positions, 2)
				return positions[file+"::Following"] - positions[file+"::Target"]
			}
			require.Equal(t, strings.Count(before, "\n")+2, gap(decode(callEditOperationFrame(t, s, ctx, "read", summaryArgs)), false))
			ack := decode(callEditOperationFrame(t, s, ctx, "edit", map[string]any{"operation": "symbol", "target": map[string]any{"symbol": file + "::Target"}, "match": before, "replacement": after, "output": output}))
			require.NotEmpty(t, ack["mutation_receipt"])
			for _, call := range []struct {
				name, operation, target string
				depth                   int
			}{{"read", "summary", file, 1}, {"relations", "callers", file + "::Target", 1}, {"trace", "call_chain", file + "::Following", 2}} {
				target := map[string]any{"symbol": call.target}
				if call.operation == "summary" {
					target = map[string]any{"file": call.target}
				}
				body := decode(callEditOperationFrame(t, s, ctx, call.name, map[string]any{"operation": call.operation, "target": target, "options": map[string]any{"depth": call.depth, "limit": 1000}, "output": output}))
				require.Equal(t, false, body["truncated"])
				require.Equal(t, strings.Count(after, "\n")+2, gap(body, call.operation != "summary"))
				if call.operation == "summary" {
					continue
				}
				incoming, outgoing := false, false
				for _, raw := range body["edges"].([]any) {
					edge := raw.(map[string]any)
					if edge["kind"] != "calls" {
						continue
					}
					incoming = incoming || edge["from"] == file+"::Following" && edge["to"] == file+"::Target"
					outgoing = outgoing || edge["from"] == file+"::Target" && edge["to"] == file+"::Example"
				}
				require.True(t, incoming, "unchanged actual caller relation missing")
				if call.operation == "call_chain" {
					require.True(t, outgoing, "actual target-to-callee relation missing")
				}
			}
		})
	}
}
