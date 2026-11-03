package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func readbackReferenceWire(body any) json.RawMessage {
	b, _ := json.Marshal(body)
	r, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "isError": false})
	return r
}
func readbackReferenceOracleFixture(t *testing.T) (readbackReferenceImage, map[string]map[string]any) {
	t.Helper()
	image, err := readbackReferenceParse("probe.go", "package probe\n\nfunc Target() int {\n v := 7\n return v\n}\n\nfunc Survivor() int { return 2 }\n")
	require.NoError(t, err)
	image.Prefix = "owned"
	rows := []any{}
	for _, name := range []string{"Target", "Survivor"} {
		s := image.Symbols[name]
		rows = append(rows, map[string]any{"id": "owned/probe.go::" + name, "name": name, "kind": "function", "file_path": "probe.go", "start_line": s.StartLine, "end_line": s.EndLine})
	}
	target := image.Symbols["Target"]
	return image, map[string]map[string]any{
		"source":          {"id": "owned/probe.go::Target", "name": "Target", "file_path": "probe.go", "source": target.Body, "start_line": target.StartLine, "end_line": target.EndLine},
		"summary":         {"nodes": rows, "total_nodes": len(rows), "truncated": false},
		"editing_context": {"file": map[string]any{"id": "owned/probe.go"}, "defines": rows, "graph_freshness": "pending", "reason": "store_busy"},
		"file":            {"path": "probe.go", "content": image.Bytes},
	}
}

func TestReadbackReferenceActualQuartetRequiresExactBytesAndAllRanges(t *testing.T) {
	image, bodies := readbackReferenceOracleFixture(t)
	for _, endpoint := range []string{"source", "summary", "editing_context", "file"} {
		t.Run(endpoint, func(t *testing.T) {
			require.True(t, readbackReferenceCorrect(endpoint, readbackReferenceWire(bodies[endpoint]), image, "Target", false))
			require.False(t, readbackReferenceCorrect(endpoint, readbackReferenceWire(map[string]any{}), image, "Target", false))
		})
	}
}
func TestReadbackReferenceWrongSourceOrOldRangesNeverCountExact(t *testing.T) {
	image, b := readbackReferenceOracleFixture(t)
	b["source"]["source"] = strings.ReplaceAll(image.Symbols["Target"].Body, "7", "9")
	require.False(t, readbackReferenceCorrect("source", readbackReferenceWire(b["source"]), image, "Target", false))
	b["source"]["source"] = image.Symbols["Target"].Body
	b["source"]["start_line"] = 2
	require.False(t, readbackReferenceCorrect("source", readbackReferenceWire(b["source"]), image, "Target", false))
	rows := b["summary"]["nodes"].([]any)
	rows[1].(map[string]any)["start_line"] = 1
	require.False(t, readbackReferenceCorrect("summary", readbackReferenceWire(b["summary"]), image, "Target", false))
	require.False(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(b["editing_context"]), image, "Target", false))
}
func TestReadbackReferenceSummaryCannotHideMissingOrExtraDeclarations(t *testing.T) {
	image, b := readbackReferenceOracleFixture(t)
	rows := b["summary"]["nodes"].([]any)
	b["summary"]["nodes"] = rows[:1]
	b["summary"]["total_nodes"] = 1
	require.False(t, readbackReferenceCorrect("summary", readbackReferenceWire(b["summary"]), image, "Target", false))
	b["editing_context"]["defines"] = []any{}
	require.False(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(b["editing_context"]), image, "Target", false))
	b["file"]["content"] = image.Bytes + "\n"
	require.False(t, readbackReferenceCorrect("file", readbackReferenceWire(b["file"]), image, "Target", false))
}
func TestReadbackReferenceOfferedDenominatorNeverDropsRefusalOrCensor(t *testing.T) {
	good := make([]bool, 100)
	for i := range good {
		good[i] = true
	}
	s := summarizeReadbackReference(100, good)
	require.True(t, s.Pass)
	require.Equal(t, 100, s.Correct)
	good[99] = false
	s = summarizeReadbackReference(100, good)
	require.False(t, s.Pass)
	require.Equal(t, 99, s.Correct)
	require.Equal(t, 1, s.Failures)
	s = summarizeReadbackReference(100, good[:99])
	require.False(t, s.Pass)
	require.Equal(t, 1, s.Missing)
	s = summarizeReadbackReference(100, append(good, true))
	require.False(t, s.Pass, "duplicate observations cannot inflate the offered denominator")
}
func TestReadbackReferenceCellCoverageIsFiniteAndCannotDiluteFailures(t *testing.T) {
	for _, agents := range []int{1, 4, 8, 16, 32} {
		for _, mode := range []string{"", "local"} {
			for kind, tools := range readbackReferenceTools {
				for _, tool := range tools {
					require.NoError(t, (readbackReferenceCell{Agents: agents, ACKMode: mode, Kind: kind, Tool: tool, Offers: 100}).validate())
				}
			}
		}
	}
	require.Error(t, (readbackReferenceCell{Agents: 8, Kind: "modify_body", Tool: "edit.file", Offers: 101}).validate())
	require.Error(t, (readbackReferenceCell{Agents: 8, Kind: "unknown", Tool: "edit.file", Offers: 100}).validate())
}

func TestReadbackReferenceDeletionRequiresExactScopedRefusalAndSurvivors(t *testing.T) {
	image, b := readbackReferenceOracleFixture(t)
	image.Bytes = "package probe\n\nfunc Survivor() int { return 2 }\n"
	delete(image.Symbols, "Target")
	require.True(t, readbackReferenceCorrect("source", absentSourceWire("symbol not found: owned/probe.go::Target"), image, "Target", true))
	require.False(t, readbackReferenceCorrect("source", absentSourceWire("symbol not found: foreign/probe.go::Target"), image, "Target", true))
	structured := map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": `{"condition":"symbol_not_found","message":"symbol not found: removed by pending edit","data":{"id":"owned/probe.go::Target","reason":"removed by pending edit"}}`}}}
	raw, _ := json.Marshal(structured)
	require.True(t, readbackReferenceCorrect("source", raw, image, "Target", true))
	require.False(t, readbackReferenceCorrect("source", readbackReferenceWire(b["source"]), image, "Target", true))
	require.False(t, readbackReferenceCorrect("summary", readbackReferenceWire(b["summary"]), image, "Target", true), "a still-visible deleted row cannot be hidden by scoped not-found")
}
func TestReadbackReferenceNeverUsesJSONAuxiliaryAfterInvalidPrimary(t *testing.T) {
	image, b := readbackReferenceOracleFixture(t)
	valid, _ := json.Marshal(b["source"])
	raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": "not JSON"}, map[string]any{"type": "text", "text": string(valid)}}})
	require.False(t, readbackReferenceCorrect("source", raw, image, "Target", false))
}
