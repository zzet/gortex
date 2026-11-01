package mcp

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestRestartLostReceiptsReturnWithoutWaiting(t *testing.T) {
	for _, key := range []string{"receipt", "mutation_id"} {
		t.Run(key, func(t *testing.T) {
			s := newMutationServer()
			req := mcp.CallToolRequest{}
			req.Params.Name = "mutation_status"
			req.Params.Arguments = map[string]any{key: "mutation-100", "require_fresh": true,
				"wait_deadline": time.Now().Add(time.Minute).Format(time.RFC3339Nano)}
			res, err := s.handleMutationStatus(context.Background(), req)
			require.NoError(t, err)
			payload := resultJSON(t, res)
			require.Equal(t, "lost", payload["state"])
			require.Equal(t, false, payload["found"])
			require.Equal(t, true, payload["graph_status_terminal"])
			require.NotContains(t, payload, "graph_freshness")
		})
	}
}

func TestFreshnessFieldsHaveOneProducer(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	keys := map[string]bool{"graph_freshness": true, "file_graph_at": true, "relations_at": true, "derived_at": true}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "freshness_rider.go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				key, _ := strconv.Unquote(literal.Value)
				require.False(t, keys[key], "%s produces tier field %s outside the shared rider", name, key)
			}
			return true
		})
	}
}

type watchedTierFixture struct {
	mutationTestWatcher
	mu    sync.RWMutex
	paths map[string]indexer.MutationTierState
}

func (w *watchedTierFixture) TierPaths() map[string]indexer.MutationTierState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	copyPaths := make(map[string]indexer.MutationTierState, len(w.paths))
	for path, state := range w.paths {
		copyPaths[path] = state
	}
	return copyPaths
}

func TestRequireFreshWaitsForRelationsOnWatchedPrimary(t *testing.T) {
	s, root := newRelationFixture(t)
	path := filepath.Join(root, "caller.go")
	watcher := &watchedTierFixture{paths: map[string]indexer.MutationTierState{
		path: {Generation: 1, AckAt: time.Now(), FileGraphAt: time.Now()},
	}}
	s.SetWatcher(watcher)
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": targetID(t, s), "require_fresh": true, "freshness_scope": "relations"}
	finished := make(chan *mcp.CallToolResult, 1)
	go func() {
		finished <- s.awaitWatchedFreshness(t.Context(), req.Params.Name, req, takeRequestFreshness(&req))
	}()
	select {
	case <-finished:
		t.Fatal("file publication incorrectly certified pending relations")
	case <-time.After(35 * time.Millisecond):
	}
	watcher.mu.Lock()
	state := watcher.paths[path]
	state.RelationsAt = time.Now()
	watcher.paths[path] = state
	watcher.mu.Unlock()
	select {
	case refusal := <-finished:
		require.Nil(t, refusal, "relations are visible even while derived is unfinished")
	case <-time.After(time.Second):
		t.Fatal("relations publication did not release the primary wait")
	}
	require.Empty(t, s.freshnessFor(t.Context(), "relations", []string{targetID(t, s)}, nil).state)
	require.Equal(t, "pending", s.freshnessFor(t.Context(), "derived", []string{targetID(t, s)}, nil).state)
}

func TestFailedPatchLabelsOnlyItsFile(t *testing.T) {
	s, root := newRelationFixture(t)
	s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
		filepath.Join(root, "other.go"): {Generation: 1, Failed: true},
	}})
	require.Empty(t, s.freshnessFor(t.Context(), "relations", []string{targetID(t, s)}, nil).state)
	label := s.freshnessFor(t.Context(), "relations", nil, []string{"other.go"})
	require.Equal(t, "failed", label.state)
	require.Equal(t, []string{"other.go"}, label.files)
}

func TestFreshnessRiderLabelsEveryOutputMode(t *testing.T) {
	rider := map[string]any{"freshness_scope": "relations", "graph_freshness": "pending",
		"pending_file_count": 2, "pending_files": []string{"caller.go", "target.go"}}
	for _, body := range []string{"clean: checked 2 callers", "callers[1]{name}:\n  Caller", "[1,2]"} {
		res := decorateResultWithFreshness(mcp.NewToolResultText(body), rider)
		require.Equal(t, "relations: pending (2 files)\n"+body, resultText(res))
	}
	jsonResult := decorateResultWithFreshness(mcp.NewToolResultText(`{"ok":true}`), rider)
	require.Equal(t, "pending", resultJSON(t, jsonResult)["freshness"].(map[string]any)["graph_freshness"])
}

func TestMutationIDsAreEpochSeeded(t *testing.T) {
	// Counters beginning at one can reuse a previous process's receipt after a
	// restart, turning an explicitly lost write into an unrelated live write.
	const plausibleEpoch = uint64(1_000_000_000_000_000_000)
	require.Greater(t, mutationReceiptSequence.Load(), plausibleEpoch)
	require.Greater(t, mutationCommitSequence.Load(), plausibleEpoch)
}
