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
