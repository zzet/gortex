package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestRelationConsumersLiteral300msWithoutRequireFresh(t *testing.T) {
	for _, tool := range []string{"verify_change", "get_edit_plan", "change_contract", "preview_edit"} {
		for _, explicitFalse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit_false_%t", tool, explicitFalse), func(t *testing.T) {
				s, root := newRelationFixture(t)
				pendingFreshnessReceipt(s, "never-publishes", "",
					writeUnindexed(t, root, "namer.go", "package p\nfunc Namer() { Target() }\n"), 1)
				id := targetID(t, s)
				args := map[string]any{"format": "json"}
				var handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
				var analysisKey string
				switch tool {
				case "verify_change":
					handler, analysisKey = s.handleVerifyChange, "checked_callers"
					args["changes"] = fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target()"}]`, id)
				case "get_edit_plan":
					handler, analysisKey = s.handleGetEditPlan, "total_files"
					args["ids"] = id
				case "change_contract":
					handler, analysisKey = s.handleChangeContract, "changed_symbols"
					args["source"], args["symbols"] = "symbols", id
				case "preview_edit":
					handler, analysisKey = s.handlePreviewEdit, "touched_files"
					args["workspace_edit"] = `{"changes":{"target.go":[{"range":{"start":{"line":2,"character":5},"end":{"line":2,"character":11}},"newText":"Renamed"}]}}`
					args["diagnostics"] = false
				}
				if explicitFalse {
					args["require_fresh"] = false
				}
				started := time.Now()
				result := callChangeTool(t, handler, tool, args)
				elapsed := time.Since(started)
				require.False(t, result.IsError, "never-publishing receipt must label rather than refuse")
				payload := lifecycleResultPayload(t, result)
				require.Equal(t, "pending", payload["graph_freshness"])
				require.Equal(t, "relations", payload["freshness_scope"])
				require.Equal(t, false, payload["complete"])
				require.Equal(t, []any{"namer.go"}, payload["pending_files"])
				require.Contains(t, payload, analysisKey, "a label alone is not an analysis response")
				// This tiny real walk is included, making the assertion stronger
				// than the issue's 300ms waiting-plus-walk limit for this fixture.
				// It makes no latency claim for an arbitrarily large graph walk.
				t.Logf("actual handler elapsed=%s (includes bounded fixture walk)", elapsed)
				require.LessOrEqual(t, elapsed, 300*time.Millisecond)
			})
		}
	}
}
