package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

// A file-backed WAL database permits the real checked writer to commit across
// the reader's SQL snapshot. Both the source and database use owned temporary
// files, matching the existing checked-scope fixture's concurrency model.
func newAssemblyRetirementFixture(t *testing.T) (*Server, *store_sqlite.Store, string) {
	t.Helper()
	require.True(t, tokens.EncoderReady())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "assembly.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newProjectionTestIndexer(t, store)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	return s, store, path
}

// The real SQL reader detects the writer revision before the leaf can assemble
// a successful result. The observer runs after that transaction has released
// its connection, so real IndexFile/checked retirement can complete without a
// nested acquisition of SQLite's sole read connection.
type assemblyRetirementStore struct {
	*store_sqlite.Store
	armed atomic.Bool
	write func() error
	after func(context.Context, error)
}

func (s *assemblyRetirementStore) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	if !s.armed.CompareAndSwap(true, false) {
		return s.Store.WithFileEditingContextReader(ctx, fn)
	}
	err := s.Store.WithFileEditingContextReader(ctx, func(ctx context.Context, rows graph.FileEditingContextRows) error {
		if err := fn(ctx, rows); err != nil {
			return err
		}
		if s.write != nil {
			return s.write()
		}
		return nil
	})
	s.after(ctx, err)
	return err
}

// This isolates an early-error ordering not covered by retirement after a
// successful leaf. It is a controlled component, not attribution of the four
// untraced historical six-repository refusals. No old SQL result or selected
// file fact becomes usable; only the existing checked transition can authorize
// a second complete attempt inside the ORIGINAL 150ms/two-attempt allowance.
func TestRegisteredMetadataEarlyScopeRetirementUsesExistingBoundedAttempt(t *testing.T) {
	for _, tool := range []string{"get_editing_context", "get_file_summary"} {
		for _, surface := range []string{"raw", "facade"} {
			transitions := []string{"unchanged", "canonical_retirement", "physical_peer_same_bytes", "changed_bytes", "source_event", "unchecked_eviction", "expired", "cancelled", "canonical_no_projection"}
			if tool == "get_editing_context" && surface == "facade" {
				transitions = append(transitions, "nested_context_true", "nested_output_true", "nested_arguments_true", "nested_context_malformed", "nested_output_malformed", "nested_arguments_malformed", "nested_context_override_false", "malformed_options_container", "options_true_override_false")
			}
			for _, transition := range transitions {
				t.Run(tool+"/"+surface+"/"+transition, func(t *testing.T) {
					selectedProjectionRegistry(t)
					s, sql, path := newAssemblyRetirementFixture(t)
					backend := &assemblyRetirementStore{Store: sql}
					s.graph, s.engine = backend, query.NewEngine(backend)
					pending := make(chan indexer.MutationResult)
					s.SetWatcher(mutationTestWatcher{done: pending})
					t.Cleanup(func() {
						close(pending)
						s.mutationReceipts.Range(func(_, value any) bool {
							select {
							case <-value.(*mutationReceipt).done:
							case <-time.After(3 * time.Second):
								t.Error("owned assembly publication did not join")
							}
							return true
						})
					})
					const session = "early-scope-retirement"
					require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
					t.Cleanup(func() { s.DisconnectSession(session) })
					base := s.WithClientSession(WithSessionID(t.Context(), session), session)
					s.NoteSessionToolPolicy(session, "all", "defer")
					if surface == "facade" {
						s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
					}
					require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"early-scope","version":"1"}}}`)))
					// Produce an actual locally acknowledged, not-yet-indexed file.
					ack, err := s.runAcknowledgedEdit(t.Context(), makeReq("edit_file", map[string]any{
						"path": path, "old_string": "func Target() int { return 1 }", "new_string": "func Target() int {\n\tvalue := 7\n\treturn value\n}", "format": "json",
					}), s.handleEditFile)
					require.NoError(t, err)
					require.False(t, ack.IsError, resultText(ack))
					entry := liveEntry(pendingFileProjections, path)
					require.NotNil(t, entry)
					retirement := s.canonicalProjectionRetirement(path)
					require.NotNil(t, retirement)
					physical, err := os.ReadFile(path)
					require.NoError(t, err)
					if transition == "canonical_no_projection" {
						require.NoError(t, s.indexer.IndexFile(path))
						retirement.run()
						require.Nil(t, liveEntry(pendingFileProjections, path))
					}
					caller, cancel := context.WithCancel(base)
					defer cancel()
					var deadline time.Time
					var onOwner func(func()) bool
					var first context.Context
					var observed error
					var remainingAtEntry, remainingAfterProducer time.Duration
					calls, callbacks := 0, 0
					captured, ok := s.facades.legacy(tool)
					require.True(t, ok)
					s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
						calls++
						if calls == 1 {
							first = ctx
							remainingAtEntry = time.Until(deadline)
						}
						return captured.handler(ctx, req)
					})
					if transition != "unchanged" {
						neighbor := sql.GetNode("main.go::Neighbor")
						require.NotNil(t, neighbor)
						backend.write = func() error {
							updated := *neighbor
							updated.Meta = maps.Clone(neighbor.Meta)
							if updated.Meta == nil {
								updated.Meta = make(map[string]any)
							}
							updated.Meta["assembly_revision"] = "actual checked writer"
							return sql.AddBatchChecked([]*graph.Node{&updated}, nil)
						}
					}
					backend.after = func(_ context.Context, scopeErr error) {
						onOwner(func() {
							callbacks++
							observed = scopeErr
							require.Equal(t, deadline, first.Value(projectedSourceDeadlineKey{}))
							if transition == "unchanged" {
								require.NoError(t, scopeErr)
								return
							}
							require.ErrorIs(t, scopeErr, graph.ErrContractProjectionStale, "RED requires a REAL revision refusal, not a budget/setup failure")
							require.NoError(t, first.Err())
							if transition == "canonical_no_projection" {
								require.False(t, s.hasRequestProjectionFact(first, "main.go"))
								return
							}
							require.True(t, s.hasRequestProjectionFact(first, "main.go"))
							switch transition {
							case "canonical_retirement", "expired", "cancelled", "nested_context_true", "nested_output_true", "nested_arguments_true", "nested_context_malformed", "nested_output_malformed", "nested_arguments_malformed", "nested_context_override_false", "malformed_options_container", "options_true_override_false":
								require.NoError(t, s.indexer.IndexFile(path))
								retirement.run()
								require.True(t, entry.checkedRetired.Load())
								require.False(t, entry.suspect.Load())
								require.Nil(t, liveEntry(pendingFileProjections, path))
								require.True(t, checkedProjectionChange(first))
							case "physical_peer_same_bytes", "changed_bytes":
								after := append([]byte(nil), physical...)
								if transition == "changed_bytes" {
									after = append(after, []byte("\nfunc ChangedPeer() {}\n")...)
								}
								held, release, lockErr := acquireMutationPath(t.Context(), path)
								require.NoError(t, lockErr)
								_, commitErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, after, 0600)
								release()
								require.NoError(t, commitErr)
							case "source_event":
								pendingFileProjections.markSuspect(path)
							case "unchecked_eviction":
								pendingFileProjections.drop(entry)
							}
							require.ErrorIs(t, validateRequestProjections(first), projectionSuperseded)
							remainingAfterProducer = time.Until(deadline)
							require.True(t, time.Now().Before(deadline), "controlled producer work must fit ORIGINAL150ms")
							if transition == "expired" {
								timer := time.NewTimer(time.Until(deadline))
								defer timer.Stop()
								<-timer.C
							}
							if transition == "cancelled" {
								cancel()
							}
						})
					}
					backend.armed.Store(true)
					joinCancelledWorker := func() {
						if transition == "cancelled" {
							require.Eventually(t, func() bool { return !foregroundMCPToolCallActiveForTest() }, 3*time.Second, time.Millisecond, "join the canceled registered worker before restoring fixture state")
						}
					}
					t.Cleanup(joinCancelledWorker)
					name := tool
					args := map[string]any{"path": "main.go", "format": "json", "max_bytes": 0}
					if surface == "facade" {
						name = "read"
						operation := "editing_context"
						if tool == "get_file_summary" {
							operation = "summary"
						}
						args = map[string]any{"operation": operation, "target": map[string]any{"file": "main.go"}, "output": map[string]any{"format": "json", "max_bytes": 0}}
						if strings.HasPrefix(transition, "nested_") {
							envelope := strings.Split(transition, "_")[1]
							values, ok := args[envelope].(map[string]any)
							if !ok {
								values = make(map[string]any)
								args[envelope] = values
							}
							values["compress_bodies"] = true
							if strings.HasSuffix(transition, "_malformed") {
								values["compress_bodies"] = "not a boolean"
							}
							if transition == "nested_context_override_false" {
								args["compress_bodies"] = false
							}
						}
						if transition == "malformed_options_container" {
							args["options"] = "not an object"
						}
						if transition == "options_true_override_false" {
							args["options"] = map[string]any{"compress_bodies": true}
							args["compress_bodies"] = false
						}
					}
					// Registration, canonical setup and argument construction are
					// fixture work, not request execution. Start the one original
					// allowance only when this registered request is dispatched.
					deadline = time.Now().Add(projectedReadBudget)
					caller = context.WithValue(caller, projectedSourceDeadlineKey{}, deadline)
					result, callErr := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
						onOwner = dispatch
						return externalVersionRegisteredCall(s, caller, 20, name, args)
					})
					joinCancelledWorker()
					attempts := int32(-1)
					if first != nil {
						if allowance, ok := first.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); ok {
							attempts = allowance.attempts.Load()
						}
					}
					t.Logf("tool=%s surface=%s transition=%s leaf_calls=%d attempts=%d remaining_at_entry=%s remaining_after_producer=%s remaining_at_return=%s scope_error=%v", tool, surface, transition, calls, attempts, remainingAtEntry, remainingAfterProducer, time.Until(deadline), observed)
					if transition == "malformed_options_container" {
						require.Zero(t, callbacks, "malformed facade containers must refuse before SQL assembly")
						require.Zero(t, calls)
						require.Nil(t, first)
						if callErr != nil {
							require.ErrorContains(t, callErr, "-32602")
						} else {
							require.NotNil(t, result)
							require.True(t, result.IsError)
						}
						return
					}
					require.Equal(t, 1, callbacks)
					require.NotNil(t, first)
					if transition == "cancelled" {
						require.ErrorIs(t, caller.Err(), context.Canceled)
						// The SDK may translate cancellation to a JSON-RPC error.
						require.True(t, callErr != nil || result == nil || result.IsError)
					} else {
						require.NoError(t, callErr)
						require.NotNil(t, result)
						if transition == "unchanged" || transition == "canonical_retirement" || transition == "nested_context_override_false" {
							require.False(t, result.IsError, "checked retirement before leaf success must use the existing fresh attempt: %s", resultText(result))
							var body map[string]any
							require.NoError(t, json.Unmarshal([]byte(resultText(result)), &body))
							require.NotEmpty(t, body)
							require.Contains(t, resultText(result), "Target")
							require.Contains(t, resultText(result), "Neighbor")
							expectedCalls := 1
							if transition == "canonical_retirement" || transition == "nested_context_override_false" {
								expectedCalls = 2
								require.Equal(t, 8, sql.GetNode("main.go::Neighbor").StartLine)
							}
							require.Equal(t, expectedCalls, calls)
						} else {
							require.True(t, result.IsError, "unproved/physical transitions must not gain replay")
							require.Equal(t, 1, calls)
						}
					}
					if transition != "unchanged" && transition != "canonical_no_projection" {
						require.True(t, errors.Is(observed, graph.ErrContractProjectionStale))
						require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(first)), projectionSuperseded, "completion cannot revive the old facts")
					}
				})
			}
		}
	}
}
