package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// This variant uses the actual default daemon constructor and installer, not
// a copy of its wiring. The older MCP component fixture remains separate.
type daemonFreshnessUnrelatedExtractor struct {
	parser.Extractor
	mu        sync.Mutex
	armed     bool
	onlyName  string // nonempty only in the separate related-file negative
	waiting   sync.WaitGroup
	first     sync.Once
	closeOnce sync.Once
	entered   chan string
	release   chan struct{}
	entries   atomic.Uint64
	completed atomic.Uint64
}

func (e *daemonFreshnessUnrelatedExtractor) arm() {
	e.mu.Lock()
	e.armed = true
	e.mu.Unlock()
}
func (e *daemonFreshnessUnrelatedExtractor) hold(path string) func() {
	name := filepath.Base(path)
	if e.onlyName != "" && name != e.onlyName || e.onlyName == "" && name != "other_a.go" && name != "other_b.go" {
		return nil
	}
	e.mu.Lock()
	if !e.armed {
		e.mu.Unlock()
		return nil
	}
	// The same mutex closes admission before releaseAndJoin waits, so Add
	// cannot race a zero-count Wait after the gate has been disarmed.
	e.waiting.Add(1)
	e.entries.Add(1)
	e.mu.Unlock()
	e.first.Do(func() { e.entered <- path })
	<-e.release
	return func() { e.completed.Add(1); e.waiting.Done() }
}
func (e *daemonFreshnessUnrelatedExtractor) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	if done := e.hold(path); done != nil {
		defer done()
	}
	return e.Extractor.Extract(path, raw)
}
func (e *daemonFreshnessUnrelatedExtractor) ExtractWithOptions(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	if done := e.hold(path); done != nil {
		defer done()
	}
	return parser.Extract(e.Extractor, path, raw, options)
}
func (e *daemonFreshnessUnrelatedExtractor) releaseAndJoin() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.armed = false
		close(e.release)
		e.mu.Unlock()
		e.waiting.Wait()
	})
}

func TestDaemonFreshnessUnrelatedTwoEditsPerSecond(t *testing.T) {
	daemonFreshnessDefaultRuntime(t, false)
}

func TestDaemonFreshnessDefaultRuntimeBaselineIdentity(t *testing.T) {
	daemonFreshnessDefaultRuntime(t, true)
}

func daemonFreshnessDefaultRuntime(t *testing.T, baselineOnly bool) {
	daemonFreshnessDefaultRuntimeCase(t, baselineOnly, "")
}

func daemonFreshnessDefaultRuntimeCase(t *testing.T, baselineOnly bool, pendingCase string) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	t.Setenv("GORTEX_TOOLS", "full")
	base := startupPublicationEnv(t)
	// C5 is the ordinary watching configuration. Enable its native backend;
	// debounce is inherited unchanged from config.Default(), not overridden.
	require.NoError(t, os.WriteFile(cfgFile, []byte("semantic:\n  enabled: false\nwatch:\n  enabled: true\n"), 0600))
	root := startupPublicationRepo(t, base, "repo")
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte("semantic:\n  enabled: false\nwatch:\n  enabled: true\n"), 0600))
	require.NoError(t, os.Remove(filepath.Join(root, "main.go")))
	for name, raw := range map[string]string{
		"target.go":  "package p\nfunc Target() int { return 1 }\n",
		"caller.go":  "package p\nfunc Caller() int { return Target() }\n",
		"other_a.go": "package p\nfunc UnrelatedA() int { return 0 }\n",
		"other_b.go": "package p\nfunc UnrelatedB() int { return 0 }\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(raw), 0600))
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "freshness baseline"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	store, ok := state.graph.(*store_sqlite.Store)
	require.True(t, ok)
	gate := &daemonFreshnessUnrelatedExtractor{Extractor: languages.NewGoExtractor(), entered: make(chan string, 1), release: make(chan struct{})}
	if pendingCase == "related_target" {
		gate.onlyName = "target.go"
	}
	// Install the real extractor before indexing or any background producer.
	state.indexer.Registry().Register(gate)
	viewBuilds := indexer.NewViewBuildGate()
	state.lifecycle.SetBuildGate(viewBuilds)
	installBuildLaneBusy(state.graph, viewBuilds)
	controller := &realController{graph: state.graph, indexer: state.indexer, multiIndexer: state.multiIndexer, configManager: state.configManager, lifecycle: state.lifecycle, viewMaterializer: state.mcpServer.Materializer(), buildGate: viewBuilds, logger: zap.NewNop()}
	var runtime *daemonContractAnalysis
	var w *indexer.MultiWatcher
	t.Cleanup(func() {
		gate.releaseAndJoin()
		if runtime != nil {
			runtime.Cancel()
		}
		var watcherErr, runtimeErr error
		if w != nil {
			watcherErr = w.Stop()
		}
		controller.StopWatcher()
		if runtime != nil {
			runtimeErr = runtime.Close(context.Background())
		}
		sharedErr := state.shared.Close()
		require.NoError(t, errors.Join(watcherErr, runtimeErr, sharedErr))
	})
	runtime, err = installDaemonContractAnalysis(state, zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, runtime)
	registered, err := state.lifecycle.Register(t.Context(), config.RepoEntry{Name: "repo", Path: root}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.Equal(t, "repo", registered.Prefix)
	var warmup *warmupTimings
	w, warmup = warmupDaemonStateContext(t.Context(), state, zap.NewNop(), nil, controller.startAndAttachWatcher)
	require.NotNil(t, warmup)
	require.NoError(t, warmup.err)
	require.NotNil(t, w)
	require.True(t, w.TierAuthority("repo"))
	require.Equal(t, config.Default().Watch.DebounceMs, state.configManager.GetRepoConfig("repo").Watch.DebounceMs)
	s := state.mcpServer
	// Each physical write also enters direct production admission. A duplicate
	// native event may advance its generation, but cannot make it fresh.
	ctx := daemonFreshnessSession(t, state, root)
	// Qualify a healthy published corpus with a real physical mutation and
	// its real watcher completion. Cold registration without any accepted
	// source witness is a different state and is not credited by this test.
	baselineCtx, baselineCancel := context.WithTimeout(ctx, 5*time.Second)
	defer baselineCancel()
	edit := daemonFreshnessCall(t, s, baselineCtx, 10, "write_file", map[string]any{
		"path": filepath.Join(root, "target.go"), "content": "package p\nfunc Target() int { return 2 }\n", "format": "json",
	})
	receipt, ok := edit["mutation_receipt"].(string)
	require.True(t, ok)
	require.NotEmpty(t, receipt)
	status := daemonFreshnessCall(t, s, baselineCtx, 11, "mutation_status", map[string]any{
		"receipt": receipt, "format": "json", "require_fresh": true,
		"wait_deadline": time.Now().Add(4 * time.Second).Format(time.RFC3339Nano),
	})
	require.Equal(t, "fresh", status["graph_status"], "baseline must actually publish: %v", status)
	relationsAt, ok := status["relations_at"].(string)
	require.True(t, ok)
	require.NotEmpty(t, relationsAt)
	// This is a non-atomic observation of the fixture-owned watcher, not a
	// freshness witness, request selector, or repair. It never reads disk/SQL.
	observeBaselineWatcher := func(phase string) {
		started := time.Now()
		physicalTarget := filepath.Clean(filepath.Join(root, "target.go"))
		tier := w.TierState(physicalTarget)
		canonical := w.TierPaths()
		canonicalTarget, present := canonical[physicalTarget]
		authority := w.TierAuthority("repo")
		row := map[string]any{
			"phase": phase, "observation_started_at": started, "observation_finished_at": time.Now(),
			"atomic": false, "physical_target": physicalTarget, "graph_target": "repo/target.go",
			"watcher_tier": tier, "canonical_target_present": present, "canonical_target_tier": canonicalTarget,
			"canonical_path_count": len(canonical), "watcher_authority": authority,
			"startup_relations_pending": state.multiIndexer != nil && state.multiIndexer.RelationsPending(),
			"baseline_receipt":          receipt, "baseline_reported_generation": edit["reindex_generation"],
			"baseline_reported_applied_generation": edit["applied_generation"],
			"baseline_reported_content_sha256":     edit["content_sha256"], "baseline_reported_readback_exact": edit["readback_exact"],
			"baseline_status_relations_at":      relationsAt,
			"desired_generation":                "UNOBSERVED: internal pendingGeneration is not exposed by this fixture API; TierState.Generation is the latest observed outstanding/failed ticket",
			"independent_physical_hash":         "UNOBSERVED: diagnostic performs no additional file read",
			"request_deferred_projection_files": "UNOBSERVED: selected request context is inside the original middleware",
			"selected_view_reads_own_checkout":  "UNOBSERVED: no wrapper or extra lease installed",
		}
		raw, marshalErr := json.Marshal(row)
		require.NoError(t, marshalErr)
		t.Logf("ACTUAL_C5_BASELINE_WATCHER_SNAPSHOT %s", raw)
	}
	inputState, found, err := store.ContractInputStateContext(baselineCtx, "repo", "")
	inputErr := err
	t.Logf("ACTUAL_DEFAULT_INPUT_STATE found=%t error=%v state=%+v", found, inputErr, inputState)
	type acceptedFile struct {
		path  string
		row   *graph.ContractBoundaryReceipt
		known bool
		err   error
	}
	var acceptedFiles []acceptedFile
	for _, file := range []string{"repo/target.go", "repo/caller.go", "repo/other_a.go", "repo/other_b.go"} {
		row, known, readErr := store.ContractBoundaryReceiptContext(baselineCtx, "repo", "", file)
		acceptedFiles = append(acceptedFiles, acceptedFile{file, row, known, readErr})
		raw, encodeErr := json.Marshal(row)
		t.Logf("ACTUAL_DEFAULT_FILE_RECEIPT path=%s known=%t error=%v encode_error=%v row=%s", file, known, readErr, encodeErr, raw)
	}
	daemonFreshnessBaselineIdentity(t, ctx, state, root, registered.CheckoutID, registered.Incarnation, registered.GraphID)
	require.NoError(t, inputErr)
	require.True(t, found)
	// This is contract-namespace reconstruction debt, not an ordinary core
	// freshness gate. The real accepted per-file source guards below remain.
	for _, observed := range acceptedFiles {
		file, row := observed.path, observed.row
		require.NoError(t, observed.err)
		require.True(t, observed.known)
		require.NotNil(t, row)
		require.True(t, row.Accepted, "no receipt is manually fabricated or accepted")
		require.False(t, row.Deleted)
		require.Equal(t, "repo", row.RepoPrefix)
		require.Empty(t, row.CheckoutID)
		require.Equal(t, file, row.FilePath)
		require.NotEmpty(t, row.Version)
		require.NotEmpty(t, row.Fingerprint)
		require.NotEmpty(t, row.SourceFingerprint)
		expectedSource := map[string]string{
			"repo/target.go":  "package p\nfunc Target() int { return 2 }\n",
			"repo/caller.go":  "package p\nfunc Caller() int { return Target() }\n",
			"repo/other_a.go": "package p\nfunc UnrelatedA() int { return 0 }\n",
			"repo/other_b.go": "package p\nfunc UnrelatedB() int { return 0 }\n",
		}[file]
		require.NotEmpty(t, expectedSource)
		require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(expectedSource))), row.SourceFingerprint, "accepted source must match this fixture's real indexed bytes")
		t.Logf("ACTUAL_DEFAULT_CONTRACT_ACCEPTANCE file=%s version=%s source=%s fingerprint=%s accepted=%t", file, row.Version, row.SourceFingerprint, row.Fingerprint, row.Accepted)
	}
	// The separate unknown-bytes safeguard needs a real prior publication,
	// not another positive caller read that may itself admit catch-up. Its actual
	// write/status winner and all four accepted source receipts above establish
	// that prior publication; the ordinary full C5 still takes its original id12.
	if pendingCase == "unknown_pending_bytes" {
		require.Equal(t, true, edit["reindexed"])
		require.Equal(t, true, status["found"])
		require.Equal(t, true, status["graph_status_terminal"])
		require.Equal(t, receipt, status["receipt"])
		applied, known := status["applied_generation"].(float64)
		require.True(t, known)
		require.Positive(t, applied)
		require.Equal(t, edit["reindex_generation"], applied)
		require.Equal(t, edit["applied_generation"], applied)
		t.Logf("ACTUAL_UNKNOWN_NEGATIVE_PRIOR_PUBLICATION receipt=%s reindexed=true terminal=true winning_generation=%.0f relations_at=%s accepted_source_receipts=4 positive_id12_NOT_SENT", receipt, applied, relationsAt)
		gate.arm()
		daemonFreshnessPendingPreparationCase(t, ctx, s, root, w, gate, baselineCtx, pendingCase)
		return
	}
	baseline, baselineErr := daemonBaselineReadiness(baselineCtx, func(readCtx context.Context) map[string]any {
		observeBaselineWatcher("before_original_id12")
		// Also runs when the existing helper fails via FailNow, before any
		// watcher/store teardown. The helper retains the original RPC result.
		defer observeBaselineWatcher("after_original_id12_before_cleanup")
		return daemonFreshnessCall(t, s, readCtx, 12, "get_callers", map[string]any{
			"id": "repo/target.go::Target", "format": "json", "require_fresh": true, "freshness_scope": "relations",
		})
	}, func(attempt int, verdict string) {
		t.Logf("ACTUAL_BASELINE_READINESS attempt=%d reply=%s first_partial_never_PASS=true shared_original_5s_context=true pressure_not_started=true", attempt, verdict)
	})
	require.NoError(t, baselineErr, "baseline setup must become actually fresh within its original shared caller context")
	require.Equal(t, "fresh", daemonFreshnessTier(t, baseline)["graph_freshness"])
	daemonFreshnessHasCaller(t, baseline, "Caller")
	t.Logf("ACTUAL_HEALTHY_BASELINE receipt=%s relations_at=%s registered_callers_fresh=true untouched_cold_owner_not_claimed=true", receipt, relationsAt)
	if baselineOnly {
		return
	}

	gate.arm()
	if pendingCase != "" {
		daemonFreshnessPendingPreparationCase(t, ctx, s, root, w, gate, baselineCtx, pendingCase)
		return
	}
	t.Logf("ACTUAL_UNRELATED_PRESSURE watch_debounce_ms=%d gate_armed_after_registered_fresh_baseline=true", config.Default().Watch.DebounceMs)
	type write struct {
		offer, physical, admitted time.Time
		path                      string
		generation                uint64
		err                       error
	}
	writes := make(chan write, 4)
	finished := make(chan struct{})
	var publishing atomic.Bool
	publishing.Store(true)
	publishCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); gate.releaseAndJoin(); <-finished })
	go func() {
		defer close(finished)
		defer close(writes)
		defer publishing.Store(false)
		origin := time.Now()
		for n := 0; n < 4; n++ {
			// Fixed offers cannot disappear as ticker values can. The reader
			// never advances this schedule, and records late admission honestly.
			offer := origin.Add(time.Duration(n+1) * 500 * time.Millisecond)
			timer := time.NewTimer(time.Until(offer))
			select {
			case <-publishCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			name, symbol := "other_a.go", "UnrelatedA"
			if n%2 != 0 {
				name, symbol = "other_b.go", "UnrelatedB"
			}
			path := filepath.Join(root, name)
			err := os.WriteFile(path, []byte(fmt.Sprintf("package p\nfunc %s() int { return %d }\n", symbol, n+1)), 0600)
			physical := time.Now()
			var generation uint64
			if err == nil {
				var ticket *indexer.MutationTicket
				ticket, err = w.EnqueueFileMutation(publishCtx, path)
				if err == nil && ticket != nil {
					generation = ticket.Generation
				}
			}
			writes <- write{offer, physical, time.Now(), path, generation, err}
			if err != nil {
				return
			}
		}
	}()
	var events []write
	type observation struct {
		started, finished time.Time
		publisherActive   bool
	}
	var queries []observation
	var heldPath string
	for event := range writes {
		require.NoError(t, event.err)
		require.False(t, event.physical.Before(event.offer))
		require.True(t, event.admitted.Before(event.offer.Add(500*time.Millisecond)), "fixture missed an offered 2 Hz slot: %+v", event)
		require.NotZero(t, event.generation, "real watcher admission required")
		tier := w.TierState(event.path)
		require.NotZero(t, tier.Generation)
		require.True(t, tier.RelationsAt.IsZero(), "unrelated generation must still be pending")
		if heldPath == "" {
			select {
			case heldPath = <-gate.entered:
				name := filepath.Base(heldPath)
				require.True(t, name == "other_a.go" || name == "other_b.go", "only actual unrelated extraction may be held: %q", heldPath)
				t.Logf("ACTUAL_UNRELATED_EXTRACTION_ENTRY path=%q source=real_go_extractor", heldPath)
			case <-baselineCtx.Done():
				observeBaselineWatcher("unrelated_entry_missing_at_original_baseline_bound")
				t.Fatalf("real unrelated extractor did not enter before original baseline bound: %v", baselineCtx.Err())
			case <-publishCtx.Done():
				observeBaselineWatcher("unrelated_entry_missing_at_original_test_cancel")
				t.Fatalf("real unrelated extractor did not enter before original test cancellation: %v", publishCtx.Err())
			}
		}
		observed := observation{started: time.Now(), publisherActive: publishing.Load()}
		body := func() map[string]any {
			observeBaselineWatcher(fmt.Sprintf("before_original_stream_id%d", 20+len(events)))
			defer observeBaselineWatcher(fmt.Sprintf("after_original_stream_id%d_before_cleanup", 20+len(events)))
			return daemonFreshnessCall(t, s, ctx, 20+len(events), "get_callers", map[string]any{
				"id": "repo/target.go::Target", "format": "json", "require_fresh": true,
				"freshness_scope": "relations", "wait_deadline": time.Now().Add(2 * time.Second).Format(time.RFC3339Nano),
			})
		}()
		observed.finished = time.Now()
		queries = append(queries, observed)
		require.Equal(t, "fresh", daemonFreshnessTier(t, body)["graph_freshness"])
		daemonFreshnessHasCaller(t, body, "Caller")
		events = append(events, event)
	}
	gate.releaseAndJoin()
	require.NotEmpty(t, heldPath, "real unrelated extraction entry required")
	require.Positive(t, gate.entries.Load())
	require.Equal(t, gate.entries.Load(), gate.completed.Load(), "every held extraction must physically return before cleanup")
	t.Logf("ACTUAL_UNRELATED_EXTRACTION_JOIN entered=%d completed=%d", gate.entries.Load(), gate.completed.Load())
	require.Len(t, events, 4)
	for n := 1; n < len(events); n++ {
		require.Equal(t, 500*time.Millisecond, events[n].offer.Sub(events[n-1].offer))
	}
	overlapped := 0
	for _, query := range queries {
		if query.publisherActive && query.started.Before(events[3].physical) && query.finished.After(events[0].physical) {
			overlapped++
		}
	}
	require.Positive(t, overlapped, "queries did not overlap the actual independent write stream: events=%+v queries=%+v", events, queries)
	t.Logf("ACTUAL_2HZ_UNRELATED_WRITES count=%d offered_interval=500ms actual_writes=%+v query_intervals=%+v overlapping_queries=%d registered_callers_fresh=4", len(events), events, queries, overlapped)
}

type daemonFreshnessTransportKey struct{}
type daemonFreshnessTransport struct {
	dispatcher *mcpDispatcher
	session    *daemon.Session
	root       string
}

func daemonFreshnessSession(t *testing.T, state *daemonState, root string) context.Context {
	t.Helper()
	// The ordinary CLI sends caller CWD in Handshake, not a tool argument.
	// Register uses the exact production field-to-session initialization path.
	handshake := daemon.Handshake{Mode: daemon.ModeMCP, ClientName: "cli", CWD: filepath.Clean(root), Tools: cliLegacyToolSurface, ToolsMode: cliLegacyToolMode}
	registry := daemon.NewSessionRegistry()
	connection, peer := net.Pipe()
	session := registry.Register(connection, handshake)
	dispatcher := newMCPDispatcher(state.mcpServer, state.multiIndexer, zap.NewNop())
	dispatcher.SessionStarted(session, func([]byte) error { return nil })
	t.Cleanup(func() {
		dispatcher.SessionEnded(session)
		registry.Remove(connection)
		require.NoError(t, errors.Join(connection.Close(), peer.Close()))
	})
	client := &daemonFreshnessTransport{dispatcher: dispatcher, session: session, root: filepath.Clean(root)}
	ctx := context.WithValue(t.Context(), daemonFreshnessTransportKey{}, client)
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"freshness-default-runtime","version":"1"}}}`)
	readback := requestCWD(session, frame)
	require.Equal(t, client.root, session.EffectiveCWD())
	require.Equal(t, client.root, readback)
	raw, err := dispatcher.Dispatch(ctx, session, frame)
	require.NoError(t, err)
	t.Logf("ACTUAL_DEFAULT_INITIALIZE session_id=%q handshake_cwd=%q readback_cwd=%q roots_parameter=NOT_SENT source=production_ModeMCP_Handshake_SessionRegistry_Dispatch response=%s", session.ID, session.CWD, readback, raw)
	require.NotEmpty(t, raw)
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Nil(t, response.Error, string(raw))
	require.NotEmpty(t, response.Result)
	return ctx
}

// Read already-existing identities only. No view materialization, raw-source
// snapshot or extra BaseCorpus lease is acquired by this observation.
func daemonFreshnessBaselineIdentity(t *testing.T, ctx context.Context, state *daemonState, root, checkout, incarnation, graphID string) {
	t.Helper()
	client, connected := ctx.Value(daemonFreshnessTransportKey{}).(*daemonFreshnessTransport)
	require.True(t, connected)
	readback := requestCWD(client.session, nil)
	require.Equal(t, filepath.Clean(root), readback)
	require.Equal(t, filepath.Clean(root), client.session.EffectiveCWD())
	processCWD, cwdErr := os.Getwd()
	canonicalRoot, rootErr := filepath.EvalSymlinks(root)
	t.Logf("ACTUAL_DEFAULT_BASELINE_IDENTITY session_id=%q handshake_cwd=%q effective_cwd=%q requestCWD_readback=%q outer_context_cwd=%q process_cwd=%q cwd_error=%v fixture_root=%q canonical_root=%q canonical_error=%v checkout=%q incarnation=%q graph_id=%q original_request_readsOwnCheckout=UNOBSERVED original_request_selected_reader=UNOBSERVED original_request_pin_error=UNOBSERVED_no_public_accessor_no_extra_lease", client.session.ID, client.session.CWD, client.session.EffectiveCWD(), readback, gortexmcp.SessionCWDFromContext(ctx), processCWD, cwdErr, root, canonicalRoot, rootErr, checkout, incarnation, graphID)
	materializer := state.mcpServer.Materializer()
	authority := state.shared.OutputGenerationAuthority
	t.Logf("ACTUAL_DEFAULT_STANDALONE_INDEXER indexer=%p root=%q prefix=%q", state.indexer, state.indexer.RootPath(), state.indexer.RepoPrefix())
	t.Logf("ACTUAL_DEFAULT_AUTHORITY base_handle=%p materializer_store=%p same_store=%t view_generation=%d payload_revision=%#v authority=%p mi_authority=%p standalone_authority=%p shared_lifecycle_leases=%t shared_materializer_leases=%t stats=%+v", state.graph, materializer.Store, state.graph == materializer.Store, materializer.Store.ViewGeneration(), materializer.Store.PayloadInputRevision(), authority, state.multiIndexer.ResolvedOutputGenerationAuthority(), state.indexer.ResolvedOutputGenerationAuthority(), authority.ViewLeases() == state.lifecycle.ViewLeases(), authority.ViewLeases() == materializer.Leases, authority.Stats())
	for _, prefix := range state.multiIndexer.RepoPrefixes() {
		owner := state.multiIndexer.GetIndexer(prefix)
		ownerRoot, known := state.multiIndexer.RepoRoot(prefix)
		registration, err := materializer.Leases.LookupRawRepositoryRegistration(prefix, ownerRoot)
		var rawOwner any
		if registration != nil {
			rawOwner = registration.Owner()
		}
		var indexerRoot, indexerPrefix string
		if owner != nil {
			indexerRoot = owner.RootPath()
			indexerPrefix = owner.RepoPrefix()
		}
		t.Logf("ACTUAL_DEFAULT_EXISTING_OWNER prefix=%q indexer=%p root=%q known=%t indexer_root=%q indexer_prefix=%q raw_registration_error=%v raw_owner=%+v", prefix, owner, ownerRoot, known, indexerRoot, indexerPrefix, err, rawOwner)
	}
}
func daemonFreshnessCall(t *testing.T, s *gortexmcp.Server, ctx context.Context, id int, tool string, args map[string]any) map[string]any {
	t.Helper()
	client, connected := ctx.Value(daemonFreshnessTransportKey{}).(*daemonFreshnessTransport)
	require.True(t, connected)
	require.Same(t, s, client.dispatcher.srv)
	frame, err := buildToolCallFrameWithDefault(id, tool, args, true)
	require.NoError(t, err)
	require.Equal(t, client.root, requestCWD(client.session, frame))
	require.Equal(t, client.root, client.session.EffectiveCWD())
	raw, err := client.dispatcher.Dispatch(ctx, client.session, frame)
	require.NoError(t, err)
	if len(raw) > 64<<10 {
		t.Logf("ACTUAL_REGISTERED_RESPONSE tool=%s id=%d bytes=%d diagnostic_capped=true prefix=%s", tool, id, len(raw), raw[:64<<10])
	} else {
		t.Logf("ACTUAL_REGISTERED_RESPONSE tool=%s id=%d body=%s", tool, id, raw)
	}
	var env struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	require.Nil(t, env.Error, string(raw))
	require.False(t, env.Result.IsError, string(raw))
	require.NotEmpty(t, env.Result.Content)
	// The first text block is the tool's JSON payload. Later blocks may be
	// session momentum notes; the full original envelope above retains them.
	require.Equal(t, "text", env.Result.Content[0].Type)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(env.Result.Content[0].Text), &body))
	return body
}
func daemonFreshnessTier(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	fields, ok := body["freshness"].(map[string]any)
	require.True(t, ok, "missing actual tier evidence: %v", body)
	return fields
}

func daemonFreshnessHasCaller(t *testing.T, body map[string]any, caller string) {
	t.Helper()
	edges, ok := body["edges"].([]any)
	require.True(t, ok, "missing actual callers: %v", body)
	found := false
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if ok && edge["from"] == "repo/caller.go::"+caller && edge["to"] == "repo/target.go::Target" && edge["kind"] == "calls" {
			found = true
		}
	}
	require.True(t, found, "actual caller %s missing: %v", caller, body)
}
