package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

type nativeCreateFreshnessExtractor struct {
	parser.Extractor
	name    string
	armed   atomic.Bool
	entered chan [sha256.Size]byte
	release chan struct{}
}

func (e *nativeCreateFreshnessExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	return e.ExtractWithOptions(path, raw, parser.ExtractionOptions{})
}

func (e *nativeCreateFreshnessExtractor) ExtractWithOptions(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	if filepath.Base(path) == e.name && e.armed.CompareAndSwap(true, false) {
		e.entered <- sha256.Sum256(raw)
		<-e.release
	}
	return parser.Extract(e.Extractor, path, raw, options)
}

func TestNativeCreateResolvesCallerAcrossMachineOutputs(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	const (
		caller    = "caller.go::Caller"
		unrelated = "caller.go::Unrelated"
		control   = "caller.go::Control"
		target    = "new_target.go::Target"
		name      = "new_target.go"
		created   = "package main\nfunc Target() {}\n"
	)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	path := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"),
		[]byte("package main\nfunc Control() {}\nfunc Caller() { Control(); Target() }\nfunc Unrelated() { Control() }\n"), 0600))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native-create.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	extractor := &nativeCreateFreshnessExtractor{
		Extractor: languages.NewGoExtractor(), name: name, entered: make(chan [sha256.Size]byte, 1), release: make(chan struct{}),
	}
	unblock := sync.OnceFunc(func() { close(extractor.release) })
	registry := parser.NewRegistry()
	registry.Register(extractor)
	logger := zap.NewNop()
	idx := indexer.New(db, registry, config.Default().Index, logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)

	s := NewServer(query.NewEngine(db), db, idx, nil, logger, nil)
	t.Cleanup(func() { s.DrainBackground(); s.queryLog.Close() })
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { unblock(); require.NoError(t, watcher.Stop()) })
	s.SetWatcher(watcher)
	const session = "native-create-freshness-machine"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	ctx, cancel := context.WithTimeout(s.WithClientSession(WithSessionID(t.Context(), session), session), 12*time.Second)
	defer cancel()
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"native-create-freshness","version":"1"}}}`)))

	stored := func(wantResolved bool) {
		t.Helper()
		exact, unresolved, controls := 0, 0, 0
		for _, edge := range db.GetOutEdges(caller) {
			if edge.Kind != graph.EdgeCalls {
				continue
			}
			switch {
			case edge.To == target:
				exact++
			case edge.To == control:
				controls++
			case strings.HasPrefix(edge.To, "unresolved::") && strings.HasSuffix(edge.To, "Target"):
				unresolved++
			}
		}
		require.Equal(t, 1, controls)
		if wantResolved {
			require.Equal(t, 1, exact)
			require.Zero(t, unresolved)
			require.NotNil(t, db.GetNode(target))
		} else {
			require.Zero(t, exact)
			require.Equal(t, 1, unresolved)
			require.Nil(t, db.GetNode(target))
		}
	}

	probe := func(id int, seed, phase, format string) {
		t.Helper()
		result, err := externalVersionRegisteredCall(s, ctx, id, "trace", map[string]any{
			"operation": "call_chain",
			"target":    map[string]any{"symbol": seed},
			"output":    map[string]any{"format": format, "max_bytes": 0},
		})
		require.NoError(t, err)
		require.False(t, result.IsError, "refusal does not qualify as an assessed relation reply")
		body, ok := singleTextContent(result)
		require.True(t, ok)
		var payload map[string]any
		var label string
		if format == "toon" {
			if strings.HasPrefix(body, "relations: ") {
				label, body, ok = strings.Cut(body, "\n")
				require.True(t, ok)
			}
			require.False(t, json.Valid([]byte(body)))
			decoded, err := toon.Decode([]byte(body), toon.WithStrictMode(true))
			require.NoError(t, err)
			payload, ok = decoded.(map[string]any)
			require.True(t, ok)
		} else {
			require.NoError(t, json.Unmarshal([]byte(body), &payload))
		}
		rider, _ := payload["freshness"].(map[string]any)
		rows, ok := payload["edges"].([]any)
		require.True(t, ok)
		exact, controls := 0, 0
		for _, value := range rows {
			edge, ok := value.(map[string]any)
			require.True(t, ok)
			if edge["from"] != seed || edge["kind"] != "calls" {
				continue
			}
			switch edge["to"] {
			case target:
				exact++
			case control:
				controls++
			}
		}
		require.Equal(t, 1, controls)
		require.Equal(t, false, payload["truncated"])
		state, _ := rider["graph_freshness"].(string)
		scope, _ := rider["freshness_scope"].(string)
		if state == "" {
			state, _ = payload["graph_freshness"].(string)
			scope, _ = payload["freshness_scope"].(string)
		}
		t.Logf("NATIVE_CREATE_RESOLUTION phase=%s format=%s edge=%d control=%d label=%q state=%q scope=%q",
			phase, format, exact, controls, label, state, scope)
		switch phase {
		case "before", "unrelated":
			require.Zero(t, exact)
			require.Empty(t, label)
			require.Empty(t, state)
		case "held":
			require.Contains(t, []int{0, 1}, exact)
			if exact == 0 {
				if format == "toon" {
					require.Regexp(t, `^relations: (pending|failed|unknown) \([0-9]+ files\)$`, label)
				} else {
					require.Contains(t, []string{"pending", "failed", "unknown"}, state)
					require.Equal(t, "relations", scope)
					files, _ := rider["pending_files"].([]any)
					if files == nil {
						files, _ = payload["pending_files"].([]any)
					}
					require.Equal(t, []any{name}, files)
				}
			}
			if exact == 0 || label != "" || (state != "" && state != "fresh") {
				for _, fields := range []map[string]any{payload, rider} {
					require.NotEqual(t, "fresh", fields["graph_freshness"])
					require.NotEqual(t, true, fields["fresh"])
				}
			}
		case "published":
			require.Equal(t, 1, exact)
		}
	}

	stored(false)
	probe(2, caller, "before", "json")
	probe(3, caller, "before", "toon")
	extractor.armed.Store(true)
	offered := time.Now()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	n, writeErr := f.WriteString(created)
	require.NoError(t, f.Close())
	require.NoError(t, writeErr)
	require.Equal(t, len(created), n)
	wantHash := sha256.Sum256([]byte(created))
	select {
	case received := <-extractor.entered:
		require.Equal(t, wantHash, received, "the first watcher extraction owns the exact created bytes")
	case <-ctx.Done():
		t.Fatal("native watcher did not reach the real extraction")
	}
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, wantHash, sha256.Sum256(actual))
	held := watcher.TierState(path)
	require.NotZero(t, held.Generation)
	require.True(t, held.RelationsAt.IsZero())
	require.False(t, held.Failed)
	t.Logf("NATIVE_CREATE_RESOLUTION held path=%q hash=%x pending_generation=%d", path, wantHash, held.Generation)
	for i, format := range []string{"json", "toon"} {
		stored(false)
		probe(4+i, caller, "held", format)
		probe(8+i, unrelated, "unrelated", format)
		after := watcher.TierState(path)
		require.Equal(t, held.Generation, after.Generation)
		require.True(t, after.RelationsAt.IsZero())
	}
	stored(false)
	unblock()
	for {
		select {
		case event, ok := <-watcher.Events():
			require.True(t, ok)
			if event.FilePath != path {
				continue
			}
			// Native create and write notifications can coalesce as modified.
			require.Contains(t, []indexer.ChangeKind{indexer.ChangeCreated, indexer.ChangeModified}, event.Kind)
			readback, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, wantHash, sha256.Sum256(readback))
			require.True(t, event.Timestamp.After(offered))
			require.Positive(t, event.NodesAdded)
			t.Logf("NATIVE_CREATE_RESOLUTION native_event=%s path=%q timestamp=%s nodes_added=%d edges_added=%d",
				event.Kind, event.FilePath, event.Timestamp.Format(time.RFC3339Nano), event.NodesAdded, event.EdgesAdded)
			stored(true)
			probe(6, caller, "published", "json")
			probe(7, caller, "published", "toon")
			return
		case <-ctx.Done():
			t.Fatal("native watcher did not publish the owned creation")
		}
	}
}

func TestNativePendingCallChainExplicitBaseKeepsCanonicalRoot(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	s := stack.srv
	const (
		name    = "base_pending_target.go"
		content = "package repo\nfunc BasePendingTarget() {}\n"
		caller  = "repo/base_pending_caller.go::BasePendingCaller"
		target  = "repo/base_pending_target.go::BasePendingTarget"
		control = "repo/base_pending_caller.go::BaseControl"
	)
	root, err := filepath.EvalSymlinks(stack.repoRoot)
	require.NoError(t, err)
	path := filepath.Join(root, name)
	callerPath := filepath.Join(root, "base_pending_caller.go")
	require.NoError(t, os.WriteFile(callerPath,
		[]byte("package repo\nfunc BaseControl() {}\nfunc BasePendingCaller() { BaseControl(); BasePendingTarget() }\n"), 0600))
	_, err = s.multiIndexer.IncrementalReindexRepo("repo", []string{callerPath})
	require.NoError(t, err)
	require.NotNil(t, stack.store.GetNode(caller))
	require.Nil(t, stack.store.GetNode(target))
	require.NotEmpty(t, stack.store.GetOutEdges(caller))
	idx := s.multiIndexer.GetIndexer("repo")
	require.NotNil(t, idx)
	gate := &nativeCreateFreshnessExtractor{
		Extractor: languages.NewGoExtractor(), name: name, entered: make(chan [sha256.Size]byte, 1), release: make(chan struct{}),
	}
	idx.Registry().Register(gate)
	unblock := sync.OnceFunc(func() { close(gate.release) })
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { unblock(); require.NoError(t, watcher.Stop()) })
	s.SetWatcher(watcher)

	const session = "native-create-explicit-base"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	ctx, cancel := context.WithTimeout(s.WithClientSession(WithSessionID(t.Context(), session), session), 12*time.Second)
	defer cancel()
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"native-create-explicit-base","version":"1"}}}`)))
	probe := func(id int, format string) string {
		t.Helper()
		args := baseSelectorArgs(stack.graphID, map[string]any{
			"operation": "call_chain", "target": map[string]any{"symbol": caller},
			"output": map[string]any{"format": format, "max_bytes": 0},
		})
		result, err := externalVersionRegisteredCall(s, ctx, id, "trace", args)
		require.NoError(t, err)
		require.False(t, result.IsError, "%s", resultText(result))
		body, ok := singleTextContent(result)
		require.True(t, ok)
		require.Contains(t, body, control)
		require.NotContains(t, body, target)
		return body
	}

	var baseline map[string]any
	require.NoError(t, json.Unmarshal([]byte(probe(2, "json")), &baseline))
	baselineRider, ok := baseline["freshness"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pending", baselineRider["graph_freshness"])
	require.Equal(t, float64(0), baselineRider["pending_file_count"])
	require.Empty(t, baselineRider["pending_files"])
	require.True(t, strings.HasPrefix(probe(3, "toon"), "relations: pending (0 files)\n"))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	gate.armed.Store(true)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(t, err)
	_, writeErr := f.WriteString(content)
	require.NoError(t, f.Close())
	require.NoError(t, writeErr)
	select {
	case received := <-gate.entered:
		require.Equal(t, sha256.Sum256([]byte(content)), received)
	case <-ctx.Done():
		t.Fatal("canonical watcher did not reach the new definition")
	}
	held := watcher.TierState(path)
	require.NotZero(t, held.Generation)
	require.True(t, held.RelationsAt.IsZero())
	var jsonBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(probe(4, "json")), &jsonBody))
	rider, ok := jsonBody["freshness"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pending", rider["graph_freshness"])
	require.Equal(t, "relations", rider["freshness_scope"])
	require.Equal(t, []any{"repo/" + name}, rider["pending_files"])
	require.True(t, strings.HasPrefix(probe(5, "toon"), "relations: pending (1 files)\n"))
	require.Equal(t, held.Generation, watcher.TierState(path).Generation)
	require.True(t, watcher.TierState(path).RelationsAt.IsZero())
	unblock()
}
