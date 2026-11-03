package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func TestPendingTextSearchReadsCurrentBytesWithoutWatcherCacheSideEffects(t *testing.T) {
	for _, scenario := range []struct {
		name                 string
		refused, readRefused bool
		deleted              bool
	}{{"projected", false, false, false}, {"projection explicitly disabled", true, false, false}, {"projected but current read refused", false, true, false}, {"projection disabled and current read refused", true, true, false}, {"projected then deleted", false, false, true}, {"projection disabled then deleted", true, false, true}} {
		refused, name := scenario.refused, scenario.name
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			if refused {
				t.Setenv("GORTEX_PROJECTION", "0")
			} else {
				t.Setenv("GORTEX_PROJECTION", "1")
			}
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			original := []byte("package main\n\nfunc OldPendingText() {}\n")
			const unique = "func UnannouncedQuartzZebra("
			require.NoError(t, os.WriteFile(path, original, 0o600))
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			_, err = idx.Index(root)
			require.NoError(t, err)
			for range 4 {
				require.Len(t, idx.GrepText("func OldPendingText(", 10), 1)
			}
			// Prove cached postings, rather than a streaming fallback, are in use.
			require.NoError(t, os.WriteFile(path, []byte("package main\n\n"+unique+") {}\n"), 0o600))
			require.Empty(t, idx.GrepText(unique, 10))
			require.NoError(t, os.WriteFile(path, original, 0o600))
			server := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
			done := make(chan indexer.MutationResult, 1)
			t.Cleanup(func() { close(done) })
			// A held accepted publication ticket is sufficient. Search correctness
			// cannot depend on one watcher implementation also patching trigram state.
			server.SetWatcher(mutationTestWatcher{done: done})
			edit := callToolByName(t, server, t.Context(), "edit_file", map[string]any{"path": path, "old_string": "OldPendingText", "new_string": "UnannouncedQuartzZebra", "format": "json"})
			require.False(t, edit.IsError, "%v", edit.Content)
			ack := extractTextResult(t, edit)
			require.Equal(t, "local", ack["ack_mode"])
			view := ack["file_view"].(map[string]any)
			if refused {
				require.Equal(t, "unavailable", view["state"])
				require.Equal(t, "disabled", view["reason"])
			} else {
				require.Equal(t, "projected", view["state"])
			}
			if scenario.readRefused {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			if scenario.deleted {
				require.NoError(t, os.Remove(path))
			}
			result := callToolByName(t, server, t.Context(), "search_text", map[string]any{"query": unique, "limit": 100, "max_bytes": 0, "format": "json"})
			if scenario.readRefused {
				require.True(t, result.IsError, "a refused current-byte read must not fall back to canonical text: %v", result.Content)
				return
			}
			require.False(t, result.IsError, "%v", result.Content)
			rows, _ := extractTextResult(t, result)["matches"].([]any)
			old := callToolByName(t, server, t.Context(), "search_text", map[string]any{"query": "func OldPendingText(", "limit": 100, "max_bytes": 0, "format": "json"})
			require.False(t, old.IsError, "%v", old.Content)
			oldRows, _ := extractTextResult(t, old)["matches"].([]any)
			require.Empty(t, oldRows, "old canonical postings must not survive the same pending physical write")
			if scenario.deleted {
				require.Empty(t, rows, "actual absence must mask both the pending text and old postings")
				return
			}
			require.Len(t, rows, 1, "first search must scan the physical pending file even when no projection was admitted")
			row := rows[0].(map[string]any)
			require.Equal(t, "main.go", row["path"])
			require.Equal(t, float64(3), row["line"])
			require.Contains(t, row["text"], unique)
			require.Nil(t, store.GetNode("main.go::UnannouncedQuartzZebra"))
			require.NotNil(t, store.GetNode("main.go::OldPendingText"))
		})
	}
}

func TestPendingTextSearchIncludesActualUnsupportedAndOversizeProjections(t *testing.T) {
	for _, scenario := range []struct{ name, path, content string }{{"unsupported language", "notes.unknown", "first line\nUniquePendingUnsupportedLiteral\n"}, {"projection size cap", "large.go", "package main\n// UniquePendingUnsupportedLiteral\n" + strings.Repeat("// source padding\n", 70000)}} {
		t.Run(scenario.name, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "helper.go"), []byte("package main\nfunc Existing() {}\n"), 0o600))
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			_, err = idx.Index(root)
			require.NoError(t, err)
			for range 4 {
				require.Len(t, idx.GrepText("func Existing(", 10), 1)
			}
			server := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
			done := make(chan indexer.MutationResult, 1)
			t.Cleanup(func() { close(done) })
			server.SetWatcher(mutationTestWatcher{done: done})
			result := callToolByName(t, server, t.Context(), "write_file", map[string]any{"path": filepath.Join(root, scenario.path), "content": scenario.content, "format": "json"})
			require.False(t, result.IsError, "%v", result.Content)
			ack := extractTextResult(t, result)
			require.Equal(t, "local", ack["ack_mode"])
			view := ack["file_view"].(map[string]any)
			require.Equal(t, "unavailable", view["state"])
			require.NotEmpty(t, view["reason"])
			result = callToolByName(t, server, t.Context(), "search_text", map[string]any{"query": "UniquePendingUnsupportedLiteral", "limit": 10, "format": "json"})
			require.False(t, result.IsError, "%v", result.Content)
			rows, _ := extractTextResult(t, result)["matches"].([]any)
			require.Len(t, rows, 1)
			require.Equal(t, scenario.path, rows[0].(map[string]any)["path"])
			require.Equal(t, float64(2), rows[0].(map[string]any)["line"])
		})
	}
}

func TestPendingTextAbsenceRequiresAnOwnedMissingPath(t *testing.T) {
	for _, mode := range []string{"missing leaf", "missing directory", "broken final link", "escaping parent", "broken parent"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "sub")
			path := filepath.Join(parent, "main.go")
			require.NoError(t, os.Mkdir(parent, 0o700))
			require.NoError(t, os.WriteFile(path, []byte("package main\nfunc BeforeDeletion() {}\n"), 0o600))
			idx := indexer.New(graph.New(), testRegistry(), config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			_, err := idx.Index(root)
			require.NoError(t, err)
			require.NoError(t, os.Remove(path))
			if mode != "missing leaf" && mode != "broken final link" {
				require.NoError(t, os.Remove(parent))
			}
			switch mode {
			case "broken final link":
				if err := os.Symlink(filepath.Join(root, "absent.go"), path); err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			case "escaping parent", "broken parent":
				target := t.TempDir()
				if mode == "broken parent" {
					target = filepath.Join(target, "absent")
				}
				if err := os.Symlink(target, parent); err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			}
			server := &Server{}
			rows, excluded, err := server.scanPendingTextFiles(t.Context(), []pendingTextFile{{abs: path, path: "sub/main.go", relative: "sub/main.go", indexer: idx}}, "BeforeDeletion", nil, 10)
			if mode != "missing leaf" && mode != "missing directory" {
				require.Error(t, err, "an unsafe unresolved path is not evidence of deletion")
				return
			}
			require.NoError(t, err)
			require.Empty(t, rows)
			require.Contains(t, excluded[idx.RepoPrefix()], "sub/main.go", "actual absence must still hide old postings")
		})
	}
}

func TestPendingTextScopeAndPhysicalRootPrecedeAnyRead(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	selected, root := ackModeServer(t, "local")
	sibling, siblingRoot := ackModeServer(t, "local")
	searchProjectedAck(t, selected, root, "SelectedText")
	searchProjectedAck(t, sibling, siblingRoot, "ForeignText")
	// A foreign same-prefix registry entry must neither leak its content nor
	// poison the selected repository when its current-byte read is refused.
	foreign := filepath.Join(siblingRoot, "main.go")
	require.NoError(t, os.Remove(foreign))
	require.NoError(t, os.Mkdir(foreign, 0o700))
	result := callToolByName(t, selected, t.Context(), "search_text", map[string]any{"query": "ForeignText", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	rows, _ := extractTextResult(t, result)["matches"].([]any)
	require.Empty(t, rows)
	own := filepath.Join(root, "main.go")
	require.NoError(t, os.Remove(own))
	require.NoError(t, os.Mkdir(own, 0o700))
	result = callToolByName(t, selected, t.Context(), "search_text", map[string]any{"query": "SelectedText", "path": "unrelated.go", "format": "json"})
	require.False(t, result.IsError, "scope must exclude the pending read before I/O: %v", result.Content)
	result = callToolByName(t, selected, t.Context(), "search_text", map[string]any{"query": "SelectedText", "format": "json"})
	require.True(t, result.IsError, "the same owned current-byte read really is refused")
}

func TestPendingTextMultiRepoMergeKeepsUncoveredHitsAndAppliesLimitLast(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	alpha := setupNestedRepo(t, "alpha", "shared", "package sub\n\nfunc AlphaBefore() {}\n")
	beta := setupNestedRepo(t, "beta", "shared", "package sub\n\nfunc Beta() { println(\"BetaNeedle\") }\n")
	original, _ := nestedRepoServer(t, []config.RepoEntry{{Path: alpha, Name: "alpha", Project: "backend"}, {Path: beta, Name: "beta", Project: "backend"}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, original.multiIndexer.Close(ctx))
	})
	server := NewServer(original.engine, original.graph, original.indexer, nil, zap.NewNop(), nil, MultiRepoOptions{ConfigManager: original.configManager, MultiIndexer: original.multiIndexer, EditAckMode: config.EditAckLocal})
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	server.SetWatcher(mutationTestWatcher{done: done})
	path := filepath.Join(alpha, "pkg", "sub", "main.go")
	result := callToolByName(t, server, t.Context(), "edit_file", map[string]any{"path": path, "old_string": "func AlphaBefore() {}", "new_string": "func AlphaAfter() { println(\"AlphaNeedle\") }", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	require.Equal(t, "local", extractTextResult(t, result)["ack_mode"])
	require.Nil(t, server.graph.GetNode("alpha/pkg/sub/main.go::AlphaAfter"), "the admitted publication remains held")
	for _, regexp := range []bool{false, true} {
		result = callToolByName(t, server, t.Context(), "search_text", map[string]any{"query": "Needle", "regexp": regexp, "repo": "*", "limit": 1, "format": "json"})
		require.False(t, result.IsError, "%v", result.Content)
		rows, _ := extractTextResult(t, result)["matches"].([]any)
		require.Len(t, rows, 1)
		require.Equal(t, "alpha/pkg/sub/main.go", rows[0].(map[string]any)["path"], "the new pending hit sorts before uncovered beta and must survive the final cut")
	}
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	result = callToolByName(t, server, t.Context(), "search_text", map[string]any{"query": "BetaNeedle", "repo": "beta", "format": "json"})
	require.False(t, result.IsError, "a different repository's refused pending read cannot poison this scope: %v", result.Content)
	rows, _ := extractTextResult(t, result)["matches"].([]any)
	require.Len(t, rows, 1)
	require.Equal(t, "beta/pkg/sub/main.go", rows[0].(map[string]any)["path"])
}
