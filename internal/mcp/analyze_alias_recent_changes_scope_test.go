package mcp

// Scope regression tests for `analyze kind=recent_changes`, which the
// facade routes to the legacy get_recent_changes tool instead of the
// analyze dispatcher. Because that alias shadows the dispatcher, the
// handler never ran resolveScope — and under the daemon the watcher it
// reads is a MultiWatcher whose History unions the per-repo feeds of every
// tracked repo, across workspaces. Each row's `file` is an absolute path,
// so a workspace-bound session learned sibling workspaces' on-disk roots,
// file names and edit timestamps.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// historyTestWatcher is a watcherHistory double returning a fixed feed —
// the shape MultiWatcher produces after merging its per-repo histories.
type historyTestWatcher struct {
	events []indexer.GraphChangeEvent
}

func (w historyTestWatcher) History() []indexer.GraphChangeEvent { return w.events }

func (w historyTestWatcher) HistorySince(since time.Time) []indexer.GraphChangeEvent {
	var out []indexer.GraphChangeEvent
	for _, ev := range w.events {
		if ev.Timestamp.After(since) {
			out = append(out, ev)
		}
	}
	return out
}

func (historyTestWatcher) OnSymbolChange(indexer.SymbolChangeCallback) {}

// recentChangesServer builds a two-repo server and installs a merged feed
// carrying one event per repo, each stamped with its repo prefix the way a
// per-repo Watcher stamps it.
func recentChangesServer(t *testing.T, repos ...analyzeRepoSpec) (*Server, map[string]string) {
	t.Helper()
	srv, paths := newAnalyzeServer(t, true, repos...)
	events := make([]indexer.GraphChangeEvent, 0, len(repos))
	for _, r := range repos {
		events = append(events, indexer.GraphChangeEvent{
			FilePath:   filepath.Join(paths[r.name], "main.go"),
			RepoPrefix: r.name,
			Kind:       indexer.ChangeModified,
			NodesAdded: 1,
			Timestamp:  time.Now().Add(-time.Minute),
		})
	}
	srv.SetWatcher(historyTestWatcher{events: events})
	return srv, paths
}

func runRecentChanges(t *testing.T, srv *Server, ctx context.Context, args map[string]any) (string, string) {
	t.Helper()
	return runToolOK(t, srv, ctx, "get_recent_changes", args, srv.handleGetRecentChanges)
}

// TestGetRecentChanges_WorkspaceIsolation is the isolation invariant: the
// merged feed must be narrowed to the session's own workspace.
func TestGetRecentChanges_WorkspaceIsolation(t *testing.T) {
	srv, paths := recentChangesServer(t,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	text, applied := runRecentChanges(t, srv, sessionCtx("s-alpha", paths["repo-a"]), nil)

	assert.Contains(t, text, "repo-a", "alpha session must still see its own file churn")
	assert.NotContains(t, text, "repo-b",
		"alpha-bound session leaked beta's file churn — cross-workspace isolation breach")
	assert.NotEmpty(t, applied, "every scoped response must disclose scope_applied")
}

// TestGetRecentChanges_SinceBranchIsClamped guards the `since` path
// independently: the handler used to render it in a second, duplicated
// loop, so a clamp applied to only one branch would have looked correct.
func TestGetRecentChanges_SinceBranchIsClamped(t *testing.T) {
	srv, paths := recentChangesServer(t,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)

	since := time.Now().Add(-time.Hour).Format(time.RFC3339)
	text, _ := runRecentChanges(t, srv, sessionCtx("s-alpha", paths["repo-a"]),
		map[string]any{"since": since})

	assert.Contains(t, text, "repo-a", "the since branch must still return in-workspace rows")
	assert.NotContains(t, text, "repo-b", "the since branch must be clamped like the full-history branch")
}

// TestGetRecentChanges_RepoNarrows pins the explicit selector inside a
// single workspace holding two repos.
func TestGetRecentChanges_RepoNarrows(t *testing.T) {
	srv, paths := recentChangesServer(t,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)
	ctx := sessionCtx("s-shared", paths["repo-a"])

	wide, _ := runRecentChanges(t, srv, ctx, nil)
	require.Contains(t, wide, "repo-b", "both repos share one workspace, so both start visible")

	byName, applied := runRecentChanges(t, srv, ctx, map[string]any{"repo": "repo-a"})
	assert.Contains(t, byName, "repo-a")
	assert.NotContains(t, byName, "repo-b", "an explicit repo selector must narrow the feed")
	assert.Equal(t, "repo:repo-a", applied)

	byPath, _ := runRecentChanges(t, srv, ctx, map[string]any{"repo": paths["repo-a"]})
	// Compare the narrowed feed, not the whole response: graph_current_as_of
	// is the wall-clock read time at RFC3339 second precision, so two calls
	// straddling a second boundary differ there even when they narrow
	// identically.
	assert.Equal(t, recentChangesRows(t, byName), recentChangesRows(t, byPath),
		"the tracked-name and path forms of repo must narrow identically")
}

// recentChangesRows decodes a get_recent_changes response and returns its
// `changes` rows, dropping the per-call graph_current_as_of read stamp.
func recentChangesRows(t *testing.T, text string) []any {
	t.Helper()
	var resp struct {
		Changes          []any  `json:"changes"`
		GraphCurrentAsOf string `json:"graph_current_as_of"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp), "response: %s", text)
	require.NotEmpty(t, resp.GraphCurrentAsOf, "every response must carry its read stamp")
	return resp.Changes
}

// TestGetRecentChanges_RowsCarryRepoAttribution pins the new field. Without
// it a merged row is unattributable: FilePath is absolute and
// GraphChangeEvent carried nothing naming the repo it came from.
func TestGetRecentChanges_RowsCarryRepoAttribution(t *testing.T) {
	srv, paths := recentChangesServer(t,
		analyzeRepoSpec{name: "repo-a", workspace: "shared-ws", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "shared-ws", body: structuralBody("b")},
	)

	text, _ := runRecentChanges(t, srv, sessionCtx("s-shared", paths["repo-a"]), nil)

	assert.Contains(t, text, `"repo":"repo-a"`)
	assert.Contains(t, text, `"repo":"repo-b"`)
}

// TestGetRecentChanges_UnboundSessionUnchanged is the compatibility fence
// for the single-repo `gortex mcp --watch` path and for CLI / control
// clients, which carry no session cwd: they must still see the full feed,
// and a single-repo watcher's unprefixed rows must not be filtered away.
func TestGetRecentChanges_UnboundSessionUnchanged(t *testing.T) {
	srv, paths := recentChangesServer(t,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
		analyzeRepoSpec{name: "repo-b", workspace: "beta", body: structuralBody("b")},
	)
	srv.SetWatcher(historyTestWatcher{events: []indexer.GraphChangeEvent{
		{FilePath: filepath.Join(paths["repo-a"], "main.go"), Kind: indexer.ChangeModified, Timestamp: time.Now()},
		{FilePath: filepath.Join(paths["repo-b"], "main.go"), Kind: indexer.ChangeModified, Timestamp: time.Now()},
	}})

	text, _ := runRecentChanges(t, srv, context.Background(), nil)

	assert.Contains(t, text, "repo-a")
	assert.Contains(t, text, "repo-b", "an unbound session must not be clamped")
	assert.NotContains(t, text, `"repo":`, "an unprefixed row must keep its original shape")
}

// TestGetRecentChanges_NoWatcherStillErrors keeps the pre-existing contract:
// the watch-mode check must run before any scope work.
func TestGetRecentChanges_NoWatcherStillErrors(t *testing.T) {
	srv, paths := newAnalyzeServer(t, true,
		analyzeRepoSpec{name: "repo-a", workspace: "alpha", body: structuralBody("a")},
	)

	res, err := srv.handleGetRecentChanges(sessionCtx("s-alpha", paths["repo-a"]),
		makeReq("get_recent_changes", nil))
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Equal(t, "watch mode is not active", toolResultText(res))
}
