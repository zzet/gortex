package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Compact's freshness prefix counts files but does not identify them. The
// consumer wire must carry the actual scoped paths as well; the load oracle
// cannot manufacture pending_files from the fixture it hoped to observe.
func TestCompactRelationConsumerKeepsScopedPendingFiles(t *testing.T) {
	for _, tool := range []string{"rename_symbol", "verify_change", "find_dead_code", "detect_changes"} {
		t.Run(tool, func(t *testing.T) {
			s, root := newRelationFixture(t)
			t.Cleanup(s.indexer.Close)
			target := targetID(t, s)
			namer := "package p\nfunc Namer() { Target() }\n"
			if tool == "find_dead_code" {
				unused := writeUnindexed(t, root, "unused.go", "package p\nfunc unused() {}\n")
				require.NoError(t, s.indexer.IndexFile(unused))
				namer = "package p\nfunc Namer() { unused() }\n"
			}
			pendingFreshnessReceipt(s, "compact-pending-namer", "", writeUnindexed(t, root, "namer.go", namer), 1)
			args := map[string]any{"compact": true, "format": "json", "max_bytes": 0}
			var text string
			switch tool {
			case "rename_symbol":
				args["id"], args["new_name"], args["dry_run"] = target, "Renamed", true
				result := callChangeTool(t, s.handleRenameSymbol, tool, args)
				require.False(t, result.IsError)
				text = freshnessToolResultText(result)
			case "verify_change":
				args["changes"] = fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target(probe int)"}]`, target)
				result := callChangeTool(t, s.handleVerifyChange, tool, args)
				require.False(t, result.IsError)
				text = freshnessToolResultText(result)
			case "find_dead_code":
				result := callChangeTool(t, s.handleFindDeadCode, tool, args)
				require.False(t, result.IsError)
				text = freshnessToolResultText(result)
			case "detect_changes":
				result := callChangeTool(t, s.handleDetectChanges, tool, args)
				require.False(t, result.IsError)
				text = freshnessToolResultText(result)
			}
			require.NotEmpty(t, text)
			require.False(t, strings.HasPrefix(strings.TrimSpace(text), "{"), "JSON fallback is not compact coverage")
			scope := "relations"
			if tool == "find_dead_code" {
				scope = "derived"
			}
			require.True(t, strings.HasPrefix(text, scope+": pending ("), "actual tier must precede the verdict: %s", text)
			require.Contains(t, text, "pending_file \"namer.go\"\n", "pending scope identity must be producer evidence, not inferred by the harness")
		})
	}
}
