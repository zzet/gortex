package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	"go.uber.org/zap"
)

// Delegate the REAL completed SQLite write before holding its caller. This
// point is after apply/sidecars/read-version acceptance, before resolver catchup.
// No SQL transaction/writeMu/parse lease is held by this wrapper's channel.
type sharedReceiptFreshContractStore struct {
	*store_sqlite.Store
	armed   atomic.Bool
	applied chan struct{}
	release chan struct{}
	unblock func()
}

func (s *sharedReceiptFreshContractStore) BulkSetFileMtimes(repo string, mtimes map[string]int64) error {
	if err := s.Store.BulkSetFileMtimes(repo, mtimes); err != nil {
		return err
	}
	if s.armed.Load() && s.GetNode("main.go::Stream1Edit00") != nil && s.armed.CompareAndSwap(true, false) {
		close(s.applied)
		<-s.release
	}
	return nil
}
func newSharedReceiptFreshContractFixture(t *testing.T) (*sharedReceiptFixture, *sharedReceiptFreshContractStore, *sharedReceiptFreshContractWatcher) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Target() {}\n// stream-0-tail\n// stream-1-tail\n"), 0600))
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "tier-gap.sqlite"))
	require.NoError(t, err)
	closeStore := sync.OnceFunc(func() { require.NoError(t, sql.Close()) })
	t.Cleanup(closeStore)
	store := &sharedReceiptFreshContractStore{Store: sql, applied: make(chan struct{}), release: make(chan struct{})}
	store.unblock = sync.OnceFunc(func() { close(store.release) })
	logger := zap.NewNop()
	idx := indexer.New(store, testRegistry(), config.Default().Index, logger)
	closeIndexer := sync.OnceFunc(idx.Close)
	t.Cleanup(closeIndexer)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, logger, nil)
	closeLog := sync.OnceFunc(func() { s.DrainBackground(); s.queryLog.Close() })
	t.Cleanup(closeLog)
	s.editAckMode = config.EditAckLocal
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	closeWatcher := sync.OnceFunc(func() { require.NoError(t, watcher.Stop()) })
	t.Cleanup(closeWatcher)
	require.NoError(t, watcher.Start([]string{root}))
	observed := &sharedReceiptFreshContractWatcher{sharedReceiptWatcher: &sharedReceiptWatcher{watcher}, path: filepath.Join(root, "main.go"), pending: make(chan struct{})}
	s.SetWatcher(observed)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	f := &sharedReceiptFixture{server: s, root: root}
	var sessions []string
	f.close = sync.OnceFunc(func() {
		store.unblock()
		for _, id := range sessions {
			s.DisconnectSession(id)
		}
		cancel()
		closeWatcher()
		s.DrainBackground()
		closeIndexer()
		closeLog()
		closeStore()
	})
	t.Cleanup(f.Close)
	for agent := range f.contexts {
		id := fmt.Sprintf("tier-gap-stream-%d", agent)
		require.NoError(t, s.ConnectSession(id, func([]byte) error { return nil }))
		sessions = append(sessions, id)
		f.contexts[agent] = s.WithClientSession(WithSessionID(ctx, id), id)
		s.NoteSessionToolPolicy(id, "all", "defer")
		require.NotNil(t, s.MCPServer().HandleMessage(f.contexts[agent], []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"tier-gap-contract","version":"1"}}}`)))
	}
	return f, store, observed
}
func sharedReceiptFreshContractPublication(t *testing.T, ctx context.Context, f *sharedReceiptFixture, offer *sharedReceiptOffer) (*indexer.MutationTicket, *mutationReceipt) {
	t.Helper()
	require.NoError(t, offer.err)
	var ticket *indexer.MutationTicket
	select {
	case ticket = <-offer.ticket:
	case <-ctx.Done():
		t.Fatal("real own admission absent within original observation deadline")
	}
	record, found := f.server.mutationCommits.byReceipt(offer.receipt)
	require.True(t, found)
	record.mu.RLock()
	key := record.publicationReceipt
	requested := record.reindexGeneration
	record.mu.RUnlock()
	value, found := f.server.mutationReceipts.Load(key)
	require.True(t, found)
	receipt, ok := value.(*mutationReceipt)
	require.True(t, ok)
	require.Same(t, ticket, receipt.ticket)
	require.Equal(t, ticket.Generation, requested)
	return ticket, receipt
}
func sharedReceiptFreshContractOwnEdges(t *testing.T, result *mcplib.CallToolResult, own string) (int, map[string]json.RawMessage) {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, resultText(result))
	body, err := sharedReceiptJSONBody(result)
	require.NoError(t, err)
	var edges []graph.Edge
	require.NoError(t, json.Unmarshal(body["edges"], &edges))
	n := 0
	for _, e := range edges {
		if e.From == own && e.To == "main.go::Target" && e.Kind == graph.EdgeCalls {
			n++
		}
	}
	return n, body
}

// Observe the REAL watcher snapshot, unchanged. This is a test-only interface
// adapter, not a synthetic ready/pending map or a new production wait hook.
type sharedReceiptFreshContractWatcher struct {
	*sharedReceiptWatcher
	path         string
	generation   atomic.Uint64
	armed        atomic.Bool
	once         sync.Once
	pending      chan struct{}
	pendingState indexer.MutationTierState
}

func (w *sharedReceiptFreshContractWatcher) TierPaths() map[string]indexer.MutationTierState {
	paths := w.Watcher.TierPaths()
	if state, ok := paths[w.path]; ok && w.armed.Load() && sharedReceiptInsideActualFreshWait() && state.Generation >= w.generation.Load() && !state.AckAt.IsZero() && state.RelationsAt.IsZero() && !state.Failed {
		w.once.Do(func() { w.pendingState = state; close(w.pending) })
	}
	return paths
}

// The watcher also serves pre-handler/decoration snapshots. Signal ONLY the
// existing actual awaitWatchedFreshness call stack, so an early snapshot cannot
// release B and turn this into a flag-map mirror. This observes the test call;
// it does not add a production hook, modify a map or block the read extractor.
func sharedReceiptInsideActualFreshWait() bool {
	var pcs [16]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".(*Server).awaitWatchedFreshness") {
			return true
		}
		if !more {
			return false
		}
	}
}

// The exact argument helper used by the C4 workload goes through registered
// dispatch and the existing related-pending freshness wait. The OS writes,
// SQLite apply, watcher ticket, resolver and first caller response are real.
// This pins the request contract, not a latency result or a general promise
// that freshness can succeed while unrelated future writes keep arriving.
func TestSharedReceiptCallersWaitForFreshRelationsWithinOriginalDeadline(t *testing.T) {
	selectedProjectionRegistry(t)
	t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	f, store, watched := newSharedReceiptFreshContractFixture(t)
	// The fixture already registers Close; the later RPC join-cleanup must run
	// first by t.Cleanup LIFO, never via an earlier function defer.
	const own = "main.go::Stream0Edit00"
	a := &sharedReceiptOffer{agent: 0, ordinal: 0, ticket: make(chan *indexer.MutationTicket, 1)}
	f.edit(a)
	// Exactly the workload's observation bound: starts after own edit ACK and
	// is reused through own completion/status, caller and planned impact.
	ctx, cancel := context.WithTimeout(context.WithValue(f.contexts[0], sharedReceiptOfferKey{}, a), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	ticketA, receiptA := sharedReceiptFreshContractPublication(t, ctx, f, a)
	select {
	case <-receiptA.done:
	case <-ctx.Done():
		t.Fatal("A did not complete within original5s")
	}
	require.False(t, ticketA.TierState().RelationsAt.IsZero())
	status, err := sharedReceiptJSON(f.server, ctx, 200, "mutation_status", map[string]any{"receipt": a.receipt, "format": "json", "max_bytes": 0})
	require.NoError(t, err)
	var tiers map[string]string
	require.NoError(t, json.Unmarshal(status["tier_state"], &tiers))
	require.Equal(t, "fresh", tiers["relations"])
	// Independent setup: A is genuinely indexed and checked-retired before B.
	// The existing method verifies canonical bytes/nodes; no registry erasure,
	// fabricated tier/receipt or claim to reproduce R26 is involved.
	ownOut := store.GetOutEdges(own)
	require.Len(t, ownOut, 1)
	require.Equal(t, "main.go::Target", ownOut[0].To)
	f.server.canonicalProjectionRetirement(filepath.Join(f.root, "main.go")).run()
	require.Nil(t, liveEntry(pendingFileProjections, filepath.Join(f.root, "main.go")))
	store.armed.Store(true)
	b := &sharedReceiptOffer{agent: 1, ordinal: 0, ticket: make(chan *indexer.MutationTicket, 1)}
	bctx, cancelB := context.WithDeadline(f.contexts[1], deadline)
	defer cancelB()
	f.contexts[1] = bctx
	f.edit(b)
	ticketB, receiptB := sharedReceiptFreshContractPublication(t, ctx, f, b)
	select {
	case <-store.applied:
	case <-ctx.Done():
		t.Fatal("B did not reach actual post-apply/pre-resolve checkpoint")
	}
	require.True(t, ticketB.TierState().RelationsAt.IsZero())
	select {
	case <-receiptB.done:
		t.Fatal("B cannot be complete at held resolver boundary")
	default:
	}
	watched.generation.Store(ticketB.Generation)
	watched.armed.Store(true)
	type reply struct {
		result *mcplib.CallToolResult
		err    error
		B      indexer.MutationTierState
	}
	first := make(chan reply, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		store.unblock()
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("registered first caller did not join after release/cancel")
		}
		f.Close()
	})
	go func() {
		defer close(joined)
		result, err := externalVersionRegisteredCall(f.server, ctx, 300, "get_callers", sharedReceiptCallersArguments())
		first <- reply{result, err, ticketB.TierState()}
	}()
	select {
	case <-watched.pending:
	case early := <-first:
		earlyRaw, marshalErr := json.Marshal(early.result)
		if marshalErr == nil {
			t.Logf("C4_FRESH_RELATIONS_CONTRACT_FIRST_REPLY %s", earlyRaw)
		} else {
			t.Logf("C4_FRESH_RELATIONS_CONTRACT_FIRST_REPLY_MARSHAL_ERROR %v", marshalErr)
		}
		store.unblock()
		t.Fatalf("first caller answered before visiting actual related pending B: %v", early.err)
	case <-ctx.Done():
		store.unblock()
		t.Fatal("registered fresh caller did not observe actual related pending scope")
	}
	require.True(t, watched.pendingState.RelationsAt.IsZero())
	require.GreaterOrEqual(t, watched.pendingState.Generation, ticketB.Generation)
	// The observed real pending snapshot is already returned to the product;
	// release ONLY B's existing graph writer, never alter that snapshot/result.
	store.unblock()
	var got reply
	select {
	case got = <-first:
	case <-ctx.Done():
		t.Fatal("first caller exceeded the SAME original5s deadline")
	}
	raw, err := json.Marshal(got.result)
	require.NoError(t, err)
	t.Logf("C4_FRESH_RELATIONS_CONTRACT_FIRST_REPLY %s", raw)
	require.NoError(t, got.err)
	count, body := sharedReceiptFreshContractOwnEdges(t, got.result, own)
	require.Equal(t, 1, count)
	require.False(t, got.B.RelationsAt.IsZero(), "first reply must follow actual B relations completion")
	var freshness struct {
		Fresh    bool   `json:"fresh"`
		Scope    string `json:"freshness_scope"`
		WaitedMS int64  `json:"waited_ms"`
		Deadline string `json:"wait_deadline"`
	}
	require.NoError(t, json.Unmarshal(body["freshness"], &freshness))
	require.True(t, freshness.Fresh, "registered request must have used the real explicit-fresh outcome, not default partial decoration")
	require.Equal(t, "relations", freshness.Scope)
	require.Positive(t, freshness.WaitedMS, "actual pending B must be waited on, not a mocked argument mirror")
	actualDeadline, err := time.Parse(time.RFC3339, freshness.Deadline)
	require.NoError(t, err)
	require.False(t, actualDeadline.After(deadline), "wait cannot renew original deadline")
	stillOriginal, _ := ctx.Deadline()
	require.True(t, stillOriginal.Equal(deadline))
	select {
	case <-receiptB.done:
	case <-ctx.Done():
		t.Fatal("released B did not complete within SAME original bound")
	}
	// Original planned impact stays unmodified, once, after normal B completion.
	impact, err := sharedReceiptJSON(f.server, ctx, 400, "explain_change_impact", map[string]any{"ids": "main.go::Target", "format": "json", "max_bytes": 0, "limit": 100})
	require.NoError(t, err)
	require.NoError(t, sharedReceiptOwnImpact(impact, own))
}
