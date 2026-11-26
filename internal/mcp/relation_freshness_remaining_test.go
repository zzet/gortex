package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
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
				// Compact rows are tab-separated, with each field quoted.
				const row = "\t\"function\"\t\"unused.go::unused\"\t"
				require.Contains(t, text, "\npending_unknown"+row)
				require.NotContains(t, text, "\ndead"+row, "pending candidate must never be rendered as dead")
			}
		})
	}
	// The cases above inject the pending receipt. This one reaches the same
	// state through a real local edit ACK whose publication is held, and reads
	// the registered analyze facade with compact output.
	t.Run("registered_compact_after_local_ack", func(t *testing.T) {
		t.Setenv("GORTEX_TOOLS", "full")
		t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
		selectedProjectionRegistry(t)
		root := t.TempDir()
		caller := filepath.Join(root, "caller.go")
		const before = "package probe\n\nfunc Caller() {}\n"
		const after = "package probe\n\nfunc Caller() { unused() }\n"
		require.NoError(t, os.WriteFile(caller, []byte(before), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "target.go"), []byte("package probe\n\nfunc unused() {}\n"), 0o600))
		sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, sql.Close()) })
		idx := newProjectionTestIndexer(t, sql)
		_, err = idx.Index(root)
		require.NoError(t, err)
		s := NewServer(query.NewEngine(sql), sql, idx, nil, zap.NewNop(), nil)
		s.editAckMode = config.EditAckLocal
		held := make(chan indexer.MutationResult)
		s.SetWatcher(mutationTestWatcher{done: held})
		t.Cleanup(func() {
			close(held)
			s.mutationReceipts.Range(func(_, value any) bool {
				select {
				case <-value.(*mutationReceipt).done:
				case <-time.After(3 * time.Second):
					t.Error("local edit publication ticket did not join")
				}
				return true
			})
		})
		editor := relationBarrierRegisteredSession(t, s, "legacy")
		reader := relationBarrierRegisteredSession(t, s, "facade")
		args := map[string]any{
			"kind":    "dead_code",
			"options": map[string]any{"limit": 100},
			"output":  map[string]any{"format": "json", "compact": true, "max_bytes": 0},
		}
		const candidate = "\"function\"\t\"target.go::unused\"\t\"unused\"\t\"target.go\"\t3\n"
		baseline := guardE2ECall(t, s, reader, 2, "analyze", args)
		require.False(t, baseline.IsError, resultText(baseline))
		require.Contains(t, freshnessToolResultText(baseline), "dead\t"+candidate, "prove a real dead-code candidate before the edit adds its caller")

		ack := preselectionRegisteredCall(t, s, editor, 3, "batch_edit", map[string]any{
			"transaction_id": "compact-dead-code-pending",
			"edits": []any{
				map[string]any{"op": "edit_file", "path": caller, "old_string": before, "new_string": after},
			},
		})
		reply := guardE2ECall(t, s, reader, 4, "analyze", args)

		require.False(t, ack.IsError, resultText(ack))
		body := extractTextResult(t, ack)
		require.Equal(t, "committed", body["disk_status"])
		require.Equal(t, "local", body["ack_mode"])
		require.Equal(t, "pending", body["graph_status"])
		contents, err := os.ReadFile(caller)
		require.NoError(t, err)
		require.Equal(t, after, string(contents))

		require.False(t, reply.IsError, resultText(reply))
		text := freshnessToolResultText(reply)
		require.True(t, strings.HasPrefix(text, "derived: pending ("), "compact freshness must precede the membership rows: %s", text)
		require.Contains(t, text, "\ndead_code_v1\t")
		require.Contains(t, text, "\npending_file \"caller.go\"\n")
		require.Contains(t, text, "\npending_unknown\t"+candidate)
		require.NotContains(t, text, "\ndead\t"+candidate, "pending candidate must never be rendered as dead")
	})
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

func TestDeadCodeRequiresDerivedTierForEachCandidate(t *testing.T) {
	for _, test := range []struct {
		name                      string
		pendingFile               string
		derivedReady, wantUnknown bool
	}{
		{name: "relations-published-derived-pending", pendingFile: "unused.go", wantUnknown: true},
		{name: "derived-published", pendingFile: "unused.go", derivedReady: true},
		{name: "unrelated-derived-pending", pendingFile: "other.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, root := newRelationFixture(t)
			unused := writeUnindexed(t, root, "unused.go", "package p\nfunc unused() {}\n")
			require.NoError(t, s.indexer.IndexFile(unused))
			now := time.Now()
			tier := indexer.MutationTierState{Generation: 11, AckAt: now, FileGraphAt: now, RelationsAt: now}
			if test.derivedReady {
				tier.DerivedAt = now
			}
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				filepath.Join(root, test.pendingFile): tier,
			}})
			payload := lifecycleResultPayload(t, callChangeTool(t, s.handleFindDeadCode, "find_dead_code", map[string]any{"format": "json"}))
			contains := func(key string) bool {
				rows, _ := payload[key].([]any)
				for _, raw := range rows {
					if raw.(map[string]any)["id"] == "unused.go::unused" {
						return true
					}
				}
				return false
			}
			require.Equal(t, test.wantUnknown, contains("pending_unknown"))
			require.Equal(t, !test.wantUnknown, contains("dead_code"))
			if test.wantUnknown {
				require.Equal(t, "pending", payload["graph_freshness"])
				require.Equal(t, "derived", payload["freshness_scope"])
			}
		})
	}
}
