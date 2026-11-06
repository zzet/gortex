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
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

type sameOwnerReadCOWStore struct {
	*store_sqlite.Store
	armed     atomic.Bool
	arrivals  atomic.Int32
	gateReads int32
	captured  chan *projectionEntry
	release   chan struct{}
	path      string
}

func (s *sameOwnerReadCOWStore) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	return s.FileMetasByPathsContext(context.Background(), prefix, paths)
}

func (s *sameOwnerReadCOWStore) FileMetasByPathsContext(ctx context.Context, prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	rows, err := s.Store.FileMetasByPathsContext(ctx, prefix, paths)
	if err == nil && s.armed.Load() && s.arrivals.Add(1) <= s.gateReads {
		s.captured <- liveEntry(pendingFileProjections, s.path)
		<-s.release
	}
	return rows, err
}

type checkedReadObservation struct {
	tool  string
	ctx   context.Context
	entry *projectionEntry
}

// The actual daemon admits independent Fast requests from one connection.
// All three reads see the same already-observed source event and capture the
// same ACK owner before any same-byte read-COW publication can win its CAS.
func TestConcurrentRegisteredReadsRevalidateSameACKOwner(t *testing.T) {
	runConcurrentRegisteredReadPopulation(t, false, false)
}

// Match the native connection's full public population while keeping canonical
// publication held. This distinguishes read-COW contention from a later real
// mutation, source event or checked canonical retirement.
func TestRegisteredConcurrentNativeReadPopulationKeepsCurrentRows(t *testing.T) {
	runConcurrentRegisteredReadPopulation(t, true, false)
}

// A second public read may verify the unchanged source and COW the read-only
// owner while another exact read is assembling its response. The original
// context remains invalid; audited readers must use only their remaining whole
// call allowance, while full metadata still forbids late enrichment replay.
func TestRegisteredReadCOWAfterBorrowUsesOriginalFreshAllowance(t *testing.T) {
	runConcurrentRegisteredReadPopulation(t, true, true)
}

type registeredReadCOWActorKey struct{}
type registeredBorrowObservation struct {
	tool string
	ctx  context.Context
}

func runConcurrentRegisteredReadPopulation(t *testing.T, nativePopulation, afterBorrow bool) {
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
	backend := sql
	capturedReads := make(chan checkedReadObservation, gateReads)
	releaseReads := make(chan struct{})
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
	if !afterBorrow {
		for _, tool := range tools {
			if tool == "read_file" {
				continue
			}
			var once sync.Once
			observeRegisteredCheckedRead(s, tool, func(ctx context.Context, entry *projectionEntry) {
				once.Do(func() {
					capturedReads <- checkedReadObservation{tool: tool, ctx: ctx, entry: entry}
					select {
					case <-releaseReads:
					case <-ctx.Done():
					}
				})
			})
		}
	}
	if afterBorrow {
		for _, name := range tools {
			if name == "read_file" {
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
	require.Equal(t, 5, sql.GetNode("main.go::Neighbor").StartLine, "the independent canonical rows precede this actual local ACK")
	oldCtx, err := retainRequestProjection(withRequestProjectionFacts(t.Context()), entry)
	require.NoError(t, err)
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
		require.Same(t, entry.layer, borrowedOwner.layer)
	}
	budget := projectedReadBudget
	if afterBorrow {
		// Deliberately held leaf/peer/refresh orchestration tests lifecycle,
		// separately from the unchanged production150ms default and expiry.
		budget = canonicalLifecycleFixtureBudget
	}
	originalDeadline = time.Now().Add(budget)
	ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, originalDeadline)
	release := sync.OnceFunc(func() { close(releaseReads) })
	type answer struct {
		tool   string
		result *mcplib.CallToolResult
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
			result := preselectionRegisteredCall(t, s, requestCtx, 20+i, tool, args)
			answers <- answer{tool: tool, result: result}
		}()
	}
	if !afterBorrow {
		seen := make(map[string]bool)
		for range gateReads {
			select {
			case captured := <-capturedReads:
				require.Same(t, entry, captured.entry, "every actual request must capture the same old pointer before the actual CAS")
				require.Equal(t, session, SessionIDFromContext(captured.ctx))
				require.Contains(t, tools, captured.tool)
				require.False(t, seen[captured.tool], "one captured expectation per actual tool request")
				seen[captured.tool] = true
			case <-time.After(3 * time.Second):
				t.Fatal("owned registered reads did not reach the common verified pre-CAS boundary")
			}
		}
		release()
	} else {
		selected := make(map[string]context.Context)
		for range 4 {
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
		t.Logf("post-borrow read-COW old_events=%d current_events=%d old_checked=%v current_checked=%v", borrowedOwner.sourceEvents, current.sourceEvents, borrowedOwner.checkedRetired.Load(), current.checkedRetired.Load())
		pendingFileProjections.mu.Unlock()
		for _, old := range selected {
			require.ErrorIs(t, validateRequestProjections(old), projectionSuperseded, "the new response cannot revive the original context")
		}
		releaseBorrowedOnce()
	}
	results := map[string]*mcplib.CallToolResult{}
	for range tools {
		select {
		case a := <-answers:
			results[a.tool] = a.result
		case <-time.After(3 * time.Second):
			t.Fatal("owned registered read did not join")
		}
	}
	workers.Wait()
	if afterBorrow {
		require.GreaterOrEqual(t, len(selectedBudgets), len(tools)-1, "each held graph leaf must expose its original allowance")
	}
	for _, observed := range selectedBudgets {
		require.Equal(t, originalDeadline, observed.ctx.Value(projectedSourceDeadlineKey{}), "a COW retry cannot reset the original fixture deadline")
		require.Same(t, allowances[observed.tool], observed.ctx.Value(projectedSourceAllowanceKey{}), "all attempts keep the original two-attempt allowance")
	}
	winner := liveEntry(pendingFileProjections, path)
	require.NotNil(t, winner)
	require.NotSame(t, entry, winner)
	require.Zero(t, winner.commitSeq)
	require.Same(t, entry.layer, winner.layer)
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
		if afterBorrow {
			require.True(t, metadata.IsError, "full metadata has no late session/LSP replay authority")
			require.Equal(t, int32(1), allowances["get_symbol"].attempts.Load())
		} else {
			require.False(t, metadata.IsError, resultText(metadata))
			node := extractTextResult(t, metadata)["node"].(map[string]any)
			require.Equal(t, "main.go::Neighbor", node["id"])
			require.EqualValues(t, 8, node["start_line"], "full metadata must carry the actual shifted neighbor")
			require.EqualValues(t, 8, node["end_line"])
		}
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

// An early CAS allowance alone never authorizes replay of active enrichment.
// Inert full-JSON metadata has a separately audited accepted-only contract;
// this guard retains the unsafe on-demand producer shape and strict refusal.
func TestFullSymbolCASAllowanceDoesNotGrantAfterLeafReplay(t *testing.T) {
	selectedProjectionRegistry(t)
	s, path, entry, _ := acknowledgedCanonicalFile(t)
	// The existing no-provider fixture exercises output admission without LSP subprocesses.
	s.semanticMgr = &semantic.Manager{}
	retirement := s.canonicalProjectionRetirement(path)
	require.NotNil(t, retirement)
	calls := 0
	var selected context.Context
	leaf := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		selected = ctx
		result, err := s.handleGetSymbol(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(result))
		require.EqualValues(t, 8, extractTextResult(t, result)["node"].(map[string]any)["start_line"])
		require.True(t, ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt).replayUnsafe, "actual enrichment work must mark the attempt ineligible")
		require.NoError(t, s.indexer.IndexFile(path))
		retirement.run()
		require.True(t, entry.checkedRetired.Load())
		return result, nil
	}
	result, err := s.wrapToolHandler(leaf)(t.Context(), makeReq("get_symbol", map[string]any{"id": "main.go::Neighbor", "detail": "full", "format": "json"}))
	require.NoError(t, err)
	require.True(t, result.IsError, resultText(result))
	require.Equal(t, 1, calls)
	require.Error(t, validateRequestProjections(selected), "a late old selection remains unusable")
	facts := selected.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	facts.mu.RLock()
	old := facts.entries[entry.key]
	facts.mu.RUnlock()
	require.Same(t, entry, old)
}
