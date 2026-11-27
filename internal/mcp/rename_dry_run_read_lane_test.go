package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// renameLaneFixture indexes target.go and caller.go into a SQLite store, so a
// rename that writes leaves bytes on disk, a mutation receipt and WAL frames.
func renameLaneFixture(t *testing.T, facade bool) (*Server, *store_sqlite.Store, context.Context, string) {
	t.Helper()
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_TOOLS", "full")
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "target.go"), []byte(renameTargetSrc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"), []byte(renameCallerSrc), 0o644))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "rename-lane.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	idx.ResolveAll()
	s := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	const session = "rename-lane"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
	if facade {
		s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	} else {
		s.NoteSessionToolPolicy(session, "full", "defer")
	}
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"rename-lane-test","version":"1"}}}`)))
	s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	return s, store, ctx, root
}

// A dry-run rename is admitted as a read, so it must not write: no source
// bytes, no mutation receipt and no store transaction. A rename that writes
// stays in the mutating lane. Each spelling is classified by the dispatcher
// and then run through the registered tool, and the two must agree.
func TestDryRunRenameIsAdmittedAsAReadAndWritesNothing(t *testing.T) {
	target := map[string]any{"symbol": "target.go::Target"}
	cases := []struct {
		name   string
		tool   string
		args   map[string]any
		writes bool
	}{
		{"legacy dry run", "rename_symbol", map[string]any{"id": "target.go::Target", "new_name": "Renamed", "dry_run": true}, false},
		{"facade dry run", "refactor", map[string]any{"operation": "rename", "target": target, "new_name": "Renamed", "dry_run": true}, false},
		{"facade dry run in options", "refactor", map[string]any{"operation": "rename", "target": target, "new_name": "Renamed", "options": map[string]any{"dry_run": true}}, false},
		{"facade dry run as text", "refactor", map[string]any{"operation": "rename", "target": target, "new_name": "Renamed", "dry_run": "true"}, false},
		{"legacy apply", "rename_symbol", map[string]any{"id": "target.go::Target", "new_name": "Renamed"}, true},
		{"facade apply", "refactor", map[string]any{"operation": "rename", "target": target, "new_name": "Renamed", "dry_run": false}, true},
		{"facade apply overrides options", "refactor", map[string]any{"operation": "rename", "target": target, "new_name": "Renamed", "dry_run": false, "options": map[string]any{"dry_run": true}}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			facade := test.tool == "refactor"
			s, store, ctx, root := renameLaneFixture(t, facade)
			wantClass := daemon.MCPDispatchFast
			if test.writes {
				wantClass = daemon.MCPDispatchMutating
			}
			// The registered tool gets the spelling plus the output format, so
			// the lane is checked on that exact map as well as on the spelling.
			args := map[string]any{"format": "json"}
			if facade {
				args = map[string]any{"output": map[string]any{"format": "json"}}
			}
			for key, value := range test.args {
				args[key] = value
			}
			require.Equal(t, wantClass, s.DispatchClass(test.tool, test.args), "dispatch lane")
			require.Equal(t, wantClass, s.DispatchClass(test.tool, args), "dispatch lane of the arguments the tool runs with")

			stat := func(name string) os.FileInfo {
				info, err := os.Stat(filepath.Join(root, name))
				require.NoError(t, err)
				return info
			}
			receipts := func() float64 {
				tool, args := "mutation_status", map[string]any{"format": "json"}
				if facade {
					tool, args = "change", map[string]any{"operation": "receipt", "output": map[string]any{"format": "json"}}
				}
				listing := extractTextResult(t, snapshotRegisteredCall(t, s, ctx, 3, tool, args))
				count, ok := listing["count"].(float64)
				require.True(t, ok, "mutation_status listing has no count: %v", listing)
				return count
			}
			targetBefore, callerBefore := stat("target.go"), stat("caller.go")
			receiptsBefore := receipts()
			walBefore := store.WALWriteMark()
			require.True(t, walBefore.Valid, "the store must expose its WAL position")

			result := snapshotRegisteredCall(t, s, ctx, 2, test.tool, args)
			require.False(t, result.IsError, resultText(result))
			body := extractTextResult(t, result)

			wal := store_sqlite.WALWrittenBetween(walBefore, store.WALWriteMark())
			require.True(t, wal.Valid)
			targetBytes, err := os.ReadFile(filepath.Join(root, "target.go"))
			require.NoError(t, err)
			callerBytes, err := os.ReadFile(filepath.Join(root, "caller.go"))
			require.NoError(t, err)
			if test.writes {
				// The observers below see a real rename, so their silence on a
				// dry run means nothing was written.
				require.Equal(t, "applied", body["status"], "%v", body)
				require.Contains(t, string(targetBytes), "func Renamed() string")
				require.Contains(t, string(callerBytes), "return Renamed()")
				require.Greater(t, receipts(), receiptsBefore, "an applied rename records a mutation receipt")
				require.Positive(t, wal.Frames, "an applied rename reindexes into the store")
				return
			}
			require.Equal(t, "would_apply", body["status"], "%v", body)
			require.Equal(t, true, body["dry_run"])
			require.Equal(t, false, body["written"])
			require.Equal(t, renameTargetSrc, string(targetBytes))
			require.Equal(t, renameCallerSrc, string(callerBytes))
			require.Equal(t, targetBefore.ModTime(), stat("target.go").ModTime())
			require.Equal(t, callerBefore.ModTime(), stat("caller.go").ModTime())
			require.Equal(t, receiptsBefore, receipts(), "a dry run records no mutation receipt")
			require.Zero(t, wal.Frames, "a dry run writes no store transaction")
			require.False(t, wal.Reset)
		})
	}
}
