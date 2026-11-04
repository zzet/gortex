package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func requireReadFreshnessContract(t *testing.T, result *mcplib.CallToolResult, code string) {
	t.Helper()
	require.NotNil(t, result)
	require.True(t, result.IsError, "a retry contract must never turn the refused first response into success")
 require.True(t, metadataTransientRefusal(result), "only exact data-free typed refusals bypass successful freshness decoration")
	var body struct {
		Code      string         `json:"error_code"`
		Retriable bool           `json:"retriable"`
		Data      map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(result)), &body), "typed transient refusal must survive actual MCP result encoding: %s", resultText(result))
	require.Equal(t, code, body.Code)
	require.True(t, body.Retriable)
	require.Equal(t, "read_only", body.Data["retry_scope"])
	require.Equal(t, false, body.Data["delivery_unknown"])
}

// A real completed leaf must still refuse a different physical publication.
// This tests wire classification, never permission to replay that leaf.
func TestRegisteredReadFreshnessRefusalCarriesTypedReason(t *testing.T) {
	for _, tc := range []struct{ legacy, operation string }{{"get_file_summary", "summary"}, {"get_editing_context", "editing_context"}} {
		t.Run(tc.operation, func(t *testing.T) {
			s, backend, base, path, _ := snapshotFacadeFixture(t)
			idx, _ := s.indexerForRel("main.go")
			require.NotNil(t, idx.Search)
			s.engine.SetSearchProvider(idx.Search)
			fts, err := backend.SymbolFTSCount()
			require.NoError(t, err)
			require.Positive(t, fts)
			leaf, ok := s.facades.legacy(tc.legacy)
			require.True(t, ok)
			type selected struct {
				ctx    context.Context
				result *mcplib.CallToolResult
				err    error
			}
			held := make(chan selected, 1)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				r, e := leaf.handler(ctx, req)
				held <- selected{ctx, r, e}
				<-release
				return r, e
			})
			type answer struct {
				result *mcplib.CallToolResult
				err    error
			}
			answered := make(chan answer, 1)
			var worker sync.WaitGroup
			worker.Add(1)
			t.Cleanup(func() { unblock(); worker.Wait() })
			logical, cancelLogical := context.WithTimeout(base, time.Second)
			defer cancelLogical()
			deadline := time.Now().Add(projectedReadBudget)
			caller := context.WithValue(logical, projectedSourceDeadlineKey{}, deadline)
			go func() {
				defer worker.Done()
				r, e := externalVersionRegisteredCall(s, caller, 20, "read", map[string]any{"operation": tc.operation, "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
				answered <- answer{r, e}
			}()
			var old selected
			select {
			case old = <-held:
			case <-time.After(3 * time.Second):
				t.Fatal("registered read did not reach actual leaf")
			}
			require.NoError(t, old.err)
			require.NotNil(t, old.result)
			require.False(t, old.result.IsError, resultText(old.result))
			require.NoError(t, validateRequestProjections(old.ctx))
			ack := snapshotRegisteredCall(t, s, base, 30, "edit", snapshotEditArgs(path, "NeighborBefore", "NeighborAfter"))
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, true, fields["readback_exact"])
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(raw), "NeighborAfter")
			require.Equal(t, fields["content_sha256"], fmtReadSHA(raw))
			require.ErrorIs(t, validateRequestProjections(old.ctx), projectionSuperseded)
			require.True(t, time.Now().Before(deadline), "original150ms must be live at the actual physical peer transition")
			unblock()
			got := <-answered
			worker.Wait()
			require.NoError(t, got.err)
			t.Logf("CAUSAL_PEER_ACK_POSTLEAF_VERIFIED nativeFTS=%d operation=%s original_remaining=%s oldfacts_invalid=true actual_refusal=%t", fts, tc.operation, time.Until(deadline), got.result != nil && got.result.IsError)
			requireReadFreshnessContract(t, got.result, "projection_superseded")
			fresh := snapshotRegisteredCall(t, s, logical, 40, "read", map[string]any{"operation": tc.operation, "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
			require.False(t, fresh.IsError, resultText(fresh))
			require.Contains(t, resultText(fresh), "NeighborAfter")
			require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(old.ctx)), projectionSuperseded, "old ownership stays invalid independently of completed RPC cancellation")
		})
	}
}

func fmtReadSHA(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// Real SQL admission exhausts the original150ms projection allowance and the
// separate existing30ms file section. The caller remains live, bytes stay exact,
// and no current projection rows exist to authorize a successful metadata answer.
func TestRegisteredPendingEmptyReadCarriesTypedReason(t *testing.T) {
	s, backend, base, path, original := snapshotFacadeFixture(t)
	idx, _ := s.indexerForRel("main.go")
	require.NotNil(t, idx.Search)
	s.engine.SetSearchProvider(idx.Search)
	fts, err := backend.SymbolFTSCount()
	require.NoError(t, err)
	require.Positive(t, fts)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	changed := bytes.Replace(raw, []byte("return 2"), []byte("return 3"), 1)
	require.NotEqual(t, raw, changed)
	require.NoError(t, os.WriteFile(path, changed, 0600))
	pendingFileProjections.markSuspect(path)
	require.True(t, original.suspect.Load())
	backend.entered = make(chan struct{})
	backend.query = make(chan struct{})
	backend.returned = make(chan error, 1)
	backend.armed.Store(true)
	startSQL := sync.OnceFunc(func() { close(backend.query) })
	release := make(chan struct{})
	releasePool := sync.OnceFunc(func() { close(release) })
	var request, holders sync.WaitGroup
	t.Cleanup(func() { startSQL(); releasePool(); request.Wait(); holders.Wait() })
	leaf, ok := s.facades.legacy("get_editing_context")
	require.True(t, ok)
	type selected struct {
		ctx    context.Context
		result *mcplib.CallToolResult
		err    error
	}
	selectedLeaf := make(chan selected, 1)
	s.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		r, e := leaf.handler(ctx, req)
		selectedLeaf <- selected{ctx, r, e}
		return r, e
	})
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	answered := make(chan answer, 1)
	caller, cancel := context.WithTimeout(base, 3*time.Second)
	defer cancel()
	request.Add(1)
	go func() {
		defer request.Done()
		r, e := externalVersionRegisteredCall(s, caller, 20, "read", map[string]any{"operation": "editing_context", "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
		answered <- answer{r, e}
	}()
	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("changed-byte refresh did not reach metadata SQL")
	}
	deadline, ok := backend.observed.Deadline()
	require.True(t, ok)
	require.Positive(t, time.Until(deadline))
	require.LessOrEqual(t, time.Until(deadline), projectedReadBudget)
	count := min(runtime.NumCPU(), 4)
	entered := make(chan struct{}, count)
	holderErrors := make(chan error, count)
	fixtureRelease := errors.New("fixture reader rollback")
	for range count {
		holders.Add(1)
		go func() {
			defer holders.Done()
			holderErrors <- backend.WithFileEditingContextReader(t.Context(), func(context.Context, graph.FileEditingContextRows) error {
				entered <- struct{}{}
				<-release
				return fixtureRelease
			})
		}()
	}
	for range count {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("actual SQLite pool reservation failed")
		}
	}
	before := backend.ReaderWaitMark()
	startSQL()
	got := <-answered
	owned := <-selectedLeaf
	releasePool()
	request.Wait()
	holders.Wait()
	for range count {
		require.ErrorIs(t, <-holderErrors, fixtureRelease)
	}
	require.NoError(t, got.err)
	require.NoError(t, owned.err)
	require.NoError(t, caller.Err())
	require.NotEmpty(t, requestDeferredProjectionFiles(owned.ctx))
	require.Contains(t, requestStoreBusyFiles(owned.ctx), "main.go")
	require.Greater(t, backend.ReaderWaitMark().PoolWaits, before.PoolWaits)
	physical, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, changed, physical)
	t.Logf("CAUSAL_PENDING_EMPTY_SQL_VERIFIED nativeFTS=%d projection_private_expired=%t caller_live=true pool_wait=true request_pending=true actual_empty_refusal=%t", fts, !time.Now().Before(deadline), got.result != nil && got.result.IsError)
	requireReadFreshnessContract(t, got.result, "file_view_pending")
	fresh := snapshotRegisteredCall(t, s, caller, 40, "read", map[string]any{"operation": "editing_context", "target": map[string]any{"file": path}, "output": map[string]any{"format": "json", "max_bytes": 0}})
	require.False(t, fresh.IsError, resultText(fresh))
	require.Contains(t, resultText(fresh), "main.go::Target")
	require.NoError(t, caller.Err(), "both distinct RPCs retain one outer logical budget")
}

func TestMetadataTransientContractLeavesTerminalErrorsUnmarked(t *testing.T) {
 for _, body := range []string{
 `{"error_code":"projection_superseded","retriable":false,"data":{"retry_scope":"read_only","delivery_unknown":false}}`,
 `{"error_code":"file_view_pending","retriable":true,"data":{"retry_scope":"read_only"}}`,
 `{"error_code":"file_view_pending","retriable":true,"data":{"retry_scope":"write","delivery_unknown":false}}`,
 `{"error_code":"unknown","retriable":true,"data":{"retry_scope":"read_only","delivery_unknown":false}}`,
 } { require.False(t, metadataTransientRefusal(mcplib.NewToolResultError(body))) }
 require.False(t, metadataTransientRefusal(mcplib.NewToolResultText(`{"error_code":"file_view_pending","retriable":true,"data":{"retry_scope":"read_only","delivery_unknown":false}}`)))

	s, _ := setupTestServer(t)
	for _, tc := range []struct {
		name, tool string
		err        error
		args       map[string]any
		cancel     bool
	}{
		{"message_only", "get_file_summary", errors.New(projectionSuperseded.Error()), nil, false},
		{"genuine_notfound", "get_editing_context", errors.New("no symbols found for file: main.go"), nil, false},
		{"scope_denial", "get_editing_context", errors.New("scope denied"), nil, false},
		{"write", "edit_file", projectionSuperseded, nil, false},
		{"unaudited_callers", "get_callers", projectionSuperseded, nil, false},
		{"compressed", "get_editing_context", projectionSuperseded, map[string]any{"compress_bodies": true}, false},
		{"caller_cancelled", "get_file_summary", projectionSuperseded, nil, true},
		{"file_cap", "get_file_summary", projectionOverFiles, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			req := makeReq(tc.tool, tc.args)
			result := s.metadataReadError(ctx, req, tc.err)
			require.True(t, result.IsError)
			var body struct {
				Code string `json:"error_code"`
			}
			_ = json.Unmarshal([]byte(resultText(result)), &body)
			require.NotEqual(t, "projection_superseded", body.Code)
			require.NotEqual(t, "file_view_pending", body.Code)
		})
	}
	ctx := withRequestProjectionFacts(t.Context())
	req := makeReq("get_editing_context", map[string]any{"path": "main.go"})
	require.Nil(t, s.pendingEmptyMetadataRead(ctx, req, "main.go"), "real absence has no pending witness")
	markRequestProjectionDeferred(ctx, "other.go", "external_write")
	require.Nil(t, s.pendingEmptyMetadataRead(ctx, req, "main.go"), "another file cannot mark requested absence transient")
}
