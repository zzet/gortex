package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// Read-built publication already calls this production watcher admission
// boundary before returning its new owner to initial search binding.
type searchBindAdmissionWatcher struct {
	mutationTestWatcher
	armed   atomic.Bool
	observe func(context.Context, string)
}

func (w *searchBindAdmissionWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	if w.armed.CompareAndSwap(true, false) {
		w.observe(ctx, path)
	}
	return w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
}

func TestRegisteredNativeSearchSelectsCheckedOwnerAfterReadAdmission(t *testing.T) {
	for _, transition := range []string{"retain_same_file", "snapshot_sibling"} {
		for _, query := range []string{"name:Target", "name:TargetBefore"} {
			t.Run(transition+"/"+query, func(t *testing.T) {
				t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
				s, backend, base, path, original := snapshotFacadeFixture(t)
				s.engine.SetSearchProvider(s.indexer.Search)
				require.Implements(t, (*graph.FoldedNameCandidateReader)(nil), backend)
				nativeCount, err := backend.SymbolFTSCount()
				require.NoError(t, err)
				require.Positive(t, nativeCount)
				targetPath, targetSymbol := path, "main.go::Target"
				if transition == "snapshot_sibling" {
					targetPath = filepath.Join(filepath.Dir(path), "z.go")
					targetSymbol = "z.go::Sibling"
					ack := snapshotRegisteredCall(t, s, base, 12, "edit", map[string]any{
						"operation": "write", "target": map[string]any{"file": targetPath},
						"content": "package probe\n\nfunc Sibling() int { return 3 }\n",
						"output": map[string]any{"format": "json"},
					})
					require.False(t, ack.IsError, resultText(ack))
					require.Equal(t, true, extractTextResult(t, ack)["readback_exact"])
				}
				targetOriginal := liveEntry(pendingFileProjections, targetPath)
				require.NotNil(t, targetOriginal)
				oldFacts, err := s.bindPendingSearch(base)
				require.NoError(t, err)
				require.NoError(t, validateRequestProjections(oldFacts))
				physicalTarget := snapshotPhysicalTarget(t, path)
				targetBytes, err := os.ReadFile(targetPath)
				require.NoError(t, err)
				physicalSelected := sha256.Sum256(targetBytes)

				const peerID = "checked-owner-admission-reader"
				require.NoError(t, s.ConnectSession(peerID, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(peerID) })
				peer := s.WithClientSession(WithSessionID(t.Context(), peerID), peerID)
				s.NoteSessionToolPolicy(peerID, FacadeSurfaceVersion, "hide")
				require.NotNil(t, s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"checked-owner-peer","version":"1"}}}`)))
				s.MCPServer().HandleMessage(peer, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
				var admitted, replaced *projectionEntry
				var peerReply *mcplib.CallToolResult
				var peerErr, originalErr error
				var remaining time.Duration
				var callbacks int
				previous, ok := s.currentWatcher().(mutationTestWatcher)
				require.True(t, ok)
				watcher := &searchBindAdmissionWatcher{mutationTestWatcher: previous}
				watcher.observe = func(ctx context.Context, admittedPath string) {
					callbacks++
					if admittedPath != path {
						return
					}
					admitted = liveEntry(pendingFileProjections, path)
					replaced = liveEntry(pendingFileProjections, targetPath)
					pendingFileProjections.markSuspect(targetPath)
					peerReply, peerErr = externalVersionRegisteredCall(s, peer, 30, "read", map[string]any{
						"operation": "source", "target": map[string]any{"symbol": targetSymbol},
						"context": map[string]any{"context_lines": 0}, "output": map[string]any{"format": "json"},
					})
					originalErr = ctx.Err()
					if deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok {
						remaining = time.Until(deadline)
					}
				}
				s.SetWatcher(watcher)
				watcher.armed.Store(true)
				pendingFileProjections.markSuspect(path)
				args := snapshotSearchArgs()
				args["query"] = query
				result, callErr := externalVersionRegisteredCall(s, base, 20, "search", args)
				current := liveEntry(pendingFileProjections, targetPath)
				wire, marshalErr := json.Marshal(result)
				t.Logf("transition=%s query=%s native_FTS=%d callbacks=%d editing_session=%s peer_session=%s remaining_original=%s result=%s error=%v", transition, query, nativeCount, callbacks, SessionIDFromContext(base), SessionIDFromContext(peer), remaining, wire, callErr)
				// Prove the real causal lifecycle before the intended RED oracle.
				require.NoError(t, marshalErr)
				require.Equal(t, 1, callbacks)
				require.NotNil(t, admitted)
				require.NotSame(t, original, admitted)
				require.Same(t, original.layer, admitted.layer)
				require.True(t, admitted.readVersionChecked)
				require.Zero(t, admitted.commitSeq)
				require.NoError(t, peerErr)
				require.NotNil(t, peerReply)
				require.False(t, peerReply.IsError, resultText(peerReply))
				require.NoError(t, originalErr)
				require.Positive(t, remaining, "original150ms allowance must still be live")
				require.NotNil(t, current)
				require.NotSame(t, replaced, current)
				require.Same(t, targetOriginal.layer, current.layer)
				require.Equal(t, targetOriginal.rawHash, current.rawHash)
				require.Equal(t, targetOriginal.contentSHA256, current.contentSHA256)
				require.Equal(t, targetOriginal.sourceCommitSeq, current.sourceCommitSeq)
				require.True(t, current.readVersionChecked)
				require.Zero(t, current.commitSeq)
				require.Zero(t, current.sourceEvents)
				require.True(t, replaced.suspect.Load())
				require.True(t, liveProjectionForAcknowledgement(current))
				require.Equal(t, physicalTarget, snapshotPhysicalTarget(t, path))
				after, err := os.ReadFile(targetPath)
				require.NoError(t, err)
				require.Equal(t, physicalSelected, sha256.Sum256(after))
				require.Nil(t, backend.GetNode("main.go::Target"), "canonical publication remains held")
				require.ErrorIs(t, validateRequestProjections(oldFacts), projectionSuperseded)
				require.NoError(t, callErr)
				t.Log("CAUSAL_PREMISES_VERIFIED: actual read-admission COW, physical bytes unchanged, old facts invalid, original allowance live")
				if query == "name:Target" {
					snapshotTargetRows(t, result)
				} else {
					require.NotNil(t, result)
					require.False(t, result.IsError, resultText(result))
					fields := extractTextResult(t, result)
					require.Empty(t, fields["results"])
					require.Equal(t, float64(0), fields["total"])
					require.NotEqual(t, true, fields["truncated"], "absence must be complete")
				}
			})
		}
	}
}
