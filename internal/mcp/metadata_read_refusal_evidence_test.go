package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// These are observations of existing refusing guards, not authority to reuse a
// selection. Restore the suspect flag before encoding to catch later sampling.
func TestMetadataReadRefusalEvidenceCapturesActualPredicate(t *testing.T) {
	for _, flag := range []string{"", "0", "true", "1"} {
		for _, guard := range []string{"suspect_before", "owner_before", "stamp_changed"} {
			t.Run(flag+"/"+guard, func(t *testing.T) {
				t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", flag)
				selectedProjectionRegistry(t)
				path := filepath.Join(t.TempDir(), "proof.go")
				raw := []byte("package proof\n")
				require.NoError(t, os.WriteFile(path, raw, 0600))
				stamp, err := statDiskStamp(path)
				require.NoError(t, err)
				entry := syntheticProjectionEntry(path, "proof.go", "proof", 1, stamp, raw)
				require.NoError(t, pendingFileProjections.publish(entry))
				require.True(t, liveProjectionForAcknowledgement(entry))
				switch guard {
				case "suspect_before":
					entry.suspect.Store(true)
				case "owner_before":
					pendingFileProjections.drop(entry)
				case "stamp_changed":
					require.NoError(t, os.WriteFile(path, []byte("package changed\n"), 0600))
				}
				_, refusal := retainRequestProjection(t.Context(), entry)
				require.ErrorIs(t, refusal, projectionSuperseded)
				if guard == "suspect_before" {
					entry.suspect.Store(false)
					require.True(t, liveProjectionForAcknowledgement(entry))
				}
				t.Logf("METADATA_GUARD_CAUSAL flag=%q original_guard=%s refused=true suspect_restored=%v", flag, guard, guard == "suspect_before")
				result := searchRefusalToolResult(refusal)
				require.True(t, result.IsError)
				require.Equal(t, projectionSuperseded.Error(), resultText(result))
				if flag != "1" {
					require.True(t, refusal == projectionSuperseded, "disabled diagnostics preserve error identity")
					require.Nil(t, result.Meta)
					return
				}
				require.NotNil(t, result.Meta)
				evidence := result.Meta.AdditionalFields["gortex_projection_refusal"]
				require.Equal(t, map[string]any{"stage": "bind.retain", "guard": guard}, evidence)
				encoded, err := json.Marshal(evidence)
				require.NoError(t, err)
				require.LessOrEqual(t, len(encoded), 128)
				require.NotContains(t, string(encoded), path)
			})
		}
	}
}

// Existing registered all/defer requests execute a real leaf and real peer
// LOCALACK. This verifies opt-in evidence on the existing physical refusal;
// it does not manufacture a same-byte failure or replay full-symbol effects.
func TestRegisteredMetadataReadRefusalEvidenceIsOptIn(t *testing.T) {
	for _, tool := range []string{"get_symbol", "get_editing_context"} {
		for _, flag := range []string{"", "1"} {
			t.Run(tool+"/"+flag, func(t *testing.T) {
				t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", flag)
				s, backend, _, path, _ := snapshotFacadeFixture(t)
				s.engine.SetSearchProvider(s.indexer.Search)
				fts, err := backend.SymbolFTSCount()
				require.NoError(t, err)
				require.Positive(t, fts)
				const session = "metadata-refusal-evidence"
				require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
				t.Cleanup(func() { s.DisconnectSession(session) })
				base := s.WithClientSession(WithSessionID(t.Context(), session), session)
				s.NoteSessionToolPolicy(session, "all", "defer")
				require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"metadata-evidence","version":"1"}}}`)))
				s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
				leaf, ok := s.facades.legacy(tool)
				require.True(t, ok)
				var selected context.Context
				var peer *mcplib.CallToolResult
				var peerErr, selectedErr error
				var remaining time.Duration
				calls := 0
				s.addTool(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					calls++
					result, readErr := leaf.handler(ctx, req)
					if readErr == nil && result != nil && !result.IsError && calls == 1 {
						selected = ctx
						peer, peerErr = externalVersionRegisteredCall(s, base, 30, "edit_symbol", map[string]any{"id": "main.go::NeighborBefore", "old_source": "func NeighborBefore() int { return 2 }", "new_source": "func NeighborBefore() int { return 3 }", "dry_run": false})
						selectedErr = validateRequestProjections(ctx)
						remaining = time.Until(ctx.Value(projectedSourceDeadlineKey{}).(time.Time))
					}
					return result, readErr
				})
				args := map[string]any{"path": "main.go", "format": "json"}
				if tool == "get_symbol" {
					args = map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"}
				}
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				deadline := time.Now().Add(projectedReadBudget)
				ctx := context.WithValue(base, projectedSourceDeadlineKey{}, deadline)
				result, err := externalVersionRegisteredCall(s, ctx, 20, tool, args)
				require.NoError(t, err)
				require.NotNil(t, selected, "the actual registered leaf must run")
				require.Equal(t, 1, calls)
				require.NoError(t, peerErr)
				require.NotNil(t, peer)
				require.False(t, peer.IsError, resultText(peer))
				require.Equal(t, "local", extractTextResult(t, peer)["ack_mode"])
				require.ErrorIs(t, selectedErr, projectionSuperseded)
				require.Positive(t, remaining, "the original150ms must still be live at the causal boundary")
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotEqual(t, before, after)
				require.True(t, result.IsError)
				t.Logf("REAL_METADATA_REFUSAL native_FTS=%d leaf_calls=%d remaining_original150=%s tool=%s physical_changed=true selected_facts_invalid=true flag=%q", fts, calls, remaining, tool, flag)
				if flag != "1" {
					if result.Meta != nil {
						require.NotContains(t, result.Meta.AdditionalFields, "gortex_projection_refusal")
					}
					return
				}
				require.NotNil(t, result.Meta)
				evidence := result.Meta.AdditionalFields["gortex_projection_refusal"]
				require.Equal(t, map[string]any{"stage": "response.decoration", "guard": "owner_before"}, evidence)
				t.Logf("REAL_METADATA_REFUSAL native_FTS=%d leaf_calls=%d remaining_original150=%s tool=%s evidence=%v", fts, calls, remaining, tool, evidence)
			})
		}
	}
}

// Validation must describe the phase that actually rejected the borrowed
// fact, even when the guard is healthy again before wire conversion.
func TestMetadataReadValidationEvidenceKeepsFirstPredicate(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(fmt.Sprint(inherited), func(t *testing.T) {
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
			selectedProjectionRegistry(t)
			path := filepath.Join(t.TempDir(), "proof.go")
			raw := []byte("package proof\n")
			require.NoError(t, os.WriteFile(path, raw, 0600))
			stamp, err := statDiskStamp(path)
			require.NoError(t, err)
			entry := syntheticProjectionEntry(path, "proof.go", "proof", 1, stamp, raw)
			require.NoError(t, pendingFileProjections.publish(entry))
			ctx, err := retainRequestProjection(t.Context(), entry)
			require.NoError(t, err)
			entry.suspect.Store(true)
			var refusal error
			stage := "projection.validation"
			if inherited {
				_, refusal = (&Server{}).ensureProjected(ctx, nil)
				stage = "bind.inherited_facts"
			} else {
				refusal = validateRequestProjections(ctx)
			}
			require.ErrorIs(t, refusal, projectionSuperseded)
			entry.suspect.Store(false)
			require.NoError(t, validateRequestProjections(ctx))
			t.Logf("METADATA_VALIDATION_CAUSAL inherited=%v original_guard=suspect_before restored_validation_pass=true", inherited)
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "0")
			result := searchRefusalToolResult(refusal)
			require.True(t, result.IsError)
			require.Equal(t, projectionSuperseded.Error(), resultText(result))
			require.NotNil(t, result.Meta)
			require.Equal(t, map[string]any{"stage": stage, "guard": "suspect_before"}, result.Meta.AdditionalFields["gortex_projection_refusal"])
		})
	}
}

func TestMetadataReadEvidenceDoesNotAnnotateCancellationOrSuccess(t *testing.T) {
	t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
	selectedProjectionRegistry(t)
	path := filepath.Join(t.TempDir(), "proof.go")
	raw := []byte("package proof\n")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	entry := syntheticProjectionEntry(path, "proof.go", "proof", 1, stamp, raw)
	require.NoError(t, pendingFileProjections.publish(entry))
	ctx, err := retainRequestProjection(t.Context(), entry)
	require.NoError(t, err)
	require.NoError(t, validateRequestProjections(ctx))
	for _, deadline := range []bool{false, true} {
		var stopped context.Context
		var cancel context.CancelFunc
		want := context.Canceled
		if deadline {
			stopped, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			stopped, cancel = context.WithCancel(ctx)
			cancel()
		}
		refusal := validateRequestProjections(stopped)
		cancel()
		require.ErrorIs(t, refusal, want)
		result := searchRefusalToolResult(refusal)
		require.Nil(t, result.Meta)
		require.Equal(t, want.Error(), resultText(result))
	}
	generic := errors.New(projectionSuperseded.Error())
	require.Nil(t, searchRefusalToolResult(generic).Meta, "matching English text carries no typed guard authority")
}
