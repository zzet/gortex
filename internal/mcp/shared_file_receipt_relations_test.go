package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type sharedReceiptOfferKey struct{}

type sharedReceiptOffer struct {
	agent, ordinal int
	population     string
	ticket         chan *indexer.MutationTicket
	ticketObserved sync.Once
	due            time.Time
	started        time.Time
	relationsAt    time.Time
	finished       time.Time
	receipt        string
	generation     uint64
	applied        uint64
	tiers          indexer.MutationTierState
	wire           map[string]string
	content        map[string]json.RawMessage
	diagnostic     map[string]any
	err            error
}

// This observer forwards admission unchanged. The actual request context binds
// one offered write to its own production ticket, never the latest path ticket.
type sharedReceiptWatcher struct{ *indexer.Watcher }

func (w *sharedReceiptWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	ticket, err := w.Watcher.EnqueueToolMutation(ctx, path)
	if offer, ok := ctx.Value(sharedReceiptOfferKey{}).(*sharedReceiptOffer); ok && ticket != nil {
		// Read-built catch-up inherits the offer context too. Keep only its
		// first native ticket; observation must never block later admissions.
		offer.ticketObserved.Do(func() { offer.ticket <- ticket })
	}
	return ticket, err
}

type sharedReceiptFixture struct {
	server   *Server
	root     string
	native   *observer.ObservedLogs
	contexts [2]context.Context
	close    func()
}

func (f *sharedReceiptFixture) Close() { f.close() }

func newSharedReceiptFixture(tb testing.TB) *sharedReceiptFixture {
	tb.Helper()
	root, err := filepath.EvalSymlinks(tb.TempDir())
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Target() {}\n// stream-0-tail\n// stream-1-tail\n"), 0600))
	storeDir := tb.TempDir()
	store, err := store_sqlite.Open(filepath.Join(storeDir, "shared.sqlite"))
	require.NoError(tb, err)
	closeStore := sync.OnceFunc(func() { require.NoError(tb, store.Close()) })
	tb.Cleanup(closeStore)
	logger := zap.NewNop()
	var native *observer.ObservedLogs
	if os.Getenv("GORTEX_TEST_SHARED_RECEIPT_NATIVE_LOGS") == "1" {
		core, logs := observer.New(zap.DebugLevel)
		logger, native = zap.New(core), logs
	}
	idx := indexer.New(store, testRegistry(), config.Default().Index, logger)
	closeIndexer := sync.OnceFunc(idx.Close)
	tb.Cleanup(closeIndexer)
	_, err = idx.Index(root)
	require.NoError(tb, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, logger, nil)
	closeLog := sync.OnceFunc(func() { s.queryLog.Close() })
	tb.Cleanup(closeLog)
	s.editAckMode = config.EditAckLocal
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(tb, err)
	require.NoError(tb, watcher.Start([]string{root}))
	s.SetWatcher(&sharedReceiptWatcher{watcher})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f := &sharedReceiptFixture{server: s, root: root, native: native}
	var sessions []string
	f.close = sync.OnceFunc(func() {
		for _, session := range sessions {
			s.DisconnectSession(session)
		}
		cancel()
		require.NoError(tb, watcher.Stop())
		s.DrainBackground()
		closeIndexer()
		closeLog()
		closeStore()
		require.NoError(tb, os.RemoveAll(root))
		require.NoError(tb, os.RemoveAll(storeDir))
	})
	tb.Cleanup(f.Close)
	for agent := range f.contexts {
		session := fmt.Sprintf("shared-file-stream-%d", agent)
		require.NoError(tb, s.ConnectSession(session, func([]byte) error { return nil }))
		sessions = append(sessions, session)
		f.contexts[agent] = s.WithClientSession(WithSessionID(ctx, session), session)
		s.NoteSessionToolPolicy(session, "all", "defer")
		reply := s.MCPServer().HandleMessage(f.contexts[agent], []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"shared-file-receipts","version":"1"}}}`))
		require.NotNil(tb, reply)
	}
	return f
}

func sharedReceiptJSON(s *Server, ctx context.Context, id int, tool string, args map[string]any) (map[string]json.RawMessage, error) {
	result, err := externalVersionRegisteredCall(s, ctx, id, tool, args)
	if err != nil {
		return nil, err
	}
	if offer, ok := ctx.Value(sharedReceiptOfferKey{}).(*sharedReceiptOffer); ok {
		if offer.wire == nil {
			offer.wire = make(map[string]string)
			offer.content = make(map[string]json.RawMessage)
		}
		offer.wire[tool] = resultText(result)
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, marshalErr
		}
		offer.content[tool] = raw
	}
	if result.IsError {
		return nil, fmt.Errorf("%s refused: %s", tool, resultText(result))
	}
	body, err := sharedReceiptJSONBody(result)
	if err != nil {
		return nil, fmt.Errorf("%s JSON: %w", tool, err)
	}
	return body, nil
}

// JSON is the first content block. The production momentum middleware appends
// separate session-note blocks; concatenating those with JSON invents an error.
func sharedReceiptJSONBody(result *mcplib.CallToolResult) (map[string]json.RawMessage, error) {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return nil, fmt.Errorf("missing or failed primary JSON content")
	}
	text, ok := result.Content[0].(mcplib.TextContent)
	if !ok {
		return nil, fmt.Errorf("primary content is not text")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("primary content is not a JSON object")
	}
	return body, nil
}

// The real own receipt/ticket has already certified relations. Later edits can
// label the overall graph pending without erasing a present exact own record.
// Unknown incompleteness, truncation, lower bounds and a missing own row fail.
func sharedReceiptOwnImpact(impact map[string]json.RawMessage, want string) error {
	var depths map[string][]struct{ ID string }
	var completeValue, truncatedValue *bool
	var lowerBound bool
	if err := json.Unmarshal(impact["by_depth"], &depths); err != nil {
		return err
	}
	if err := json.Unmarshal(impact["complete"], &completeValue); err != nil {
		return err
	}
	if err := json.Unmarshal(impact["truncated"], &truncatedValue); err != nil {
		return err
	}
	if completeValue == nil || truncatedValue == nil {
		return fmt.Errorf("missing typed impact completeness/truncation")
	}
	complete, truncated := *completeValue, *truncatedValue
	if raw, present := impact["lower_bound"]; present {
		if err := json.Unmarshal(raw, &lowerBound); err != nil {
			return err
		}
	}
	var freshness string
	if raw, present := impact["graph_freshness"]; present {
		if err := json.Unmarshal(raw, &freshness); err != nil {
			return err
		}
	}
	exact := 0
	for _, entry := range depths["1"] {
		if entry.ID == want {
			exact++
		}
	}
	if truncated || lowerBound || exact != 1 || !complete && freshness != "pending" {
		return fmt.Errorf("own impact %s not exact: complete=%v truncated=%v lower_bound=%v matches=%d", want, complete, truncated, lowerBound, exact)
	}
	return nil
}

func (f *sharedReceiptFixture) edit(offer *sharedReceiptOffer) {
	ctx := context.WithValue(f.contexts[offer.agent], sharedReceiptOfferKey{}, offer)
	marker := fmt.Sprintf("// stream-%d-tail", offer.agent)
	name := fmt.Sprintf("Stream%dEdit%02d", offer.agent, offer.ordinal)
	offer.started = time.Now()
	body, err := sharedReceiptJSON(f.server, ctx, 100+offer.ordinal, "edit_file", map[string]any{
		"path": "main.go", "old_string": marker, "new_string": "func " + name + "() { Target() }\n" + marker,
		"expected_occurrences": 1, "format": "json", "max_bytes": 0,
	})
	if err != nil {
		offer.err = err
		return
	}
	if err := json.Unmarshal(body["mutation_receipt"], &offer.receipt); err != nil || offer.receipt == "" {
		offer.err = fmt.Errorf("missing own physical receipt: %s", body["mutation_receipt"])
		return
	}
}

// C4 requests current relations explicitly; completion of one receipt does
// not freeze later same-file edits. Historical C4 comparisons require the
// same declared request contract on both sides; unflagged runs stay historical.
func sharedReceiptCallersArguments() map[string]any {
	return map[string]any{"id": "main.go::Target", "format": "json", "max_bytes": 0, "require_fresh": true, "freshness_scope": "relations"}
}

func (f *sharedReceiptFixture) observe(offer *sharedReceiptOffer) {
	if offer.err != nil {
		f.captureFailure(f.contexts[offer.agent], offer)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(f.contexts[offer.agent], sharedReceiptOfferKey{}, offer), 5*time.Second)
	defer cancel()
	defer func() {
		if offer.err != nil {
			f.captureFailure(ctx, offer)
		}
	}()
	var ticket *indexer.MutationTicket
	select {
	case ticket = <-offer.ticket:
	case <-ctx.Done():
		offer.err = fmt.Errorf("own admission missing: %w", ctx.Err())
		return
	}
	offer.generation = ticket.Generation
	record, found := f.server.mutationCommits.byReceipt(offer.receipt)
	if !found {
		offer.err = fmt.Errorf("own physical receipt %s was lost", offer.receipt)
		return
	}
	// The receipt's done channel broadcasts completion to all observers. Do
	// not consume ticket.Done, which the real receipt consumer owns, or poll
	// status rendering while publication is running.
	record.mu.RLock()
	publicationReceipt := record.publicationReceipt
	record.mu.RUnlock()
	value, found := f.server.mutationReceipts.Load(publicationReceipt)
	receipt, valid := value.(*mutationReceipt)
	if !found || !valid || receipt.ticket != ticket || receipt.generation != offer.generation {
		offer.err = fmt.Errorf("own physical receipt %s lacks its admitted publication receipt", offer.receipt)
		return
	}
	select {
	case <-receipt.done:
	case <-ctx.Done():
		offer.err = fmt.Errorf("own receipt did not finish: %w", ctx.Err())
		return
	}
	statusAfterCompletion := f.server.mutationStatusPayload(record)
	if statusAfterCompletion["graph_status"] != mutationGraphFresh {
		offer.err = fmt.Errorf("own completed receipt is not fresh: %v", statusAfterCompletion)
		return
	}
	state := ticket.TierState()
	offer.tiers = state
	offer.relationsAt = state.RelationsAt
	record.mu.RLock()
	offer.applied = record.appliedGeneration
	requested, sequence, owner := record.reindexGeneration, record.commitSeq, record.owner
	record.mu.RUnlock()
	if owner != f.server || sequence == 0 || requested != offer.generation || state.RelationsAt.IsZero() || state.Failed || offer.applied < offer.generation {
		offer.err = fmt.Errorf("own ticket lacks successful covering relations: generation=%d applied=%d tiers=%+v", offer.generation, offer.applied, state)
		return
	}
	status, err := sharedReceiptJSON(f.server, ctx, 200+offer.ordinal, "mutation_status", map[string]any{"receipt": offer.receipt, "format": "json", "max_bytes": 0})
	if err != nil {
		offer.err = err
		return
	}
	var tiers map[string]string
	var publicReceipt string
	if err := json.Unmarshal(status["receipt"], &publicReceipt); err != nil || publicReceipt != offer.receipt {
		offer.err = fmt.Errorf("public status changed the original receipt: %s", status["receipt"])
		return
	}
	if err := json.Unmarshal(status["tier_state"], &tiers); err != nil || tiers["relations"] != "fresh" {
		offer.err = fmt.Errorf("public own receipt stays relations-pending after covering store commit: %s", status["tier_state"])
		return
	}
	name := fmt.Sprintf("Stream%dEdit%02d", offer.agent, offer.ordinal)
	want := "main.go::" + name
	callers, err := sharedReceiptJSON(f.server, ctx, 300+offer.ordinal, "get_callers", sharedReceiptCallersArguments())
	if err != nil {
		offer.err = err
	} else {
		var edges []struct{ From, To, Kind string }
		if decodeErr := json.Unmarshal(callers["edges"], &edges); decodeErr != nil {
			offer.err = decodeErr
		} else {
			exact := 0
			for _, edge := range edges {
				if edge.From == want && edge.To == "main.go::Target" && edge.Kind == "calls" {
					exact++
				}
			}
			if exact != 1 {
				offer.err = fmt.Errorf("own caller %s has %d exact public edges", want, exact)
			}
		}
	}
	if offer.err != nil {
		// Preserve the first failed response and its immediate state. The
		// already-planned impact query below is not a caller success retry.
		f.captureFailure(ctx, offer)
	}
	impact, err := sharedReceiptJSON(f.server, ctx, 400+offer.ordinal, "explain_change_impact", map[string]any{"ids": "main.go::Target", "format": "json", "max_bytes": 0, "limit": 100})
	if err != nil {
		offer.err = errors.Join(offer.err, err)
		return
	}
	if err := sharedReceiptOwnImpact(impact, want); err != nil {
		offer.err = errors.Join(offer.err, err)
		return
	}
	offer.finished = time.Now()
}

// Failure-only observations cannot turn a failed original reply into success.
// These independent reads are explicitly not an atomic snapshot; all original
// public replies remain separate, without a diagnostic RPC or success retry.
func (f *sharedReceiptFixture) captureFailure(ctx context.Context, offer *sharedReceiptOffer) {
	if offer.diagnostic != nil {
		return
	}
	want := fmt.Sprintf("main.go::Stream%dEdit%02d", offer.agent, offer.ordinal)
	reader := f.server.engineFor(ctx).Reader()
	d := map[string]any{"at": time.Now(), "edit_started": offer.started, "relations_at": offer.relationsAt, "error": offer.err.Error(), "receipt": offer.receipt,
		"generation": offer.generation, "applied": offer.applied, "ticket_tiers": offer.tiers,
		"reader_type": fmt.Sprintf("%T", reader), "request_reader_node": reader.GetNode(want),
		"request_reader_in": reader.GetInEdges("main.go::Target"), "request_reader_out": reader.GetOutEdges(want),
		"canonical_node": f.server.graph.GetNode(want), "canonical_file_nodes": f.server.graph.GetFileNodes("main.go"),
		"canonical_in": f.server.graph.GetInEdges("main.go::Target"), "canonical_out": f.server.graph.GetOutEdges(want),
		"read_sequence_independent_not_atomic": true}
	if f.native != nil {
		var logs []map[string]any
		for _, entry := range f.native.All() {
			if entry.Time.Before(offer.started.Add(-250 * time.Millisecond)) {
				continue
			}
			if strings.HasPrefix(entry.Message, "resolver: incremental") || strings.Contains(entry.Message, "Go package preparation failed") || strings.HasPrefix(entry.Message, "watcher: point publication") {
				logs = append(logs, map[string]any{"at": entry.Time, "message": entry.Message, "fields": entry.ContextMap()})
			}
		}
		d["native_logs_overflow"] = len(logs) > 256
		if len(logs) > 256 {
			logs = logs[len(logs)-256:]
		}
		d["native_pipeline_logs"] = logs
	}
	path := filepath.Join(f.root, "main.go")
	before, beforeErr := os.Stat(path)
	raw, readErr := os.ReadFile(path)
	after, afterErr := os.Stat(path)
	d["physical_read_error"] = fmt.Sprint(readErr)
	if readErr == nil {
		d["physical_bytes"] = len(raw)
		d["physical_sha256"] = fmt.Sprintf("%x", sha256.Sum256(raw))
		if len(raw) <= 8192 {
			d["physical_source"] = string(raw)
		} else {
			d["physical_source_omitted_over_8192"] = true
		}
	}
	if beforeErr == nil && afterErr == nil {
		d["physical_before_mtime_ns"], d["physical_after_mtime_ns"] = before.ModTime().UnixNano(), after.ModTime().UnixNano()
		d["physical_before_size"], d["physical_after_size"] = before.Size(), after.Size()
		d["physical_same_file"] = os.SameFile(before, after)
		d["physical_stable_metadata"] = os.SameFile(before, after) && before.ModTime().Equal(after.ModTime()) && before.Size() == after.Size()
	} else {
		d["physical_stat_errors"] = fmt.Sprintf("%v / %v", beforeErr, afterErr)
	}
	if record, found := f.server.mutationCommits.byReceipt(offer.receipt); found {
		d["physical_commit"] = record.snapshot()
	}
	offer.diagnostic = d
}

func runSharedReceiptPopulation(tb testing.TB, loaded bool, perAgent int) []*sharedReceiptOffer {
	tb.Helper()
	f := newSharedReceiptFixture(tb)
	defer f.Close() // end every population before the next one starts
	rows := make([]*sharedReceiptOffer, 0, 2*perAgent)
	for ordinal := range perAgent {
		for agent := range 2 {
			population := "idle"
			if loaded {
				population = "loaded"
			}
			rows = append(rows, &sharedReceiptOffer{agent: agent, ordinal: ordinal, population: population, ticket: make(chan *indexer.MutationTicket, 1)})
		}
	}
	if !loaded {
		for _, offer := range rows {
			f.edit(offer)
			f.observe(offer)
		}
		return rows
	}
	start := time.Now()
	var streams, observers sync.WaitGroup
	for agent := range 2 {
		streams.Add(1)
		go func() {
			defer streams.Done()
			for ordinal := range perAgent {
				offer := rows[2*ordinal+agent]
				offer.due = start.Add(time.Duration(ordinal) * 250 * time.Millisecond)
				time.Sleep(time.Until(offer.due))
				observers.Add(1)
				go func() {
					defer observers.Done()
					f.edit(offer)
					f.observe(offer)
				}()
			}
		}()
	}
	streams.Wait()
	observers.Wait()
	return rows
}

func sharedReceiptComparison(tb testing.TB, perAgent int, enforceLatency bool) float64 {
	tb.Helper()
	retainReferenceRaw := os.Getenv("GORTEX_TEST_SHARED_RECEIPT_REFERENCE_RAW") == "1"
	if retainReferenceRaw {
		profile := map[string]any{"pid": os.Getpid(), "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "goenv": map[string]string{"GOMAXPROCS": os.Getenv("GOMAXPROCS"), "GOMEMLIMIT": os.Getenv("GOMEMLIMIT"), "GOGC": os.Getenv("GOGC"), "GODEBUG": os.Getenv("GODEBUG")}}
		raw, err := json.Marshal(profile)
		require.NoError(tb, err)
		tb.Logf("reference own receipt process: %s", raw)
	}
	tb.Setenv("GORTEX_QUERY_LOG", filepath.Join(tb.TempDir(), "query-log.jsonl"))
	tb.Setenv(batchTransactionDirEnv, filepath.Join(tb.TempDir(), "transactions"))
	idle := runSharedReceiptPopulation(tb, false, perAgent)
	loaded := runSharedReceiptPopulation(tb, true, perAgent)
	if retainReferenceRaw {
		for _, population := range [][]*sharedReceiptOffer{idle, loaded} {
			for _, row := range population {
				// Both populations and all callbacks are already joined. Emit
				// their buffered first replies before any fail-fast assertion;
				// never add a read, retry, or work inside the native metric.
				var nativeElapsed any
				if !row.relationsAt.IsZero() {
					nativeElapsed = row.relationsAt.Sub(row.started).Nanoseconds()
				}
				evidence := map[string]any{"population": row.population, "stream": row.agent, "edit": row.ordinal, "receipt": row.receipt, "generation": row.generation, "applied_generation": row.applied, "offered_at": row.started, "relations_at": row.relationsAt, "native_elapsed_ns": nativeElapsed, "readbacks_finished_at": row.finished, "error": fmt.Sprint(row.err), "first_responses": row.content}
				raw, err := json.Marshal(evidence)
				require.NoError(tb, err)
				tb.Logf("reference own receipt first replies: %s", raw)
			}
		}
	}
	for _, population := range [][]*sharedReceiptOffer{idle, loaded} {
		for _, row := range population {
			if row.err != nil {
				raw, err := json.Marshal(map[string]any{"population": row.population, "stream": row.agent, "edit": row.ordinal, "original_replies": row.wire, "original_content": row.content, "first_failure_snapshot": row.diagnostic})
				require.NoError(tb, err)
				tb.Logf("failed own receipt: %s", raw)
			}
		}
	}
	var maximum float64
	seen := make(map[string]bool, len(loaded))
	for i, row := range loaded {
		require.NoError(tb, idle[i].err, "idle stream=%d edit=%d", row.agent, row.ordinal)
		require.NoError(tb, row.err, "loaded stream=%d edit=%d", row.agent, row.ordinal)
		require.False(tb, seen[row.receipt], "a receipt may not be borrowed by another offered edit")
		seen[row.receipt] = true
		base, actual := idle[i].relationsAt.Sub(idle[i].started), row.relationsAt.Sub(row.started)
		require.Positive(tb, base)
		ratio := float64(actual) / float64(base)
		maximum = max(maximum, ratio)
		tb.Logf("own receipt stream=%d edit=%d receipt=%s ticket=%d applied=%d arrival_lag=%s idle_relations=%s shared_relations=%s exact_readbacks=%s ratio=%.3f", row.agent, row.ordinal, row.receipt, row.generation, row.applied, row.started.Sub(row.due), base, actual, row.finished.Sub(row.started), ratio)
		if enforceLatency {
			require.LessOrEqual(tb, actual, 3*base, "every own receipt must reach exact callers+impact within three times its matching idle edit")
		}
	}
	return maximum
}

func TestSharedFileOwnReceiptsReachExactRelationsAtFourEditsPerSecond(t *testing.T) {
	sharedReceiptComparison(t, 8, false)
}

// A real run of this benchmark supplies the separate bench evidence. No pooled
// percentile or successful subset can hide one receipt's latency or refusal.
func BenchmarkSharedFileOwnReceiptRelationsAtFourEditsPerSecond(b *testing.B) {
	for range b.N {
		maximum := sharedReceiptComparison(b, 8, true)
		b.ReportMetric(maximum, "max-own-idle-ratio")
	}
}
