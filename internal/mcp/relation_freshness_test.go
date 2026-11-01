package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// newRelationFixture indexes a committed git repository in which Target
// (target.go) is called from caller.go, beside the unrelated other.go and
// namer.go. It returns the server and the indexed root receipts are keyed by.
func newRelationFixture(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"target.go": "package p\n\nfunc Target() {}\n",
		"caller.go": "package p\n\nfunc Caller() {\n\tTarget()\n}\n",
		"other.go":  "package p\n\nfunc Other() {}\n",
		"namer.go":  "package p\n\nfunc Namer() {}\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
	}
	checkoutMutationGit(t, dir, "init", "-q")
	checkoutMutationGit(t, dir, "add", ".")
	checkoutMutationGit(t, dir, "commit", "-q", "-m", "base")

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil), idx.RootPath()
}

// writeUnindexed changes a file on disk without telling the graph, the state
// a pending edit leaves behind.
func writeUnindexed(t *testing.T, root, name, src string) string {
	t.Helper()
	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	return path
}

func targetID(t *testing.T, s *Server) string {
	t.Helper()
	for _, n := range s.graph.AllNodes() {
		if n.Name == "Target" && n.Kind == graph.KindFunction {
			return n.ID
		}
	}
	t.Fatal("Target is not indexed")
	return ""
}

func callChangeTool(t *testing.T, handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error), name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	req := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}}
	result, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func TestChangeToolsIgnorePendingReceiptOutsideScope(t *testing.T) {
	s, root := newRelationFixture(t)
	writeUnindexed(t, root, "caller.go", "package p\n\nfunc Caller() {\n\tTarget()\n\tTarget()\n}\n")
	pendingFreshnessReceipt(s, "receipt-other", "", filepath.Join(root, "other.go"), 1)

	impact := lifecycleResultPayload(t, callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
		map[string]any{"ids": targetID(t, s), "format": "json"}))
	detect := lifecycleResultPayload(t, callChangeTool(t, s.handleDetectChanges, "detect_changes",
		map[string]any{"format": "json"}))
	for tool, payload := range map[string]map[string]any{"impact": impact, "detect": detect} {
		require.NotContains(t, payload, "graph_freshness", "%s labelled an edit outside its scope: %v", tool, payload)
		require.Less(t, payload["barrier_wait_ms"], float64(100), "%s waited on an edit outside its scope", tool)
	}
	require.Equal(t, []any{"caller.go"}, detect["changed_files"])
	require.NotEmpty(t, detect["changed_symbols"])
}

func TestChangeToolsScopeIncludesFilesNamingTheSymbol(t *testing.T) {
	s, root := newRelationFixture(t)
	// namer.go now calls Target, but the graph has no edge for it yet;
	// other.go names only a longer identifier.
	pendingFreshnessReceipt(s, "receipt-namer", "",
		writeUnindexed(t, root, "namer.go", "package p\n\nfunc Namer() {\n\tTarget()\n}\n"), 1)
	pendingFreshnessReceipt(s, "receipt-other", "",
		writeUnindexed(t, root, "other.go", "package p\n\nfunc Other() {\n\tTargetX()\n}\n"), 2)

	payload := lifecycleResultPayload(t, callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
		map[string]any{"ids": targetID(t, s), "format": "json"}))
	require.Equal(t, "pending", payload["graph_freshness"])
	require.Equal(t, "relations", payload["freshness_scope"])
	require.Equal(t, false, payload["complete"])
	require.Equal(t, []any{"namer.go"}, payload["pending_files"])
	require.Equal(t, float64(1), payload["pending_file_count"])
	require.Less(t, payload["barrier_wait_ms"], float64(500))
}

func TestChangeToolsUnresolvedIDDoesNotWidenBarrier(t *testing.T) {
	s, root := newRelationFixture(t)
	pendingFreshnessReceipt(s, "receipt-other", "", filepath.Join(root, "other.go"), 1)
	pendingFreshnessReceipt(s, "receipt-fresh", "",
		writeUnindexed(t, root, "fresh.go", "package p\n\nfunc Fresh() {}\n"), 2)

	// Defined by a pending edit the graph has not read: labelled, and bounded
	// by the budget rather than by every receipt in the repository.
	payload := lifecycleResultPayload(t, callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
		map[string]any{"ids": "fresh.go::Fresh", "format": "json"}))
	require.Equal(t, "pending", payload["graph_freshness"])
	require.Equal(t, []any{"fresh.go"}, payload["pending_files"])
	require.Less(t, payload["barrier_wait_ms"], float64(500))

	// No pending edit relates to it: answered at once, unlabelled.
	payload = lifecycleResultPayload(t, callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
		map[string]any{"ids": "gone.go::Gone", "format": "json"}))
	require.NotContains(t, payload, "graph_freshness")
	require.Less(t, payload["barrier_wait_ms"], float64(100))
}

func TestChangeToolsRequireFreshWaitsUntilWaitDeadline(t *testing.T) {
	s, root := newRelationFixture(t)
	target := pendingFreshnessReceipt(s, "receipt-target", "", filepath.Join(root, "target.go"), 1)
	caller := pendingFreshnessReceipt(s, "receipt-caller", "",
		writeUnindexed(t, root, "caller.go", "package p\n\nfunc Caller() {\n\tTarget()\n\tTarget()\n}\n"), 2)
	deadline := func(d time.Duration) string { return time.Now().Add(d).Format(time.RFC3339Nano) }

	// The receipt never lands: the wait runs past the default budget to the
	// caller's deadline, then refuses.
	started := time.Now()
	result := callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
		map[string]any{"ids": targetID(t, s), "require_fresh": true, "wait_deadline": deadline(700 * time.Millisecond)})
	require.True(t, result.IsError)
	require.GreaterOrEqual(t, time.Since(started), 600*time.Millisecond)
	require.Contains(t, freshnessToolResultText(result), "relations: pending (2 files)")
	require.Contains(t, freshnessToolResultText(result), "target.go")

	result = callChangeTool(t, s.handleDetectChanges, "detect_changes",
		map[string]any{"require_fresh": true, "wait_deadline": deadline(500 * time.Millisecond)})
	require.True(t, result.IsError)
	require.Contains(t, freshnessToolResultText(result), "caller.go")

	// The receipts land after the default budget but before the deadline:
	// the answer waits for them and carries no label.
	go func() {
		time.Sleep(400 * time.Millisecond)
		completeFreshnessReceipt(target, indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true})
		completeFreshnessReceipt(caller, indexer.MutationResult{RequestedGeneration: 2, AppliedGeneration: 2, Reindexed: true})
	}()
	started = time.Now()
	payload := lifecycleResultPayload(t, callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
		map[string]any{"ids": targetID(t, s), "format": "json", "require_fresh": true, "wait_deadline": deadline(5 * time.Second)}))
	require.GreaterOrEqual(t, time.Since(started), 350*time.Millisecond)
	require.NotContains(t, payload, "graph_freshness")
}

func TestChangeAnswersStartWithRelationsLine(t *testing.T) {
	s, root := newRelationFixture(t)
	pendingFreshnessReceipt(s, "receipt-caller", "",
		writeUnindexed(t, root, "caller.go", "package p\n\nfunc Caller() {\n\tTarget()\n\tTarget()\n}\n"), 1)
	const line = "relations: pending (1 files)"
	id := targetID(t, s)

	for _, format := range []string{"toon", "gcx"} {
		impact := freshnessToolResultText(callChangeTool(t, s.handleEnhancedChangeImpact, "explain_change_impact",
			map[string]any{"ids": id, "format": format}))
		lines := strings.SplitN(impact, "\n", 3)
		require.GreaterOrEqual(t, len(lines), 2, impact)
		if format == "gcx" {
			require.True(t, strings.HasPrefix(lines[0], "GCX1 "), impact)
			require.Contains(t, lines[0], " graph_freshness=pending")
			require.Contains(t, lines[0], " pending_file_count=1")
			require.Contains(t, lines[0], " barrier_wait_ms=")
			require.Equal(t, "# "+line, lines[1])
		} else {
			require.Equal(t, line, lines[0], impact)
		}
	}

	detect := freshnessToolResultText(callChangeTool(t, s.handleDetectChanges, "detect_changes",
		map[string]any{"format": "toon"}))
	require.Equal(t, line, strings.SplitN(detect, "\n", 2)[0], detect)
}

func nodeID(t *testing.T, s *Server, name string) string {
	t.Helper()
	for _, n := range s.graph.AllNodes() {
		if n.Name == name && n.Kind == graph.KindFunction {
			return n.ID
		}
	}
	t.Fatalf("%s is not indexed", name)
	return ""
}

// A call to a symbol that an edit added and the graph has not read yet must
// block safe_delete like a graph reference, in plain and propagate mode. An
// edit to the symbol's own file blocks it as a whole: it may add a caller
// there, and the graph's span of the symbol may no longer hold.
func TestSafeDeleteRejectsTargetCalledFromPendingFile(t *testing.T) {
	for name, edit := range map[string]struct {
		file, src string
		ref       map[string]any
	}{
		"caller file": {"namer.go", "package p\n\nfunc Namer() {\n\tOther()\n}\n",
			map[string]any{"kind": "provisional", "file_path": "namer.go", "line": float64(4)}},
		"definition file": {"other.go", "package p\n\nfunc Extra() {\n\tOther()\n}\n\nfunc Other() {}\n",
			map[string]any{"kind": "provisional", "file_path": "other.go"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, root := newRelationFixture(t)
			pendingFreshnessReceipt(s, "receipt-"+edit.file, "", writeUnindexed(t, root, edit.file, edit.src), 1)
			otherPath := filepath.Join(root, "other.go")
			before, err := os.ReadFile(otherPath)
			require.NoError(t, err)
			id := nodeID(t, s, "Other")

			payload := lifecycleResultPayload(t, callChangeTool(t, s.handleSafeDeleteSymbol, "safe_delete_symbol",
				map[string]any{"id": id, "dry_run": false, "format": "json"}))
			require.Equal(t, "rejected_has_references", payload["status"])
			require.Equal(t, []any{edit.ref}, payload["references"])
			require.Equal(t, "pending", payload["graph_freshness"])

			payload = lifecycleResultPayload(t, callChangeTool(t, s.handleSafeDeleteSymbol, "safe_delete_symbol",
				map[string]any{"id": id, "propagate": true, "dry_run": false, "format": "json"}))
			require.Equal(t, "propagation_blocked_manual", payload["status"])
			require.Equal(t, float64(1), payload["manual"])

			after, err := os.ReadFile(otherPath)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "the target was deleted with a pending caller")
		})
	}

	// A call the graph already holds is listed once, as the graph's
	// reference, when its file has an unrelated pending edit; propagate can
	// still remove it.
	t.Run("graph reference", func(t *testing.T) {
		s, root := newRelationFixture(t)
		pendingFreshnessReceipt(s, "receipt-caller", "",
			writeUnindexed(t, root, "caller.go", "package p\n\nfunc Caller() {\n\tTarget()\n}\n\nfunc More() {}\n"), 1)
		payload := lifecycleResultPayload(t, callChangeTool(t, s.handleSafeDeleteSymbol, "safe_delete_symbol",
			map[string]any{"id": targetID(t, s), "format": "json"}))
		require.Equal(t, "rejected_has_references", payload["status"])
		require.Equal(t, float64(1), payload["reference_count"], "%v", payload["references"])
		payload = lifecycleResultPayload(t, callChangeTool(t, s.handleSafeDeleteSymbol, "safe_delete_symbol",
			map[string]any{"id": targetID(t, s), "propagate": true, "format": "json"}))
		require.Equal(t, "propagation_plan", payload["status"])
		require.Equal(t, float64(0), payload["manual"], "%v", payload["caller_patches"])
	})

	// Target is orphaned with Caller by the graph's edges, but a pending edit
	// now calls it: the cascade must not delete it with Caller.
	t.Run("cascade member", func(t *testing.T) {
		s, root := newRelationFixture(t)
		pendingFreshnessReceipt(s, "receipt-namer", "",
			writeUnindexed(t, root, "namer.go", "package p\n\nfunc Namer() {\n\tTarget()\n}\n"), 1)
		sources := map[string]string{}
		for _, name := range []string{"caller.go", "target.go"} {
			data, err := os.ReadFile(filepath.Join(root, name))
			require.NoError(t, err)
			sources[name] = string(data)
		}
		payload := lifecycleResultPayload(t, callChangeTool(t, s.handleSafeDeleteSymbol, "safe_delete_symbol",
			map[string]any{"id": nodeID(t, s, "Caller"), "cascade": "apply", "dry_run": false, "format": "json"}))
		require.Equal(t, "rejected_has_references", payload["status"], "%v", payload)
		require.Equal(t, []any{map[string]any{"kind": "provisional", "file_path": "namer.go", "line": float64(4)}}, payload["references"])
		for name, src := range sources {
			data, err := os.ReadFile(filepath.Join(root, name))
			require.NoError(t, err)
			require.Equal(t, src, string(data), "%s changed although the cascade was refused", name)
		}
	})
}

// The relation-reading mutators answer at once and label the answer while an
// edit in their scope never publishes; they never wait it out or refuse.
func TestRelationToolsLabelAndNeverRefuseWithUnresolvedReceipt(t *testing.T) {
	s, root := newRelationFixture(t)
	pendingFreshnessReceipt(s, "receipt-namer", "",
		writeUnindexed(t, root, "namer.go", "package p\n\nfunc Namer() {\n\tTarget()\n}\n"), 1)
	id := targetID(t, s)
	rename := `{"changes":{"target.go":[{"range":{"start":{"line":2,"character":5},"end":{"line":2,"character":11}},"newText":"Renamed"}]}}`
	comment := `{"changes":{"file://` + filepath.ToSlash(filepath.Join(root, "namer.go")) +
		`":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"// Namer names Target.\n"}]}}`

	for tool, call := range map[string]func() *mcpgo.CallToolResult{
		"change_contract": func() *mcpgo.CallToolResult {
			return callChangeTool(t, s.handleChangeContract, "change_contract",
				map[string]any{"source": "symbols", "symbols": id, "format": "json"})
		},
		"preview_edit": func() *mcpgo.CallToolResult {
			return callChangeTool(t, s.handlePreviewEdit, "preview_edit",
				map[string]any{"workspace_edit": rename, "diagnostics": false})
		},
		// No symbol is removed or renamed: only the touched file, named by
		// an absolute file URI, brings the pending edit into scope.
		"preview_edit file uri": func() *mcpgo.CallToolResult {
			return callChangeTool(t, s.handlePreviewEdit, "preview_edit",
				map[string]any{"workspace_edit": comment, "diagnostics": false})
		},
	} {
		started := time.Now()
		payload := lifecycleResultPayload(t, call())
		require.Less(t, time.Since(started), 2*time.Second, "%s waited on a receipt that never publishes", tool)
		require.Equal(t, "pending", payload["graph_freshness"], "%s: %v", tool, payload)
		require.Equal(t, []any{"namer.go"}, payload["pending_files"], tool)
		require.Equal(t, false, payload["complete"], tool)
	}
}
