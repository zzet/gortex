package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestVerifyChangeNeverBareCleanWhilePending(t *testing.T) {
	for _, format := range []string{"json", "compact", "toon", "gcx"} {
		t.Run(format, func(t *testing.T) {
			s, root := newRelationFixture(t)
			pendingFreshnessReceipt(s, "pending-caller", "", writeUnindexed(t, root, "namer.go", "package p\nfunc Namer() { Target() }\n"), 1)
			args := map[string]any{"changes": fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target()"}]`, targetID(t, s)), "format": format}
			if format == "compact" {
				args["compact"] = true
			}
			result := callChangeTool(t, s.handleVerifyChange, "verify_change", args)
			require.False(t, result.IsError)
			text := freshnessToolResultText(result)
			if format == "json" {
				payload := lifecycleResultPayload(t, result)
				require.Equal(t, "pending", payload["graph_freshness"])
				require.Equal(t, "relations", payload["freshness_scope"])
				require.Equal(t, false, payload["complete"])
			} else {
				require.Contains(t, text, "relations: pending")
				if format == "compact" {
					require.True(t, strings.HasPrefix(text, "relations: pending"), "freshness must precede any clean verdict")
				}
			}
		})
	}
}

func TestEditPlanLabelsPendingCallerWithoutWaiting(t *testing.T) {
	s, root := newRelationFixture(t)
	pendingFreshnessReceipt(s, "pending-caller", "", filepath.Join(root, "caller.go"), 1)
	payload := lifecycleResultPayload(t, callChangeTool(t, s.handleGetEditPlan, "get_edit_plan", map[string]any{"ids": targetID(t, s), "format": "json"}))
	require.Equal(t, "pending", payload["graph_freshness"])
	require.Equal(t, "relations", payload["freshness_scope"])
}

func TestDeadCodePendingUnknownInEveryMode(t *testing.T) {
	for _, format := range []string{"json", "compact", "toon", "gcx"} {
		t.Run(format, func(t *testing.T) {
			s, root := newRelationFixture(t)
			unused := writeUnindexed(t, root, "unused.go", "package p\nfunc unused() {}\n")
			require.NoError(t, s.indexer.IndexFile(unused))
			baseline := callChangeTool(t, s.handleFindDeadCode, "find_dead_code", map[string]any{"format": "json"})
			require.Contains(t, freshnessToolResultText(baseline), "unused.go::unused", "prove a real dead-code candidate before adding its unindexed caller")
			pendingFreshnessReceipt(s, "pending-namer", "", writeUnindexed(t, root, "namer.go", "package p\nfunc Namer() { unused() }\n"), 1)
			args := map[string]any{"format": format}
			if format == "compact" {
				args["compact"] = true
			}
			result := callChangeTool(t, s.handleFindDeadCode, "find_dead_code", args)
			require.False(t, result.IsError)
			text := freshnessToolResultText(result)
			require.Contains(t, text, "pending_unknown", "an unindexed caller prevents a definite dead-code conclusion in every mode")
			switch format {
			case "json":
				payload := lifecycleResultPayload(t, result)
				require.Equal(t, "pending", payload["graph_freshness"])
				if dead, ok := payload["dead_code"].([]any); ok {
					for _, raw := range dead {
						require.NotEqual(t, "unused.go::unused", raw.(map[string]any)["id"])
					}
				}
				unknown, ok := payload["pending_unknown"].([]any)
				require.True(t, ok)
				var found bool
				for _, raw := range unknown {
					if raw.(map[string]any)["id"] == "unused.go::unused" {
						found = true
					}
				}
				require.True(t, found, "pending candidate belongs in the separate unknown list")
			case "compact":
				for _, line := range strings.Split(text, "\n") {
					require.False(t, strings.HasPrefix(line, "function unused.go::unused "), "pending candidate must never be rendered as dead")
				}
			}
		})
	}
}

func TestRenameFindsNewSite100Of100(t *testing.T) {
	s, root := newRelationFixture(t)
	pendingFreshnessReceipt(s, "pending-new-site", "", filepath.Join(root, "namer.go"), 1)
	for i := range 100 {
		src := "package p\n" + strings.Repeat("\n", i) + "func Namer() { Target() }\n"
		writeUnindexed(t, root, "namer.go", src)
		time.Sleep(100 * time.Millisecond)
		payload := lifecycleResultPayload(t, callChangeTool(t, s.handleRenameSymbol, "rename_symbol", map[string]any{"id": targetID(t, s), "new_name": "Renamed", "dry_run": true, "format": "json"}))
		rows, ok := payload["edits"].([]any)
		require.True(t, ok)
		var found bool
		for _, raw := range rows {
			row := raw.(map[string]any)
			if row["file"] == "namer.go" && row["confidence"] == "provisional" {
				found = true
				require.EqualValues(t, i+2, row["line"])
			}
		}
		require.True(t, found, "new call site %d was neither proven nor labelled provisional", i)
	}
}

func TestRenameProvisionalSiteIsNeverWritten(t *testing.T) {
	s, root := newRelationFixture(t)
	const original = "package p\nfunc Namer() { Target() }\n"
	path := writeUnindexed(t, root, "namer.go", original)
	pendingFreshnessReceipt(s, "pending-new-site", "", path, 1)
	payload := lifecycleResultPayload(t, callChangeTool(t, s.handleRenameSymbol, "rename_symbol", map[string]any{"id": targetID(t, s), "new_name": "Renamed", "format": "json"}))
	require.Equal(t, "pending", payload["graph_freshness"])
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, string(current), "provisional references are reported for manual recovery, never auto-written")
}

func TestRenameNeverWritesPendingDefinitionOrKnownCaller(t *testing.T) {
	for _, file := range []string{"target.go", "caller.go"} {
		t.Run(file, func(t *testing.T) {
			s, root := newRelationFixture(t)
			contents := "package p\n\nfunc Target() {} // pending edit\n"
			if file == "caller.go" {
				contents = "package p\n\nfunc Caller() {\n\tTarget()\n\tTarget()\n}\n"
			}
			path := writeUnindexed(t, root, file, contents)
			pendingFreshnessReceipt(s, "pending-file", "", path, 1)
			callerBefore, err := os.ReadFile(filepath.Join(root, "caller.go"))
			require.NoError(t, err)
			payload := lifecycleResultPayload(t, callChangeTool(t, s.handleRenameSymbol, "rename_symbol", map[string]any{"id": targetID(t, s), "new_name": "Renamed", "format": "json"}))
			require.Equal(t, "pending", payload["graph_freshness"])
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, contents, string(current), "a graph-backed pending site must also stay manual")
			if file == "target.go" {
				require.Equal(t, false, payload["written"], "a pending definition cannot anchor any actual rename")
				callerAfter, err := os.ReadFile(filepath.Join(root, "caller.go"))
				require.NoError(t, err)
				require.Equal(t, callerBefore, callerAfter)
			}
		})
	}
}

// Native watcher evidence must flow through the same producer and mutation
// safety checks as tool-write receipts, including files with no graph rows.
func TestRenameNativeTierGapIsProvisionalAndNeverWritten(t *testing.T) {
	for _, file := range []string{"namer.go", "caller.go", "target.go"} {
		t.Run(file, func(t *testing.T) {
			s, root := newRelationFixture(t)
			text := "package p\nfunc Namer() { Target() }\n"
			switch file {
			case "caller.go":
				text = "package p\nfunc Caller() { Target(); Target() }\n"
			case "target.go":
				text = "package p\nfunc Target() {} // native edit\n"
			}
			path := writeUnindexed(t, root, file, text)
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				path: {Generation: 7, AckAt: time.Now()},
			}})
			payload := lifecycleResultPayload(t, callChangeTool(t, s.handleRenameSymbol, "rename_symbol", map[string]any{
				"id": targetID(t, s), "new_name": "Renamed", "format": "json",
			}))
			require.Equal(t, "pending", payload["graph_freshness"])
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, text, string(current), "native pending source must never be an automatic rename target")
			require.Greater(t, payload["provisional_site_count"], float64(0))
			if file == "target.go" {
				require.Equal(t, false, payload["written"])
			}
		})
	}
}
