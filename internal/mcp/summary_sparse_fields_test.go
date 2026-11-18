package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"
)

// The fixed bounded file needs all definition identities, rather than a larger
// response budget or a response that only happens to include early rows.
func TestFileDefinitionSparseFieldsPreserveBoundedCoverage(t *testing.T) {
	const file = "fixture01/sample.go"
	const fields = "id,name,file_path,start_line,end_line"
	// The bounded acceptance shape uses 19-byte paths and 35-byte markers.
	// Keep generic names at least that long so sparse-size proof is realistic.
	require.GreaterOrEqual(t, len(file), 19)
	require.GreaterOrEqual(t, len("AddedDefinitionWithStableIdentity099"), 35)
	var source strings.Builder
	source.WriteString("package fixture\nfunc Example() int { return 1 }\nfunc Caller() int { return Example() }\n")
	for i := range 8 {
		fmt.Fprintf(&source, "func Baseline%02d() int { return 1 }\n", i)
	}
	for i := range 100 {
		fmt.Fprintf(&source, "func AddedDefinitionWithStableIdentity%03d() int { return Example() }\n", i)
	}
	s, _ := setupMoveInlineRepo(t, map[string]string{file: source.String()})
	t.Cleanup(s.DrainBackground)
	s.ToolCallTimeout = -1
	const session = "sparse-definition-coverage"
	s.NoteSessionToolPolicy(session, "facade-v1", "hide")
	ctx := WithSessionID(t.Context(), session)
	for _, format := range []string{"json", "toon"} {
		for _, operation := range []string{"summary", "editing_context"} {
			t.Run(format+"/"+operation, func(t *testing.T) {
				output := map[string]any{"format": format, "fields": fields, "max_bytes": 40000}
				result := callEditOperationFrame(t, s, ctx, "read", map[string]any{"operation": operation, "target": map[string]any{"file": file}, "output": output})
				require.False(t, result.IsError, "%s", toolText(result))
				text := toolText(result)
				t.Logf("serialized_bytes=%d required_definitions=110 required_markers=100 cap=40000", len(text))
				require.LessOrEqual(t, len(text), 40000)
				var body map[string]any
				if format == "json" {
					require.NoError(t, json.Unmarshal([]byte(text), &body))
				} else {
					value, err := toon.Decode([]byte(text), toon.WithStrictMode(true))
					require.NoError(t, err)
					var ok bool
					body, ok = value.(map[string]any)
					require.True(t, ok)
				}
				key := "nodes"
				if operation == "editing_context" {
					key = "defines"
				}
				rows, ok := body[key].([]any)
				require.True(t, ok)
				require.Len(t, rows, 110)
				ids := make(map[string]bool)
				for _, value := range rows {
					row := value.(map[string]any)
					id, ok := row["id"].(string)
					require.True(t, ok)
					require.NotEmpty(t, id)
					require.False(t, ids[id], "duplicate definition identity")
					ids[id] = true
					require.Contains(t, row, "name")
					require.Contains(t, row, "start_line")
					require.NotContains(t, row, "meta")
					require.NotContains(t, row, "signature")
					require.NotContains(t, row, "kind")
				}
				for i := range 100 {
					require.True(t, ids[fmt.Sprintf("%s::AddedDefinitionWithStableIdentity%03d", file, i)])
				}
				require.NotEqual(t, true, body["_truncated_by_budget"], "all bounded definition rows must fit")
				if operation == "summary" {
					require.Len(t, body["edges"], 202)
				}
			})
		}
	}
}
