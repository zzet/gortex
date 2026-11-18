package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/telemetry"
)

// The base-corpus half of the same require_exact rule the route-drift
// half is pinned by in view_bytes_coherence_test.go.
//
// markBaseCorpusChange (view_request.go) is the precedent markWorktreeRouteMoved
// was written from, and it demotes the rider at the same place in the request's
// life: after the handler, before the rider is rendered. A require_exact caller
// therefore received exact:false with fallback_reason "base_changed" on a
// response it had asked never to receive — the same defect as the route half,
// on the other half of the stack.
//
// Parity is enforced in the strict direction on purpose: require_exact refuses
// ANY non-exact answer, and which half of the stack moved underneath is not a
// distinction the caller asked to be made for it.
func TestRequireExactRefusesAnAnswerWhoseBaseCorpusMoved(t *testing.T) {
	stack := newViewStack(t)
	// Registered the way the daemon registers it: exactly one dedicated owner
	// per tracked repository prefix. Without a registered owner the base pin
	// witnesses nothing and no change can be observed at all.
	if err := stack.leases.RegisterRepositoryOwner(viewTestOwner()); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}
	authority := indexer.NewOutputGenerationAuthority(stack.leases)
	mutateBaseCorpus := func(t *testing.T) {
		t.Helper()
		receipt, err := authority.Begin(context.Background(), indexer.OutputEntryWatcherDirScan,
			indexer.OutputMutationTarget{
				Kind:       indexer.OutputGenerationLegacy,
				OwnerKey:   "root:" + stack.repoRoot,
				RepoPrefix: "repo",
				RootPath:   stack.repoRoot,
			})
		if err != nil {
			t.Fatalf("admit a generation-zero mutation: %v", err)
		}
		if !receipt.Witnessed() {
			t.Fatal("the authority took no source witness for a registered repository owner")
		}
		if err := receipt.Complete(); err != nil {
			t.Fatalf("fulfil the mutation: %v", err)
		}
	}
	// One mutation establishes the witness every arm below compares against.
	mutateBaseCorpus(t)

	call := func(t *testing.T, requireExact, moveTheCorpus bool) *mcplib.CallToolResult {
		t.Helper()
		args := map[string]any{}
		if requireExact {
			args[requireExactArgName] = true
		}
		res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", args,
			func(hctx context.Context) (*mcplib.CallToolResult, error) {
				view := requestViewFromContext(hctx)
				if view == nil || !view.basePin.Witnessed() {
					t.Error("the routed request did not witness the base corpus source")
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				}
				if moveTheCorpus {
					mutateBaseCorpus(t)
				}
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		return res
	}

	t.Run("require_exact answers an undisturbed request", func(t *testing.T) {
		res := call(t, true, false)
		if res.IsError {
			t.Fatalf("require_exact refused a request nothing moved under: %s", viewResultText(t, res))
		}
		if rider := resultFreshness(t, res); rider["exact"] != true {
			t.Errorf("an undisturbed routed request reported %v, want exact", rider)
		}
	})

	t.Run("without require_exact the moved corpus still answers", func(t *testing.T) {
		res := call(t, false, true)
		if res.IsError {
			t.Fatalf("a moved base corpus must still answer without require_exact: %s", viewResultText(t, res))
		}
		rider := resultFreshness(t, res)
		if rider["exact"] != false {
			t.Errorf("rider exact = %v, want false after the base corpus moved", rider["exact"])
		}
		if rider["fallback_reason"] != baseChangedFallbackReason {
			t.Errorf("rider fallback_reason = %v, want %q", rider["fallback_reason"], baseChangedFallbackReason)
		}
	})

	t.Run("require_exact refuses the moved corpus", func(t *testing.T) {
		res := call(t, true, true)
		if !res.IsError {
			t.Fatalf("require_exact answered a request whose base corpus moved: %s", viewResultText(t, res))
		}
		text := viewResultText(t, res)
		for _, want := range []string{
			graphview.CodeViewBuilding,
			baseChangedFallbackReason,
			"require_exact",
			"no fallback was served",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the refusal does not name %q: %s", want, text)
			}
		}
	})
}

// The refusal is require_exact's alone and it refuses nothing else: a request
// that did not ask for exactness keeps every answer it had, and an exact rider
// is never refused whatever else the request collected.
func TestWithdrawnExactnessRefusalIsScopedToRequireExact(t *testing.T) {
	exact := &requestView{rider: graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto})}
	if got := (*Server)(nil).refuseWithdrawnExactness("get_symbol", true, exact); got != nil {
		t.Fatalf("an exact rider was refused: %v", got)
	}

	demoted := &requestView{rider: graphview.NewViewRider(graphview.Selector{Kind: graphview.SelectorAuto})}
	if err := demoted.rider.MarkFallback(demoted.rider.ActualView, routeMovedFallbackReason); err != nil {
		t.Fatalf("demote the rider: %v", err)
	}
	if got := (*Server)(nil).refuseWithdrawnExactness("get_symbol", false, demoted); got != nil {
		t.Fatalf("a caller that never asked for exactness was refused: %v", got)
	}
	refusal := (*Server)(nil).refuseWithdrawnExactness("get_symbol", true, demoted)
	if refusal == nil {
		t.Fatal("require_exact answered a demoted rider")
	}
	if !refusal.IsError {
		t.Fatal("the refusal is not an error result")
	}

	// Nil-safe on both halves, so the middleware needs no branch of its own.
	if got := (*Server)(nil).refuseWithdrawnExactness("get_symbol", true, nil); got != nil {
		t.Fatalf("a request with no view was refused: %v", got)
	}
	if got := (*Server)(nil).refuseWithdrawnExactness("get_symbol", true, &requestView{}); got != nil {
		t.Fatalf("a request with no rider was refused: %v", got)
	}
}

// TestALateExactnessRefusalNeverDiscardsAToolThatCouldHaveWritten is the other
// half of the post-handler gate's contract.
//
// Every other refusal in wrapToolHandler returns BEFORE the handler, so the
// sentence it hands back — "no fallback was served" — is the whole truth:
// nothing ran and a client that resubmits repeats nothing. This gate is the one
// place that sentence could be a lie, because the handler has already run. A
// client reads an error result as "nothing happened"; told that about a call
// that stored a memory, saved a note, wrote a config row or started an index,
// it retries and applies the write twice.
//
// So the late gate refuses only a call that left nothing behind. An effectful
// tool still ANSWERS, and its rider still carries exact:false and the
// fallback_reason — the caller learns exactly the same fact, on a response that
// admits the work happened.
func TestALateExactnessRefusalNeverDiscardsAToolThatCouldHaveWritten(t *testing.T) {
	stack := newViewStack(t)
	if err := stack.leases.RegisterRepositoryOwner(viewTestOwner()); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}
	authority := indexer.NewOutputGenerationAuthority(stack.leases)
	moveTheCorpus := func(t *testing.T) {
		t.Helper()
		receipt, err := authority.Begin(context.Background(), indexer.OutputEntryWatcherDirScan,
			indexer.OutputMutationTarget{
				Kind:       indexer.OutputGenerationLegacy,
				OwnerKey:   "root:" + stack.repoRoot,
				RepoPrefix: "repo",
				RootPath:   stack.repoRoot,
			})
		if err != nil {
			t.Fatalf("admit a generation-zero mutation: %v", err)
		}
		if err := receipt.Complete(); err != nil {
			t.Fatalf("fulfil the mutation: %v", err)
		}
	}
	// Establishes the witness every arm below compares against.
	moveTheCorpus(t)

	// call runs one require_exact request whose handler performs a stand-in
	// side effect and then moves the base corpus under itself — the exact
	// ordering the gate has to survive: the write lands, THEN the answer stops
	// being exact.
	call := func(t *testing.T, tool string) (*mcplib.CallToolResult, bool) {
		t.Helper()
		sideEffectLanded := false
		res, err := stack.callWithView(t, stack.worktreeRoot, tool,
			map[string]any{requireExactArgName: true},
			func(hctx context.Context) (*mcplib.CallToolResult, error) {
				view := requestViewFromContext(hctx)
				if view == nil || !view.basePin.Witnessed() {
					t.Error("the routed request did not witness the base corpus source")
					return mcplib.NewToolResultText(`{"ok":true}`), nil
				}
				sideEffectLanded = true
				moveTheCorpus(t)
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return res, sideEffectLanded
	}

	// A read-only tool is refused: nothing ran that a retry would repeat.
	t.Run("a read-only tool is still refused", func(t *testing.T) {
		res, landed := call(t, "get_symbol")
		if !landed {
			t.Fatal("the handler never ran, so this proves nothing about the gate")
		}
		if !res.IsError {
			t.Fatalf("require_exact answered a read whose base corpus moved: %s", viewResultText(t, res))
		}
	})

	// Every classified effectful tool answers. edit_file / write_file are
	// deliberately absent: the checkout-mutation admission refuses those before
	// the handler in this fixture, so an arm for them would assert the wrong
	// gate. They are covered by lateExactnessRefusalIsSafe's own unit below,
	// which is the predicate both would reach.
	for _, tool := range []string{
		"store_memory", // EffectFilesystemWrite  — a durable memory row
		"save_note",    // EffectFilesystemWrite  — a durable note
		"feedback",     // EffectFilesystemWrite
		"overlay_push", // EffectSessionWrite     — a doubled buffer push
		"track_repository",
		"analyze",         // unclassified on purpose; contains durable enrichers
		"change_contract", // unclassified on purpose; ack=true writes a memory
	} {
		t.Run(tool+" answers instead of being discarded", func(t *testing.T) {
			res, landed := call(t, tool)
			if !landed {
				t.Fatal("the handler never ran, so this proves nothing about the gate")
			}
			if res.IsError {
				t.Fatalf("a call that had already applied its effect was told it failed: %s",
					viewResultText(t, res))
			}
			// Not silently exact, either: the caller is told the same thing,
			// on a response that admits the work happened.
			rider := resultFreshness(t, res)
			if rider["exact"] != false {
				t.Errorf("rider exact = %v, want false after the base corpus moved", rider["exact"])
			}
			if rider["fallback_reason"] != baseChangedFallbackReason {
				t.Errorf("rider fallback_reason = %v, want %q",
					rider["fallback_reason"], baseChangedFallbackReason)
			}
		})
	}
}

// The predicate itself, enumerated against the canonical registry rather than
// against a list this file maintains: a tool added to daemon.ToolEffects is
// excluded from the late gate the moment it is classified, with no edit here.
func TestTheLateExactnessGateIsReadOnlyByConstruction(t *testing.T) {
	for _, tool := range daemon.SortedEffectfulTools() {
		if lateExactnessRefusalIsSafe(tool) {
			t.Errorf("%s is classified effectful and would still be discarded after its handler ran", tool)
		}
	}
	// The two conditional writers daemon.ToolEffects leaves unclassified on
	// purpose. If either is ever classified, this fails and the hard-coded
	// carve-out in lateExactnessRefusalIsSafe should be deleted.
	for _, tool := range []string{"analyze", "change_contract"} {
		if daemon.IsEffectful(tool) {
			t.Errorf("%s is now classified in daemon.ToolEffects; drop its hard-coded carve-out", tool)
		}
		if lateExactnessRefusalIsSafe(tool) {
			t.Errorf("%s writes for some argument shapes and must not be discarded after its handler ran", tool)
		}
	}
	// The read side the gate exists for stays refusable.
	for _, tool := range []string{
		"read_file", "get_symbol", "search_symbols", "find_usages",
		"get_editing_context", "search_text", "index_health",
	} {
		if !lateExactnessRefusalIsSafe(tool) {
			t.Errorf("%s reads and must still be refusable under require_exact", tool)
		}
	}
}

// TestALateExactnessRefusalIsStillCountedAndLogged keeps the ledgers honest
// about a call that ran.
//
// The post-handler gate returns early, which skips the whole middleware tail.
// Two of the things in that tail count CALLS rather than answers — the
// consent-gated usage counter, which was unconditional even for a handler that
// errored, and the retrieval query log — and before this a late-refused call
// simply vanished from both: the daemon's own usage numbers would under-report
// exactly the tool whose exactness was withdrawn most often, and the query log
// would show the refusal as if the client had never called.
//
// The decorators are deliberately still skipped. There is no answer to decorate
// with a rider, nothing to book against the retrieval savings ledger, and
// nothing worth keeping in the response ring — the same treatment every
// pre-handler refusal in this middleware gets.
func TestALateExactnessRefusalIsStillCountedAndLogged(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "query-log.jsonl")
	t.Setenv("GORTEX_QUERY_LOG", logPath)
	t.Setenv("GORTEX_QUERY_LOG_DISABLE", "")

	stack := newViewStack(t)
	if stack.srv.queryLog == nil || !stack.srv.queryLog.enabled {
		t.Fatal("the fixture's query logger is disabled, so this proves nothing")
	}
	store := telemetry.NewStore(t.TempDir())
	stack.srv.SetTelemetryRecorder(telemetry.NewRecorder(telemetry.Consent{Enabled: true}, store))

	if err := stack.leases.RegisterRepositoryOwner(viewTestOwner()); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}
	authority := indexer.NewOutputGenerationAuthority(stack.leases)
	moveTheCorpus := func(t *testing.T) {
		t.Helper()
		receipt, err := authority.Begin(context.Background(), indexer.OutputEntryWatcherDirScan,
			indexer.OutputMutationTarget{
				Kind:       indexer.OutputGenerationLegacy,
				OwnerKey:   "root:" + stack.repoRoot,
				RepoPrefix: "repo",
				RootPath:   stack.repoRoot,
			})
		if err != nil {
			t.Fatalf("admit a generation-zero mutation: %v", err)
		}
		if err := receipt.Complete(); err != nil {
			t.Fatalf("fulfil the mutation: %v", err)
		}
	}
	moveTheCorpus(t)

	// search_symbols is read-only (so the late gate applies to it) and is in
	// retrievalToolSpecs (so the query log covers it) — one call exercises both
	// ledgers.
	res, err := stack.callWithView(t, stack.worktreeRoot, "search_symbols",
		map[string]any{requireExactArgName: true, "query": "Fresh"},
		func(hctx context.Context) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(hctx)
			if view == nil || !view.basePin.Witnessed() {
				t.Error("the routed request did not witness the base corpus source")
				return mcplib.NewToolResultText(`{"ok":true}`), nil
			}
			moveTheCorpus(t)
			return mcplib.NewToolResultText(`{"results":[]}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("require_exact answered a request whose base corpus moved: %s", viewResultText(t, res))
	}

	stack.srv.FlushTelemetry()
	days, err := store.Days()
	if err != nil || len(days) != 1 {
		t.Fatalf("a late-refused call was never counted: days=%v err=%v", days, err)
	}
	roll, err := store.Load(days[0])
	if err != nil {
		t.Fatalf("load telemetry: %v", err)
	}
	if got := roll.Counts["mcp_tool_call:search_symbols"]; got != 1 {
		t.Errorf("mcp_tool_call:search_symbols = %d, want 1: a refused call is still a call", got)
	}

	stack.srv.queryLog.Close()
	records, _, err := readQueryLogTail(logPath, 10)
	if err != nil {
		t.Fatalf("readQueryLogTail: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("query log holds %d records, want 1: the late refusal was dropped", len(records))
	}
	if records[0].Tool != "search_symbols" {
		t.Errorf("logged tool = %q, want search_symbols", records[0].Tool)
	}
	if records[0].OK {
		t.Error("the late refusal was logged as a successful retrieval")
	}
}
