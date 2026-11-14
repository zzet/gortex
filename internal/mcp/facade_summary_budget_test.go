package mcp

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestRegisteredSummaryPreservesFacadeOutputBudget(t *testing.T) {
	const file = "summary.go"
	const definitions = 200
	var source strings.Builder
	source.WriteString("package fixture\n")
	for i := range definitions {
		fmt.Fprintf(&source, "func DefinitionWithEnoughIdentityForAnOversizedSummary%03d() int { return %d }\n", i, i)
	}
	s, root := setupMoveInlineRepo(t, map[string]string{file: source.String()})
	t.Cleanup(s.DrainBackground)
	s.ToolCallTimeout = -1
	s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
		filepath.Join(root, file): {Generation: 1, AckAt: time.Now()},
	}})
	for _, tc := range []struct {
		name   string
		legacy bool
		nested map[string]any
		top    map[string]any
		cap    int
	}{
		{name: "legacy_flat_zero", legacy: true, top: map[string]any{"max_bytes": 0}},
		{name: "facade_flat_zero", top: map[string]any{"max_bytes": 0}},
		{name: "facade_nested_zero", nested: map[string]any{"max_bytes": 0}},
		{name: "facade_default", cap: defaultMaxBytes},
		{name: "facade_nested_positive", nested: map[string]any{"max_bytes": 20000}, cap: 20000},
		{name: "facade_top_zero_wins", nested: map[string]any{"max_bytes": 20000}, top: map[string]any{"max_bytes": 0}},
		{name: "facade_top_positive_wins", nested: map[string]any{"max_bytes": 0}, top: map[string]any{"max_bytes": 20000}, cap: 20000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithSessionID(t.Context(), "summary-budget-"+tc.name)
			name := "read"
			args := map[string]any{"operation": "summary", "target": map[string]any{"file": file}, "output": map[string]any{"format": "json"}}
			if tc.legacy {
				name = "get_file_summary"
				args = map[string]any{"path": file, "format": "json"}
				s.NoteSessionToolPolicy("summary-budget-"+tc.name, "full", "")
			} else {
				s.NoteSessionToolPolicy("summary-budget-"+tc.name, FacadeSurfaceVersion, "hide")
				for key, value := range tc.nested {
					args["output"].(map[string]any)[key] = value
				}
			}
			for key, value := range tc.top {
				args[key] = value
			}
			res := callEditOperationFrame(t, s, ctx, name, args)
			require.False(t, res.IsError, resultText(res))
			body := resultJSON(t, res)
			require.Equal(t, "pending", body["freshness"].(map[string]any)["graph_freshness"])
			rows := body["nodes"].([]any)
			if tc.cap == 0 {
				require.Greater(t, len(resultText(res)), defaultMaxBytes, "fixture must exceed the default response cap")
				require.Len(t, rows, definitions, "explicit zero must survive the final freshness fit")
				require.NotEqual(t, true, body[budgetTruncatedKey])
			} else {
				require.LessOrEqual(t, len(resultText(res)), tc.cap)
				require.Less(t, len(rows), definitions)
				require.Equal(t, true, body[budgetTruncatedKey])
			}
		})
	}
}
