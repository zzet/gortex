package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This controlled component distinguishes an ACK-derived version from a real
// externally changed version; it does not identify the untraced native event.
func TestRegisteredExternalVersionReadCOWKeepsCurrentRows(t *testing.T) {
	t.Run("ack_derived_version", func(t *testing.T) { runExternalVersionReadCOWProof(t, false) })
	t.Run("external_built_version", func(t *testing.T) { runExternalVersionReadCOWProof(t, true) })
}

// Workers report protocol errors to their owning test goroutine instead of
// using FailNow inside the registered SDK dispatch.
func externalVersionRegisteredCall(s *Server, ctx context.Context, id int, name string, args map[string]any) (*mcplib.CallToolResult, error) {
	if !s.IsToolEnabledForSession(ctx, name) {
		return nil, fmt.Errorf("disabled registered tool %s", name)
	}
	s.EnsureToolPromotedForSession(ctx, name)
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return nil, err
	}
	reply := s.MCPServer().HandleMessage(WithAuthorizedToolCall(ctx, name), frame)
	raw, err := json.Marshal(reply)
	if err != nil {
		return nil, err
	}
	var env struct {
		Error  json.RawMessage        `json:"error"`
		Result *mcplib.CallToolResult `json:"result"`
	}
	if err = json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("registered protocol error: %s", env.Error)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("missing registered result")
	}
	return env.Result, nil
}
func runExternalVersionReadCOWProof(t *testing.T, external bool) {
	nativePopulation, afterBorrow := true, true
	tools := []string{"get_file_summary", "get_editing_context", "get_symbol_source"}
	gateReads := int32(3)
	if nativePopulation {
		tools = append(tools, "get_symbol", "read_file")
		gateReads = 4 // raw read_file does not need graph projection metadata
	}
	selectedProjectionRegistry(t)
	require.True(t, tokens.EncoderReady())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cow.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	backend := &sameOwnerReadCOWStore{Store: sql, captured: make(chan *projectionEntry, gateReads), release: make(chan struct{}), path: path, gateReads: gateReads}
	idx := newProjectionTestIndexer(t, backend)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	borrowed := make(chan registeredBorrowObservation, 8)
	releaseBorrowed := make(chan struct{})
	releaseBorrowedOnce := sync.OnceFunc(func() { close(releaseBorrowed) })
	t.Cleanup(releaseBorrowedOnce)
	var originalDeadline time.Time
	allowances := make(map[string]*projectedSourceAllowance)
	for _, tool := range tools {
		if tool != "read_file" {
			allowances[tool] = &projectedSourceAllowance{tool: tool}
		}
	}
	var selectedBudgetMu sync.Mutex
	var selectedBudgets []registeredBorrowObservation
	if afterBorrow {
		for _, name := range tools {
			if name != "get_symbol_source" && name != "get_file_summary" {
				continue
			}
			s.facades.mu.RLock()
			captured := s.facades.captured[name]
			s.facades.mu.RUnlock()
			leaf := captured.handler
			tool := name
			s.addTool(captured.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				if ctx.Value(registeredReadCOWActorKey{}) != true {
					selectedBudgetMu.Lock()
					selectedBudgets = append(selectedBudgets, registeredBorrowObservation{tool: tool, ctx: ctx})
					selectedBudgetMu.Unlock()
				}
				result, err := leaf(ctx, req)
				if err == nil && result != nil && !result.IsError && ctx.Value(registeredReadCOWActorKey{}) != true {
					borrowed <- registeredBorrowObservation{tool: tool, ctx: ctx}
					<-releaseBorrowed
				}
				return result, err
			})
		}
	}
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
	editTool := "edit_file"
	editArgs := map[string]any{"path": path, "old_string": "func Target() int { return 1 }", "new_string": body, "format": "json"}
	if nativePopulation {
		editTool = "edit_symbol"
		editArgs = map[string]any{"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false}
	}
	ack := preselectionRegisteredCall(t, s, ctx, 10, editTool, editArgs)
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
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(raw)), fields["content_sha256"], "physical ACK bytes use the actual SHA256 domain")
	oldCtx, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), entry)
	require.NoError(t, err)
	if external {
		raw = []byte(strings.Replace(string(raw), "func Target()", "// independent external version\n\nfunc Target()", 1))
		require.NoError(t, os.WriteFile(path+".native", raw, 0600))
		require.NoError(t, os.Rename(path+".native", path), "real native replacement after the registered local ACK")
	}
	require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "the independent canonical rows precede this actual local ACK")
	pendingFileProjections.markSuspect(path)
	require.True(t, entry.suspect.Load())
	require.ErrorIs(t, validateRequestProjections(oldCtx), projectionSuperseded)
	var borrowedOwner *projectionEntry
	if afterBorrow {
		actor := context.WithValue(ctx, registeredReadCOWActorKey{}, true)
		refreshed := preselectionRegisteredCall(t, s, actor, 15, "get_symbol_source", map[string]any{"id": "main.go::Target", "context_lines": 0, "compress_bodies": false, "format": "json"})
		require.False(t, refreshed.IsError, resultText(refreshed))
		borrowedOwner = liveEntry(pendingFileProjections, path)
		require.NotSame(t, entry, borrowedOwner)
		require.Zero(t, borrowedOwner.commitSeq)
		if external {
			require.NotSame(t, entry.layer, borrowedOwner.layer, "changed bytes require a new real parse")
			require.Zero(t, borrowedOwner.sourceCommitSeq, "this read-built version has no physical commit identity")
		} else {
			require.Same(t, entry.layer, borrowedOwner.layer)
			require.Positive(t, borrowedOwner.sourceCommitSeq)
		}
	}
	// This lifecycle component deliberately holds two real handlers until three
	// peer calls and a separate real refresh finish. Select one original fixture
	// allowance before the burst; production150ms/expiry is tested separately.
	originalDeadline = time.Now().Add(canonicalLifecycleFixtureBudget)
	ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, originalDeadline)
	backend.armed.Store(!afterBorrow)
	release := sync.OnceFunc(func() { close(backend.release) })
	type answer struct {
		tool   string
		result *mcplib.CallToolResult
		err    error
	}
	answers := make(chan answer, len(tools))
	var workers sync.WaitGroup
	t.Cleanup(func() { release(); releaseBorrowedOnce(); workers.Wait() })
	for i, tool := range tools {
		allowance := allowances[tool]
		workers.Add(1)
		go func() {
			defer workers.Done()
			args := map[string]any{"path": "main.go", "format": "json"}
			if tool == "get_symbol_source" {
				args = map[string]any{"id": "main.go::Target", "context_lines": 0, "compress_bodies": false, "format": "json"}
			}
			if tool == "get_symbol" {
				args = map[string]any{"id": "main.go::Neighbor", "detail": "full", "format": "json"}
			}
			requestCtx := ctx
			if allowance != nil {
				requestCtx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			}
			result, callErr := externalVersionRegisteredCall(s, requestCtx, 20+i, tool, args)
			answers <- answer{tool: tool, result: result, err: callErr}
		}()
	}
	earlyResults := map[string]*mcplib.CallToolResult{}
	if !afterBorrow {
		for range gateReads {
			select {
			case captured := <-backend.captured:
				require.Same(t, entry, captured, "both metadata reads and source must capture the same old pointer before the actual CAS")
			case <-time.After(3 * time.Second):
				t.Fatal("owned registered reads did not reach the common metadata barrier")
			}
		}
		release()
	} else {
		selected := make(map[string]context.Context)
		for range 2 {
			select {
			case observation := <-borrowed:
				require.NotContains(t, selected, observation.tool)
				selected[observation.tool] = observation.ctx
				facts := observation.ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
				facts.mu.RLock()
				owned := facts.entries[entry.key]
				facts.mu.RUnlock()
				require.Same(t, borrowedOwner, owned)
			case <-time.After(3 * time.Second):
				t.Fatal("four actual public leaves did not borrow the same current read owner")
			}
		}
		// These three real public responses must already have joined before
		// the event. Only source and summary are held at their first successful leaf.
		for range 3 {
			select {
			case a := <-answers:
				require.NoError(t, a.err)
				require.NotContains(t, selected, a.tool)
				earlyResults[a.tool] = a.result
			case <-time.After(3 * time.Second):
				t.Fatal("unheld public reader did not join")
			}
		}
		pendingFileProjections.markSuspect(path)
		require.True(t, borrowedOwner.suspect.Load())
		for _, old := range selected {
			require.ErrorIs(t, validateRequestProjections(old), projectionSuperseded)
		}
		actor := context.WithValue(ctx, registeredReadCOWActorKey{}, true)
		refreshed := preselectionRegisteredCall(t, s, actor, 40, "get_symbol_source", map[string]any{"id": "main.go::Target", "context_lines": 0, "compress_bodies": false, "format": "json"})
		require.False(t, refreshed.IsError, resultText(refreshed))
		current := liveEntry(pendingFileProjections, path)
		require.NotSame(t, borrowedOwner, current)
		require.Zero(t, current.commitSeq)
		require.Same(t, borrowedOwner.layer, current.layer)
		require.Equal(t, borrowedOwner.rawHash, current.rawHash)
		require.False(t, current.suspect.Load())
		pendingFileProjections.mu.Lock()
		t.Logf("post-borrow read-COW old_events=%d current_events=%d old_checked=%v current_checked=%v old_origin=%d current_origin=%d remaining=%s", borrowedOwner.sourceEvents, current.sourceEvents, borrowedOwner.checkedRetired.Load(), current.checkedRetired.Load(), borrowedOwner.sourceCommitSeq, current.sourceCommitSeq, time.Until(originalDeadline))
		pendingFileProjections.mu.Unlock()
		for _, old := range selected {
			require.ErrorIs(t, validateRequestProjections(old), projectionSuperseded, "the new response cannot revive the original context")
		}
		require.True(t, time.Now().Before(originalDeadline), "controlled COW must complete inside the unchanged original allowance")
		releaseBorrowedOnce()
	}
	results := earlyResults
	for range len(tools) - len(earlyResults) {
		select {
		case a := <-answers:
			require.NoError(t, a.err)
			results[a.tool] = a.result
		case <-time.After(3 * time.Second):
			t.Fatal("owned registered read did not join")
		}
	}
	workers.Wait()
	require.GreaterOrEqual(t, len(selectedBudgets), 2, "both deliberately held registered leaves must expose their original allowance")
	for _, observed := range selectedBudgets {
		require.Equal(t, originalDeadline, observed.ctx.Value(projectedSourceDeadlineKey{}), "a COW retry cannot reset the original fixture deadline")
		require.Same(t, allowances[observed.tool], observed.ctx.Value(projectedSourceAllowanceKey{}), "all attempts keep the original two-attempt allowance")
	}
	physical, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, raw, physical, "physical witness follows the controlled registered burst")
	winner := liveEntry(pendingFileProjections, path)
	require.NotNil(t, winner)
	require.NotSame(t, entry, winner)
	require.Zero(t, winner.commitSeq)
	if external {
		require.NotSame(t, entry.layer, winner.layer)
	} else {
		require.Same(t, entry.layer, winner.layer)
	}
	require.True(t, entry.suspect.Load())
	require.ErrorIs(t, validateRequestProjections(oldCtx), projectionSuperseded, "another read cannot revive the old context")
	require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "no canonical publication or fabricated Done result changed this control")
	t.Logf("remaining original whole-call allowance=%s", time.Until(originalDeadline))
	for _, tool := range tools {
		if tool == "read_file" {
			continue
		}
		t.Logf("actual registered tool=%s attempts=%d result=%s", tool, allowances[tool].attempts.Load(), resultText(results[tool]))
		require.GreaterOrEqual(t, allowances[tool].attempts.Load(), int32(1))
		require.LessOrEqual(t, allowances[tool].attempts.Load(), int32(2), "all retry paths share the original two-attempt allowance")
	}
	if nativePopulation {
		metadata := results["get_symbol"]
		require.False(t, metadata.IsError, resultText(metadata))
		node := extractTextResult(t, metadata)["node"].(map[string]any)
		expectedLine := 8
		if external {
			expectedLine = 10
		}
		require.Equal(t, "main.go::Neighbor", node["id"])
		require.EqualValues(t, expectedLine, node["start_line"])
		require.EqualValues(t, expectedLine, node["end_line"])
		requireCanonicalDefinitionRows(t, "get_editing_context", results["get_editing_context"], raw)

		file := results["read_file"]
		require.False(t, file.IsError, resultText(file))
		require.Equal(t, string(raw), extractTextResult(t, file)["content"])
		require.EqualValues(t, len(raw), extractTextResult(t, file)["bytes"])
	}
	source := results["get_symbol_source"]
	require.False(t, source.IsError, resultText(source))
	require.Equal(t, body, extractTextResult(t, source)["source"])
	for _, tool := range []string{"get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) { requireCanonicalDefinitionRows(t, tool, results[tool], raw) })
	}
}
