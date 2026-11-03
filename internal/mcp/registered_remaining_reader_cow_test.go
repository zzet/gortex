package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

type remainingReaderCOWStore struct {
	*store_sqlite.Store
	armed    atomic.Bool
	arrivals atomic.Int32
	captured chan *projectionEntry
	release  [2]chan struct{}
	path     string
}

func (s *remainingReaderCOWStore) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	return s.FileMetasByPathsContext(context.Background(), prefix, paths)
}

func (s *remainingReaderCOWStore) FileMetasByPathsContext(ctx context.Context, prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	rows, err := s.Store.FileMetasByPathsContext(ctx, prefix, paths)
	ordinal := int32(0)
	if err == nil && s.armed.Load() {
		ordinal = s.arrivals.Add(1)
	}
	if ordinal > 0 && ordinal <= 2 {
		s.captured <- liveEntry(pendingFileProjections, s.path)
		<-s.release[ordinal-1]
	}
	return rows, err
}

// The actual daemon admits independent Fast requests from one connection.
// Both reads see the same already-observed source event and capture the
// same ACK owner before any same-byte read-COW publication can win its CAS.
func TestRemainingRegisteredReadersKeepCurrentRangesOnCOWLoss(t *testing.T) {
	for _, tool := range []string{"get_symbol", "get_cfg", "read_file"} {
		t.Run(tool, func(t *testing.T) {
			selectedProjectionRegistry(t)
			require.True(t, tokens.EncoderReady())
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
			sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cow.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sql.Close()) })
			backend := &remainingReaderCOWStore{Store: sql, captured: make(chan *projectionEntry, 2), release: [2]chan struct{}{make(chan struct{}), make(chan struct{})}, path: path}
			idx := newProjectionTestIndexer(t, backend)
			_, err = idx.Index(root)
			require.NoError(t, err)
			s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
			s.editAckMode = config.EditAckLocal
			pending := make(chan indexer.MutationResult)
			s.SetWatcher(mutationTestWatcher{done: pending})
			var receipt *mutationReceipt
			t.Cleanup(func() {
				close(pending)
				if receipt != nil {
					select {
					case <-receipt.done:
					case <-time.After(3 * time.Second):
						t.Error("owned held ticket did not join")
					}
				}
			})
			const session = "same-owner-read-cow"
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
			require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"registered-readback","version":"1"}}}`)))
			s.NoteSessionToolPolicy(session, "all", "defer")
			const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
			ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_file", map[string]any{"path": path, "old_string": "func Target() int { return 1 }", "new_string": body, "format": "json"})
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, "local", fields["ack_mode"])
			require.Equal(t, true, fields["readback_exact"])
			value, ok := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
			require.True(t, ok)
			receipt = value.(*mutationReceipt)
			entry := liveEntry(pendingFileProjections, path)
			require.NotNil(t, entry)
			require.Positive(t, entry.commitSeq)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "the independent canonical rows precede this actual local ACK")
			oldCtx, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), entry)
			require.NoError(t, err)
			pendingFileProjections.markSuspect(path)
			require.True(t, entry.suspect.Load())
			require.ErrorIs(t, validateRequestProjections(oldCtx), projectionSuperseded)
			originalDeadline := time.Now().Add(projectedReadBudget)
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, originalDeadline)
			backend.armed.Store(true)

			releaseFirst := sync.OnceFunc(func() { close(backend.release[0]) })
			releaseSecond := sync.OnceFunc(func() { close(backend.release[1]) })
			release := func() { releaseFirst(); releaseSecond() }
			type answer struct {
				tool   string
				result *mcplib.CallToolResult
			}
			answers := make(chan answer, 2)
			var workers sync.WaitGroup
			t.Cleanup(func() { release(); workers.Wait() })
			start := func(id int, name string, args map[string]any) {
				workers.Add(1)
				go func() {
					defer workers.Done()
					answers <- answer{name, preselectionRegisteredCall(t, s, ctx, id, name, args)}
				}()
			}
			capture := func() {
				select {
				case captured := <-backend.captured:
					require.Same(t, entry, captured)
				case <-time.After(3 * time.Second):
					t.Fatal("registered read did not reach actual pre-CAS metadata barrier")
				}
			}
			start(20, "get_symbol_source", map[string]any{"id": "main.go::Target", "context_lines": 0, "format": "json"})
			capture()
			args := map[string]any{"id": "main.go::Neighbor", "detail": "full", "format": "json"}
			if tool == "read_file" {
				args = map[string]any{"path": "main.go", "compress_bodies": true, "keep": "Target", "format": "json"}
			}
			start(21, tool, args)
			capture()
			releaseFirst()
			var first answer
			select {
			case first = <-answers:
			case <-time.After(3 * time.Second):
				t.Fatal("winning source did not join")
			}
			require.Equal(t, "get_symbol_source", first.tool)
			require.False(t, first.result.IsError, resultText(first.result))
			require.Equal(t, body, extractTextResult(t, first.result)["source"])
			releaseSecond()
			// Both releases are already closed; cleanup must not close them twice.
			release = func() {}
			var candidate answer
			select {
			case candidate = <-answers:
			case <-time.After(3 * time.Second):
				t.Fatal("losing reader did not join")
			}
			require.Equal(t, tool, candidate.tool)
			workers.Wait()
			winner := liveEntry(pendingFileProjections, path)
			require.NotNil(t, winner)
			require.NotSame(t, entry, winner)
			require.Zero(t, winner.commitSeq)
			require.Same(t, entry.layer, winner.layer)
			require.True(t, entry.suspect.Load())
			require.ErrorIs(t, validateRequestProjections(oldCtx), projectionSuperseded, "another read cannot revive the old context")
			require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "no canonical publication or fabricated Done result changed this control")
			t.Logf("remaining original whole-call allowance=%s", time.Until(originalDeadline))

			t.Logf("actual COW loser tool=%s result=%s", tool, resultText(candidate.result))
			switch tool {
			case "get_symbol":
				require.False(t, candidate.result.IsError, resultText(candidate.result))
				node := extractTextResult(t, candidate.result)["node"].(map[string]any)
				require.EqualValues(t, 8, node["start_line"], "full symbol metadata must not return canonical old5 after losing the actual CAS")
				require.EqualValues(t, 8, node["end_line"])
			case "get_cfg":
				require.True(t, candidate.result.IsError, "deferred source must refuse instead of building CFG from old ranges")
				require.Contains(t, resultText(candidate.result), "source is pending")
			case "read_file":
				require.False(t, candidate.result.IsError, resultText(candidate.result))
				decoded := extractTextResult(t, candidate.result)
				require.Equal(t, "pending", decoded["compression_state"])
				require.Equal(t, string(raw), decoded["content"], "pending keep-range proof must retain actual current raw bytes")
			}
		})
	}
}
