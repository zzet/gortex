package mcp

import (
	"context"
	"crypto/sha256"
 "encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
 "github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

type externalScopeObservation struct {
	err              error
	writeErr         error
	originalDeadline time.Time
	remainingAtEntry time.Duration
	elapsed          time.Duration
	entry            *projectionEntry
}

type externalRevisionScopeStore struct {
	*store_sqlite.Store
	armed    atomic.Bool
	calls    atomic.Int32
	mu       sync.Mutex
	observed externalScopeObservation
	mutate   func(context.Context) error
}

// The actual SQL scope must detect a checked writer revision across its real
// read transaction. No stale sentinel is fabricated, and no errorless reader
// substitutes for the checked bundle. All assertions stay on the test owner.
func (s *externalRevisionScopeStore) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	if !s.armed.CompareAndSwap(true, false) {
		return s.Store.WithFileEditingContextReader(ctx, fn)
	}
	s.calls.Add(1)
	started := time.Now()
	deadline, _ := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	observed := externalScopeObservation{originalDeadline: deadline, remainingAtEntry: time.Until(deadline)}
	if facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts); ok {
		facts.mu.RLock()
		for _, entry := range facts.entries {
			if entry.graphPath == "main.go" {
				observed.entry = entry
			}
		}
		facts.mu.RUnlock()
	}
	err := s.Store.WithFileEditingContextReader(ctx, func(ctx context.Context, rows graph.FileEditingContextRows) error {
		if err := fn(ctx, rows); err != nil {
			return err
		}
		observed.writeErr = s.mutate(ctx)
		return observed.writeErr
	})
	observed.err, observed.elapsed = err, time.Since(started)
	s.mu.Lock()
	s.observed = observed
	s.mu.Unlock()
	return err
}

func (s *externalRevisionScopeStore) observation() externalScopeObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed
}

// This is a scheduling component for public handling, not attribution of the
// native run's untraced internal writer. A real checked canonical write makes
// the real SQL scope reject its bundle while current file facts stay separate.
func TestRegisteredExternalScopeRevisionKeepsOnlyCheckedLocalFileRows(t *testing.T) {
	for _, tool := range []string{"get_editing_context", "get_file_summary"} {
		for _, transition := range []string{"revision_only", "unchanged_scope", "source_event", "unchecked_eviction", "canonical_no_projection", "unknown_scope_error", "cancelled_scope"} {
			t.Run(tool+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				require.True(t, tokens.EncoderReady())
				root := t.TempDir()
				path := filepath.Join(root, "main.go")
				before := "package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"
				require.NoError(t, os.WriteFile(path, []byte(before), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "aux.go"), []byte("package probe\n\nfunc Other() int { return 4 }\n"), 0600))
				sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "external-scope.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sql.Close()) })
				backend := &externalRevisionScopeStore{Store: sql}
				idx := newProjectionTestIndexer(t, backend)
				_, err = idx.Index(root)
				require.NoError(t, err)
				other := sql.GetNode("aux.go::Other")
				require.NotNil(t, other)
				s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
				s.editAckMode = config.EditAckLocal
				pending := make(chan indexer.MutationResult)
				s.SetWatcher(mutationTestWatcher{done: pending})
				t.Cleanup(func() {
					close(pending)
					s.mutationReceipts.Range(func(_, value any) bool {
						select {
						case <-value.(*mutationReceipt).done:
						case <-time.After(3 * time.Second):
							t.Error("owned held publication did not join")
						}
						return true
					})
				})
				session := "external-scope-" + tool + "-" + transition
				require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(session) })
				ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
				require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"external-scope","version":"1"}}}`)))
				s.NoteSessionToolPolicy(session, "all", "defer")
				ack := preselectionRegisteredCall(t, s, ctx, 10, "edit_symbol", map[string]any{"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": "func Target() int {\n\tvalue := 7\n\treturn value\n}", "format": "json"})
				require.False(t, ack.IsError, resultText(ack))
				ackFields := extractTextResult(t, ack)
				require.Equal(t, "local", ackFields["ack_mode"])
				require.Equal(t, "committed", ackFields["disk_status"])
				ackEntry := liveEntry(pendingFileProjections, path)
				require.NotNil(t, ackEntry)
				oldFacts, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), ackEntry)
				require.NoError(t, err)
				acknowledged, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(acknowledged)), ackFields["content_sha256"])
				expected := acknowledged
				if transition == "canonical_no_projection" {
					retirement := s.canonicalProjectionRetirement(path)
					require.NotNil(t, retirement)
					require.NoError(t, idx.IndexFile(path))
					retirement.run()
					require.Nil(t, liveEntry(pendingFileProjections, path), "real canonical publication must retire the selected ACK before this negative")
				} else {
					expected = []byte(strings.Replace(string(acknowledged), "func Target()", "// independent native edit\n\nfunc Target()", 1))
					require.NoError(t, os.WriteFile(path+".native", expected, 0600))
					require.NoError(t, os.Rename(path+".native", path), "real native replacement changes the selected file after the actual ACK")
				}
				// Establish the real handler-built file selection before testing the
				// checked-store callback. This is a scheduling component, not another
				// immediate native campaign: no fake reader/manual ensure is involved.
				warm := preselectionRegisteredCall(t, s, ctx, 11, "get_symbol_source", map[string]any{"id": "main.go::Target", "format": "json", "context_lines": 0, "compress_bodies": false})
				require.False(t, warm.IsError, resultText(warm))
				if transition != "canonical_no_projection" {
					current := liveEntry(pendingFileProjections, path)
					require.NotNil(t, current, "real source read must establish the tested immutable file precondition")
					require.Equal(t, blake3.Sum256(expected), current.rawHash, "read-built projections bind raw bytes with their stored BLAKE3 digest")
				} else {
					require.Nil(t, liveEntry(pendingFileProjections, path))
				}
				caller, cancel := context.WithCancel(ctx)
				defer cancel()
				deadline := time.Now().Add(projectedReadBudget)
				allowance := &projectedSourceAllowance{tool: tool}
				caller = context.WithValue(caller, projectedSourceDeadlineKey{}, deadline)
				caller = context.WithValue(caller, projectedSourceAllowanceKey{}, allowance)
				backend.mutate = func(context.Context) error {
					if transition == "unchanged_scope" {
						return nil
					}
					if transition == "unknown_scope_error" {
						return errors.New("unrelated selected store fault")
					}
					if transition == "cancelled_scope" {
						cancel()
						return nil
					}
					updated := *other
					updated.Meta = maps.Clone(other.Meta)
					if updated.Meta == nil {
						updated.Meta = map[string]any{}
					}
					updated.Meta["scope_revision_probe"] = "checked canonical metadata publication"
					if err := sql.AddBatchChecked([]*graph.Node{&updated}, nil); err != nil {
						return err
					}
					if transition == "source_event" {
						pendingFileProjections.markSuspect(path)
					}
					if transition == "unchecked_eviction" {
						pendingFileProjections.drop(liveEntry(pendingFileProjections, path))
					}
					return nil
				}
				backend.armed.Store(true)
				var result *mcplib.CallToolResult
                var protocolError *struct { Code int `json:"code"`; Message string `json:"message"` }
                if transition == "cancelled_scope" {
                    // Cancellation may be returned by the registered SDK as a
                    // JSON-RPC error instead of a tool result. Inspect the real
                    // envelope rather than manufacturing a successful call.
                    require.True(t, s.IsToolEnabledForSession(caller, tool))
                    s.EnsureToolPromotedForSession(caller, tool)
                    frame, marshalErr := json.Marshal(map[string]any{"jsonrpc":"2.0", "id":20, "method":"tools/call", "params":map[string]any{"name":tool, "arguments":map[string]any{"path":"main.go", "format":"json"}}})
                    require.NoError(t, marshalErr)
                    reply := s.MCPServer().HandleMessage(WithAuthorizedToolCall(caller, tool), frame)
                    raw, marshalErr := json.Marshal(reply)
                    require.NoError(t, marshalErr)
                    var envelope struct { Error *struct { Code int `json:"code"`; Message string `json:"message"` } `json:"error"`; Result *mcplib.CallToolResult `json:"result"` }
                    require.NoError(t, json.Unmarshal(raw, &envelope))
                    protocolError, result = envelope.Error, envelope.Result
                } else {
                    result = preselectionRegisteredCall(t, s, caller, 20, tool, map[string]any{"path":"main.go", "format":"json"})
                }
				observed := backend.observation()
				require.Equal(t, int32(1), backend.calls.Load(), "the real checked file scope must be reached once; no outcome retry")
				t.Logf("scope stage=entry remaining_original=%s scope_elapsed=%s err=%v remaining_after=%s attempts=%d", observed.remainingAtEntry, observed.elapsed, observed.err, time.Until(deadline), allowance.attempts.Load())
				require.Equal(t, deadline, observed.originalDeadline, "no deadline or allowance replacement")
				require.EqualValues(t, 1, allowance.attempts.Load(), "a local section fallback must not add whole-call attempts")
				require.Equal(t, ackEntry.contentSHA256, ackFields["content_sha256"], "old immutable source payload cannot be rebound")
				if transition != "unchanged_scope" && transition != "unknown_scope_error" && transition != "cancelled_scope" {
					require.NoError(t, observed.writeErr)
					require.ErrorIs(t, observed.err, graph.ErrContractProjectionStale, "the checked SQL scope, not a fake error, must refuse changed ownership revision")
				}
				switch transition {
                case "cancelled_scope":
                    require.ErrorIs(t, caller.Err(), context.Canceled)
                    require.NotNil(t, protocolError)
                    require.Equal(t, "context canceled", protocolError.Message)
                    require.Nil(t, result)
				case "revision_only", "unchanged_scope":
					require.NotNil(t, observed.entry, "the actual registered handler must borrow a current file projection")
					requireCanonicalDefinitionRows(t, tool, result, expected)
					if transition == "revision_only" {
						freshness := extractTextResult(t, result)["freshness"].(map[string]any)
						require.Equal(t, "projected", freshness["file_view"])
						require.Equal(t, true, freshness["readback_exact"])
						require.Equal(t, "pending", freshness["graph_freshness"])
						require.Equal(t, false, freshness["complete"])
					}
				default:
					require.True(t, result.IsError, "unknown scope/source ownership/cancellation cannot gain current data or an invented external-write reason: %s", resultText(result))
					require.NotContains(t, resultText(result), `"file_reason":"external_write"`)
				}
				if transition == "canonical_no_projection" {
					require.Nil(t, observed.entry)
				}
				if transition == "source_event" {
					require.True(t, observed.entry.suspect.Load())
				}
				require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded, "the original ACK facts remain unusable after replacement/publication")
				physical, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, expected, physical, "physical witness is after the immediate public read")
			})
		}
	}
}
