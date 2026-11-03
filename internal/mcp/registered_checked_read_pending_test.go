package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

// The real watcher freshness callback places canonical publication after the
// leaf and the decorator's initial check, without replacing graph/source rows.
type checkedPendingTierWatcher struct {
	mutationTestWatcher
	armed   atomic.Bool
	observe func()
}

func (w *checkedPendingTierWatcher) TierPaths() map[string]indexer.MutationTierState {
	if w.armed.CompareAndSwap(true, false) {
		w.observe()
	}
	return nil
}

func TestRegisteredCheckedReadRetirementDuringDecorationReturnsNoDataPending(t *testing.T) {
	for _, tool := range []string{"get_file_summary", "get_editing_context"} {
		for _, transition := range []string{"checked_retirement", "read_cow_then_retirement", "same_bytes_event", "unchecked_eviction", "new_physical_owner", "caller_owned", "cancelled"} {
			t.Run(tool+"/"+transition, func(t *testing.T) {
				selectedProjectionRegistry(t)
				s, path, _, raw := acknowledgedCanonicalFile(t)
				external := []byte(strings.Replace(string(raw), "func Target()", "// external version\n\nfunc Target()", 1))
				require.NoError(t, os.WriteFile(path+".external", external, 0600))
				require.NoError(t, os.Rename(path+".external", path))
				pendingFileProjections.markSuspect(path)
				session := "checked-pending-" + tool
				require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(session) })
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx := s.WithClientSession(WithSessionID(parent, session), session)
				require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
				s.NoteSessionToolPolicy(session, "all", "defer")
				peerCtx := ctx
				watcher := &checkedPendingTierWatcher{mutationTestWatcher: mutationTestWatcher{done: make(chan indexer.MutationResult)}}
				// Preserve the ACK publication ticket's original watcher: this replacement
				// is used only for read freshness after the ACK has already been admitted.
				s.SetWatcher(watcher)
				var selected context.Context
				var selectedEntry *projectionEntry
				var observationErr error
				var calls, observations int
				var acceptedLeaf atomic.Int32
				s.facades.mu.RLock()
				captured := s.facades.captured[tool]
				s.facades.mu.RUnlock()
				leaf := captured.handler
				s.addTool(captured.tool, func(call context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					result, err := leaf(call, request)
					calls++
					if err == nil && result != nil && !result.IsError {
						afterFreshSymbolAcceptance(call, func() { acceptedLeaf.Add(1) })
					}
					if err == nil && result != nil && !result.IsError && calls <= 2 {
						var candidate *projectionEntry
						facts, _ := call.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
						if facts != nil {
							facts.mu.RLock()
							for _, entry := range facts.entries {
								candidate = entry
							}
							facts.mu.RUnlock()
						}
						if candidate != nil {
							selected = call
							selectedEntry = candidate
							watcher.armed.Store(true)
						}
					}
					return result, err
				})
				watcher.observe = func() {
					observations++
					entry := selectedEntry
					if entry == nil {
						observationErr = fmt.Errorf("real leaf did not retain a file owner")
						return
					}
					switch transition {
					case "read_cow_then_retirement":
						if observations == 1 {
							pendingFileProjections.markSuspect(path)
							peer, err := externalVersionRegisteredCall(s, peerCtx, 30, "get_symbol", map[string]any{"id": "main.go::Target", "format": "json", "detail": "full"})
							if err != nil {
								observationErr = err
								return
							}
							if peer == nil || peer.IsError {
								observationErr = fmt.Errorf("registered same-content peer failed: %v", peer)
								return
							}
							current := liveEntry(pendingFileProjections, path)
							if current == nil || current == entry || current.layer != entry.layer || current.rawHash != entry.rawHash {
								observationErr = fmt.Errorf("peer did not create a checked same-content COW owner")
							}
							return
						}
						fallthrough
					case "checked_retirement", "caller_owned":
						if err := s.indexer.IndexFile(path); err != nil {
							observationErr = err
							return
						}
						retirement := s.canonicalProjectionRetirement(path)
						if retirement == nil {
							observationErr = fmt.Errorf("no canonical retirement callback")
							return
						}
						retirement.run()
					case "same_bytes_event":
						pendingFileProjections.markSuspect(path)
					case "unchecked_eviction":
						pendingFileProjections.drop(entry)
					case "new_physical_owner":
						held, release, err := acquireMutationPath(t.Context(), path)
						if err != nil {
							observationErr = err
							return
						}
						_, observationErr = s.commitFileMutation(held, "write_file", "", "", "main.go", path, external, 0600)
						release()
					case "cancelled":
						cancel()
					}
				}
				if transition == "caller_owned" {
					ready := preselectionRegisteredCall(t, s, peerCtx, 31, "get_symbol", map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"})
					require.False(t, ready.IsError, resultText(ready))
					pinned := liveEntry(pendingFileProjections, path)
					require.NotNil(t, pinned)
					var err error
					ctx, err = retainRequestProjection(withRequestProjectionFacts(ctx), pinned)
					require.NoError(t, err)
				}
				allowance := &projectedSourceAllowance{tool: tool}
				deadline := time.Now().Add(projectedReadBudget)
				ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
				ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
				result, err := externalVersionRegisteredCall(s, ctx, 20, tool, map[string]any{"path": "main.go", "format": "json"})
				t.Logf("completed tool=%s transition=%s calls=%d observations=%d attempts=%d remaining=%s result=%v error=%v observer=%v", tool, transition, calls, observations, allowance.attempts.Load(), time.Until(deadline), result, err, observationErr)
				require.NoError(t, observationErr)
				wantObservations := 1
				if transition == "read_cow_then_retirement" {
					wantObservations = 2
				}
				require.Equal(t, wantObservations, observations, "each real leaf has one controlled post-leaf transition")
				require.NotNil(t, selectedEntry)
				require.Same(t, allowance, selected.Value(projectedSourceAllowanceKey{}))
				require.Equal(t, deadline, selected.Value(projectedSourceDeadlineKey{}))
				require.Zero(t, selectedEntry.sourceCommitSeq)
				require.True(t, selectedEntry.readVersionChecked)
				if transition != "cancelled" {
					proof := context.WithoutCancel(selected)
					require.ErrorIs(t, validateRequestProjections(proof), projectionSuperseded, "old facts stay invalid independently of the completed SDK call context")
				}
				require.LessOrEqual(t, allowance.attempts.Load(), int32(2))
				if transition == "cancelled" {
					require.Error(t, err)
					require.Zero(t, acceptedLeaf.Load())
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				t.Logf("tool=%s transition=%s attempts=%d remaining=%s result=%s", tool, transition, allowance.attempts.Load(), time.Until(deadline), resultText(result))
				if transition != "checked_retirement" && transition != "read_cow_then_retirement" {
					require.True(t, result.IsError, "unverified transition cannot gain a pending response")
					require.Zero(t, acceptedLeaf.Load())
					return
				}
				require.True(t, selectedEntry.checkedRetired.Load())
				if transition == "checked_retirement" && !result.IsError {
					requireCanonicalDefinitionRows(t, tool, result, external)
					require.Equal(t, 2, calls)
					require.Equal(t, int32(1), acceptedLeaf.Load(), "only the accepted current attempt gets credit")
					return
				}
				require.False(t, result.IsError, resultText(result))
				body := extractTextResult(t, result)
				require.NotContains(t, body, "nodes")
				require.NotContains(t, body, "defines")
				require.NotContains(t, body, "source")
				require.NotContains(t, body, "content")
				freshness, ok := body["freshness"].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "pending", freshness["graph_freshness"])
				require.Equal(t, "external_write", freshness["file_reason"])
				require.NotEqual(t, true, freshness["readback_exact"])
				require.Equal(t, []any{"main.go"}, freshness["pending_files"])
				require.LessOrEqual(t, calls, 2, "pending must not create a third attempt")
				require.Zero(t, acceptedLeaf.Load(), "neither discarded data-bearing leaf can receive acceptance credit")
				// Formatting is independent of permission to execute another attempt.
				proof := context.WithoutCancel(selected)
				expiredAllowance := &projectedSourceAllowance{tool: tool}
				require.True(t, expiredAllowance.claim())
				proof = context.WithValue(proof, projectedSourceAllowanceKey{}, expiredAllowance)
				proof = context.WithValue(proof, projectedSourceDeadlineKey{}, time.Now().Add(-time.Nanosecond))
				request := makeReq(tool, map[string]any{"path": "main.go", "format": "json"})
				require.False(t, retryAfterCheckedProjectionChange(proof))
				terminal, pendingErr := s.checkedReadTransitionPending(proof, request)
				require.NoError(t, pendingErr)
				require.NotNil(t, terminal)
				require.False(t, terminal.IsError, resultText(terminal))
				require.Equal(t, int32(1), expiredAllowance.attempts.Load(), "formatting pending does not claim a second/third attempt")
				wrong := makeReq(tool, map[string]any{"path": "foreign.go", "format": "json"})
				refused, pendingErr := s.checkedReadTransitionPending(proof, wrong)
				require.NoError(t, pendingErr)
				require.Nil(t, refused, "scope mismatch cannot borrow this file's transition")
				hard, cancelHard := context.WithDeadline(proof, time.Now().Add(-time.Nanosecond))
				defer cancelHard()
				refused, pendingErr = s.checkedReadTransitionPending(hard, request)
				require.ErrorIs(t, pendingErr, context.DeadlineExceeded)
				require.Nil(t, refused)

				// A path that used to resolve can be rebound after retirement. The old
				// graphRelPath fallback echoes a matching string, which is not authority.
				foreign := filepath.Join(t.TempDir(), "foreign.go")
				require.NoError(t, os.WriteFile(foreign, external, 0600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(foreign, path))
				_, _, resolveErr := s.resolveFilePath(proof, "main.go")
				require.Error(t, resolveErr)
				require.Equal(t, "main.go", s.graphRelPath(proof, "main.go"))
				refused, pendingErr = s.checkedReadTransitionPending(proof, request)
				require.NoError(t, pendingErr)
				require.Nil(t, refused, "failed physical path authority cannot become pending by string equality")
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, external, 0600))

			})
		}
	}
}
