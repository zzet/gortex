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

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The bounded incremental affected-by pass leaves dependants holding edges and
// persisted reference facts derived against the pre-edit shape. The fact that
// it did rides the graph mutation receipt (graph.MutationReceipt
// FanoutTruncations), the watcher lowers it onto indexer.MutationResult, and
// these tests pin the rest of the chain: the served store can carry the fact,
// and the mutation the edit tools publish — both on the edit response and on
// mutation_status — names the hole.
//
// The fixtures are deliberately NOT newTestServer's in-memory graph.New() on
// the load-bearing arm. That is the one type that has always implemented the
// sink, so an assertion made against it stays green no matter what the shipped
// backend does — which is exactly how the production gap this file covers
// stayed invisible. *store_sqlite.Store is what
// internal/serverstack/backend.go openSqliteBackend hands the daemon, so it is
// what has to carry the fact, and it is what produces the receipt every
// assertion below is derived from.

func newSqliteBackedServer(t *testing.T) (*Server, graph.Store) {
	t.Helper()
	st, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fanout.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	s := newTestServer(t)
	s.graph = st
	return s, st
}

// The graph backend a Server actually serves from must be able to carry a
// bounded-fan-out completeness fact, and the receipt it hands back must name
// the hole.
//
// The assertion is made against s.graph — the field every handler reads —
// rather than against a store the test built and kept to itself, so a server
// wired to a backend without the sink fails here.
//
// Revert-red: drop *store_sqlite.Store.RecordMutationFanoutTruncation (or the
// FanoutTruncations drain from its accumulator's receipt()) and the sqlite arm
// fails at the carrier assertion, exactly as the shipped daemon behaved before
// this item: a truncated affected-by pass reading as a complete one.
func TestServedGraphBackendsCarryDerivedFanoutCompleteness(t *testing.T) {
	backends := []struct {
		name  string
		build func(t *testing.T) *Server
	}{
		// The backend the daemon serves from. Load-bearing arm.
		{"sqlite", func(t *testing.T) *Server {
			s, _ := newSqliteBackedServer(t)
			return s
		}},
		// The in-memory store the test server and the standalone indexer use.
		{"memory", newTestServer},
	}

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			g := backend.build(t).graph
			store, ok := g.(graph.MutationReceiptStore)
			require.True(t, ok, "%T does not bear mutation receipts", g)
			require.True(t, graph.ReceiptFanoutCarrier(g),
				"the store this server serves from (%T) cannot carry a bounded fan-out "+
					"completeness fact; a truncated affected-by pass would read as a complete one", g)

			token := store.BeginMutationReceipt()
			note := graph.NoteMutationFanoutTruncation(g, graph.ReceiptFanoutTruncation{
				Pass:         "affected_by",
				Cap:          4,
				Considered:   6,
				Dropped:      2,
				DroppedFiles: []string{"pkg/two.go", "pkg/one.go"},
			})
			require.Equal(t, graph.MutationFanoutNoteRecorded, note, "the fact reached no carrier on %T", g)
			receipt := store.EndMutationReceipt(token)

			require.False(t, receipt.DerivedFanoutComplete(),
				"a truncated window reported a complete fan-out: %+v", receipt.FanoutTruncations)
			fact, ok := receipt.FanoutTruncationFor("affected_by")
			require.True(t, ok, "the receipt does not name the affected-by pass: %+v", receipt.FanoutTruncations)
			assert.Equal(t, 2, fact.Dropped)
			assert.Equal(t, 4, fact.Cap)
			assert.Equal(t, 6, fact.Considered)
			assert.Equal(t, 2, receipt.DroppedFanoutFiles())
			assert.Equal(t, []string{"pkg/one.go", "pkg/two.go"}, fact.DroppedFiles,
				"the named files must be sorted so two runs of one batch render identically")

			// The additive axis must not void the delta description: an
			// incomplete receipt forces the conservative whole-frontier
			// fallback the bound exists to avoid.
			assert.True(t, receipt.Complete)
			assert.Empty(t, receipt.IncompleteReason)

			// A window with no cut says so by carrying nothing.
			clean := store.EndMutationReceipt(store.BeginMutationReceipt())
			assert.True(t, clean.DerivedFanoutComplete())
			assert.Empty(t, clean.FanoutTruncations)
		})
	}
}

// The fact has to survive the wire, not just the process: every tool response
// this server returns is JSON, and a field that renders as nothing is a fact a
// client cannot read. The key names are the wire contract — keep them stable.
func TestDerivedFanoutCompletenessSurvivesTheWireEncoding(t *testing.T) {
	receipt := graph.MutationReceipt{
		Complete: true,
		FanoutTruncations: []graph.ReceiptFanoutTruncation{{
			Pass: "affected_by", Cap: 4, Considered: 6,
			Dropped: 2, DroppedFiles: []string{"pkg/one.go", "pkg/two.go"},
		}},
	}
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	facts, ok := decoded["fanout_truncations"].([]any)
	require.True(t, ok, "the completeness fact did not render: %s", encoded)
	require.Len(t, facts, 1)
	entry := facts[0].(map[string]any)
	assert.Equal(t, "affected_by", entry["pass"])
	assert.EqualValues(t, 4, entry["cap"])
	assert.EqualValues(t, 6, entry["considered"])
	assert.EqualValues(t, 2, entry["dropped"])
	assert.Len(t, entry["dropped_files"], 2)

	// A complete window must render no key at all rather than an empty one: a
	// client reading "fanout_truncations": [] and one reading nothing must
	// reach the same conclusion, and omitempty is what guarantees it.
	clean, err := json.Marshal(graph.MutationReceipt{Complete: true})
	require.NoError(t, err)
	var cleanDecoded map[string]any
	require.NoError(t, json.Unmarshal(clean, &cleanDecoded))
	_, present := cleanDecoded["fanout_truncations"]
	assert.False(t, present, "a complete fan-out rendered a key: %s", clean)
}

// ---------------------------------------------------------------------------
// The fact on the served payloads: the edit response and mutation_status
// ---------------------------------------------------------------------------

// derivedFanoutFromServedStore produces the mutation fact the way production
// does: a real receipt window on the store the server serves from, one bounded
// pass recording its cut into it, and the watcher's lowering of that receipt.
//
// Deriving the fixture from the served store rather than hand-writing a
// DerivedFanoutCompleteness is what keeps these tests from going fixture-blind
// a second time: a backend that loses the sink yields a receipt with no
// truncation, the lowering reports a COMPLETE fan-out, and every assertion
// below fails.
func derivedFanoutFromServedStore(
	t *testing.T, s *Server, dropped []string,
) indexer.DerivedFanoutCompleteness {
	t.Helper()
	store, ok := s.graph.(graph.MutationReceiptStore)
	require.True(t, ok, "%T does not bear mutation receipts", s.graph)
	token := store.BeginMutationReceipt()
	if len(dropped) > 0 {
		require.Equal(t, graph.MutationFanoutNoteRecorded,
			graph.NoteMutationFanoutTruncation(s.graph, graph.ReceiptFanoutTruncation{
				Pass:         "affected_by",
				Cap:          2,
				Considered:   2 + len(dropped),
				Dropped:      len(dropped),
				DroppedFiles: dropped,
			}),
			"the cut reached no carrier on the served store %T", s.graph)
	}
	return indexer.DerivedFanoutFromReceipt(store.EndMutationReceipt(token))
}

// serveMutationWithFanout runs the real write_file handler against a server
// whose mutation scheduler reports the given fan-out verdict, and returns the
// edit response plus the mutation_status payload for the receipt it issued.
func serveMutationWithFanout(
	t *testing.T, s *Server, target string, fanout indexer.DerivedFanoutCompleteness,
) (map[string]any, map[string]any) {
	t.Helper()
	const generation = 7
	return serveMutationResult(t, s, target, indexer.MutationResult{
		RequestedGeneration: generation,
		AppliedGeneration:   generation,
		Reindexed:           true,
		DerivedFanout:       fanout,
	})
}

// serveMutationResult is the same drive over an arbitrary terminal mutation
// result, so the failed-mutation arm can be exercised with a verdict that the
// scheduler DID report and the server must refuse to publish.
func serveMutationResult(
	t *testing.T, s *Server, target string, result indexer.MutationResult,
) (map[string]any, map[string]any) {
	t.Helper()
	generation := result.AppliedGeneration
	if generation == 0 {
		generation = result.RequestedGeneration
	}
	done := make(chan indexer.MutationResult, 1)
	done <- result
	close(done)
	s.session = &sessionState{}
	s.watcher = mutationTestWatcher{done: done, generation: generation}

	res, err := s.handleWriteFile(context.Background(), writeFileRequest(map[string]any{
		"path": target, "content": "package p\n\nfunc F(x int, y int) int { return x + y }\n",
	}))
	require.NoError(t, err)
	edit := resultJSON(t, res)
	return edit, mutationStatusFor(t, s, edit)
}

// mutationStatusFor polls mutation_status for the commit receipt an edit
// response carried.
func mutationStatusFor(t *testing.T, s *Server, edit map[string]any) map[string]any {
	t.Helper()
	receipt, _ := edit["mutation_receipt"].(string)
	require.NotEmpty(t, receipt, "the edit response carried no commit receipt: %v", edit)

	req := mcp.CallToolRequest{}
	req.Params.Name = "mutation_status"
	req.Params.Arguments = map[string]any{"receipt": receipt}
	statusRes, statusErr := s.handleMutationStatus(context.Background(), req)
	require.NoError(t, statusErr)
	return resultJSON(t, statusRes)
}

// End to end on the production backend: a mutation whose bounded affected-by
// pass was cut must reach the caller as a cut — on the edit response the tool
// returns AND on the mutation_status payload a client polls after losing that
// response.
//
// This is the fact graph_status cannot carry. graph_status reads "fresh" here:
// the graph did read the new bytes. What it does not say, and what this payload
// now does, is that two named dependants still hold edges and reference facts
// derived from the pre-edit signature.
//
// Revert-red: drop DerivedFanout from mutationReindexOutcome / from
// (*mutationReceipt).outcome, drop the attachDerivedFanout call from
// attachMutationFreshness, or drop attachRecordDerivedFanout from
// mutationStatusPayload, and a knowingly incomplete mutation publishes exactly
// what it published before this item — nothing.
func TestServedMutationStatusNamesTheDerivedFanoutCut(t *testing.T) {
	s, _ := newSqliteBackedServer(t)
	fanout := derivedFanoutFromServedStore(t, s, []string{"pkg/caller_b.go", "pkg/caller_a.go"})
	require.True(t, fanout.Observed)
	require.False(t, fanout.Complete, "the served store lost the cut before the wiring was exercised")

	target := filepath.Join(t.TempDir(), "def.go")
	edit, status := serveMutationWithFanout(t, s, target, fanout)

	for name, payload := range map[string]map[string]any{"edit_response": edit, "mutation_status": status} {
		assert.Equal(t, false, payload[derivedFanoutKeyComplete],
			"%s did not report the bounded pass as incomplete: %v", name, payload)
		assert.EqualValues(t, 2, payload[derivedFanoutKeyDropped],
			"%s did not name the number of files left stale: %v", name, payload)
		assert.Contains(t, payload, derivedFanoutKeyPasses,
			"%s did not name which derived pass was bounded: %v", name, payload)
		assert.Contains(t, payload[derivedFanoutKeyNote], "PRE-edit shape",
			"%s carried no actionable note: %v", name, payload)
	}

	// The two facts are independent and must both be published: the bytes are
	// in the graph, and the derived work over them was cut.
	assert.Equal(t, mutationGraphFresh, edit["graph_status"])
	assert.Equal(t, mutationGraphFresh, status["graph_status"])
	assert.Equal(t, true, status["graph_status_terminal"])
	assert.Equal(t, mutationDiskCommitted, status["disk_status"])
}

// A mutation under the bound publishes a POSITIVE complete verdict and no hole.
// A payload that only ever spoke up on a cut would leave a caller unable to
// tell a finished fan-out from a path that never measured one.
func TestServedMutationStatusReportsACompleteDerivedFanout(t *testing.T) {
	s, _ := newSqliteBackedServer(t)
	fanout := derivedFanoutFromServedStore(t, s, nil)
	require.True(t, fanout.Observed)
	require.True(t, fanout.Complete)

	target := filepath.Join(t.TempDir(), "def.go")
	edit, status := serveMutationWithFanout(t, s, target, fanout)

	for name, payload := range map[string]map[string]any{"edit_response": edit, "mutation_status": status} {
		assert.Equal(t, true, payload[derivedFanoutKeyComplete],
			"%s did not report the complete fan-out: %v", name, payload)
		assert.NotContains(t, payload, derivedFanoutKeyDropped, "%s invented a hole: %v", name, payload)
		assert.NotContains(t, payload, derivedFanoutKeyNote, "%s warned about a complete pass: %v", name, payload)
	}
}

// A mutation path that never measured the fan-out must publish NO verdict.
// Rendering "complete" from an unobserved fact would certify derived work that
// was never even attempted — the checkout-refresh and storm-batch arms report
// exactly this zero value today.
func TestUnobservedDerivedFanoutPublishesNoVerdict(t *testing.T) {
	s, _ := newSqliteBackedServer(t)

	target := filepath.Join(t.TempDir(), "def.go")
	edit, status := serveMutationWithFanout(t, s, target, indexer.DerivedFanoutCompleteness{})

	for name, payload := range map[string]map[string]any{"edit_response": edit, "mutation_status": status} {
		assert.NotContains(t, payload, derivedFanoutKeyComplete,
			"%s certified a fan-out nobody measured: %v", name, payload)
		assert.NotContains(t, payload, derivedFanoutKeyDropped, "%s: %v", name, payload)
	}
	// The rest of the graph verdict is unaffected: an unmeasured fan-out is not
	// a failed mutation.
	assert.Equal(t, mutationGraphFresh, status["graph_status"])
}

// attachDerivedFanout is the single renderer both payloads go through. Pin its
// three outcomes directly so a regression in either call site is attributable.
func TestAttachDerivedFanoutRendersOnlyObservedVerdicts(t *testing.T) {
	unobserved := map[string]any{}
	attachDerivedFanout(unobserved, indexer.DerivedFanoutCompleteness{})
	assert.Empty(t, unobserved)

	complete := map[string]any{}
	attachDerivedFanout(complete, indexer.DerivedFanoutCompleteness{Observed: true, Complete: true})
	assert.Equal(t, map[string]any{derivedFanoutKeyComplete: true}, complete)

	cut := map[string]any{}
	attachDerivedFanout(cut, indexer.DerivedFanoutCompleteness{
		Observed: true, Dropped: 3, Passes: []string{"affected_by"},
	})
	assert.Equal(t, false, cut[derivedFanoutKeyComplete])
	assert.Equal(t, 3, cut[derivedFanoutKeyDropped])
	assert.Equal(t, []string{"affected_by"}, cut[derivedFanoutKeyPasses])
	assert.NotEmpty(t, cut[derivedFanoutKeyNote])
}

// derivedFanoutForMutation recovers the verdict by (path, generation) because
// the fast completion path deliberately blanks the receipt id. Pin that: a
// wrong generation or a wrong path must find nothing rather than another
// mutation's verdict.
func TestDerivedFanoutForMutationIsScopedToItsPathAndGeneration(t *testing.T) {
	s := newTestServer(t)
	path := filepath.Join(t.TempDir(), "def.go")
	receipt := &mutationReceipt{
		id: "mutation-fanout", path: path, generation: 9, done: make(chan struct{}),
		completed: true,
		result: indexer.MutationResult{
			RequestedGeneration: 9, AppliedGeneration: 9, Reindexed: true,
			DerivedFanout: indexer.DerivedFanoutCompleteness{
				Observed: true, Dropped: 2, Passes: []string{"affected_by"},
			},
		},
	}
	close(receipt.done)
	s.mutationReceipts.Store(receipt.id, receipt)

	found, ok := s.derivedFanoutForMutation(path, 9)
	require.True(t, ok)
	assert.Equal(t, 2, found.Dropped)

	_, ok = s.derivedFanoutForMutation(path, 8)
	assert.False(t, ok, "a different generation must not inherit this mutation's verdict")
	_, ok = s.derivedFanoutForMutation(filepath.Join(t.TempDir(), "other.go"), 9)
	assert.False(t, ok, "a different path must not inherit this mutation's verdict")
	_, ok = s.derivedFanoutForMutation(path, 0)
	assert.False(t, ok, "an unrecorded generation must find nothing")

	// A receipt that is still in flight has no verdict to publish yet.
	pending := &mutationReceipt{id: "mutation-pending", path: path, generation: 11, done: make(chan struct{})}
	s.mutationReceipts.Store(pending.id, pending)
	_, ok = s.derivedFanoutForMutation(path, 11)
	assert.False(t, ok)
}

// A FAILED mutation must publish NO fan-out verdict, even when the scheduler
// reported one.
//
// The bounded pass can genuinely have run and been cut before the patch failed;
// what cannot be true is that the derived work over this edit finished, because
// the edit did not land. Publishing "complete" beside graph_status "failed" is
// the invented certification the whole fact exists to prevent, and publishing
// the cut is barely better: it describes a hole in a mutation that never
// applied.
//
// Revert-red: drop the Err/Reindexed gate from (*mutationReceipt).outcome and a
// failed edit response publishes derived_fanout_* keys again.
func TestFailedMutationPublishesNoDerivedFanoutVerdict(t *testing.T) {
	s, _ := newSqliteBackedServer(t)
	fanout := derivedFanoutFromServedStore(t, s, []string{"pkg/caller_a.go", "pkg/caller_b.go"})
	require.True(t, fanout.Observed, "the fixture verdict was never observed; the test would be vacuous")
	require.False(t, fanout.Complete)

	target := filepath.Join(t.TempDir(), "def.go")
	edit, status := serveMutationResult(t, s, target, indexer.MutationResult{
		RequestedGeneration: 7,
		AppliedGeneration:   7,
		Reindexed:           false,
		Err:                 errors.New("mcp-test: the graph patch failed"),
		DerivedFanout:       fanout,
	})

	for name, payload := range map[string]map[string]any{"edit_response": edit, "mutation_status": status} {
		assert.NotContains(t, payload, derivedFanoutKeyComplete,
			"%s published a fan-out verdict for a failed mutation: %v", name, payload)
		assert.NotContains(t, payload, derivedFanoutKeyDropped, "%s: %v", name, payload)
		assert.NotContains(t, payload, derivedFanoutKeyPasses, "%s: %v", name, payload)
		assert.NotContains(t, payload, derivedFanoutKeyNote, "%s: %v", name, payload)
	}

	// The failure itself is still reported: this gate removes a verdict, not an
	// error.
	assert.Equal(t, mutationGraphFailed, edit["graph_status"])
	assert.Equal(t, mutationGraphFailed, status["graph_status"])

	// The same gate covers the lookup mutation_status uses, so a record whose
	// receipt is still in the ledger cannot recover the verdict by the back door.
	_, ok := s.derivedFanoutForMutation(target, 7)
	assert.False(t, ok, "a failed mutation's verdict was recoverable by (path, generation)")
}

// End to end with no stub anywhere on the graph half: a REAL indexer, a REAL
// watcher and the production sqlite backend behind a real write_file call.
//
// The other served tests install a scheduler stub with a canned MutationResult,
// which pins the join from indexer.MutationResult to the payload but takes the
// watcher's half on faith. This one takes nothing on faith: the corpus is three
// callers of one definition under a fan-out cap of two, write_file rewrites the
// definition's signature, and the cut that the bounded affected-by pass makes
// inside the watcher has to travel the whole chain — observation, ticket,
// receipt, renderer — to reach the two payloads a caller reads.
func TestServedEditThroughARealWatcherNamesTheDerivedFanoutCut(t *testing.T) {
	dir := t.TempDir()
	defPath := filepath.Join(dir, "def.go")
	require.NoError(t, os.WriteFile(defPath, []byte("package p\n\nfunc F(x int) int { return x }\n"), 0o644))
	for c := 0; c < 3; c++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, fmt.Sprintf("caller%d.go", c)),
			[]byte(fmt.Sprintf("package p\n\nfunc Caller%d() int { return F(%d) }\n", c, c)), 0o644))
	}

	st, err := store_sqlite.Open(filepath.Join(t.TempDir(), "e2e.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	// Three referencing files against a cap of two: the fan-out is cut by one.
	cfg.AffectedByReresolveMax = 2
	idx := indexer.New(st, reg, cfg, zap.NewNop())
	idx.SetRootPath(dir)
	_, err = idx.Index(dir)
	require.NoError(t, err)

	w, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)

	s := newTestServer(t)
	s.graph = st
	s.session = &sessionState{}
	s.watcher = w
	// The verdict only rides a COMPLETED ticket; a short wait would report the
	// mutation as pending and prove nothing either way.
	s.mutationReindexWait = 60 * time.Second

	res, err := s.handleWriteFile(context.Background(), writeFileRequest(map[string]any{
		"path": defPath, "content": "package p\n\nfunc F(x int, y int) int { return x + y }\n",
	}))
	require.NoError(t, err)
	edit := resultJSON(t, res)
	require.Equal(t, true, edit["reindexed"], "the real watcher never applied the edit: %v", edit)

	status := mutationStatusFor(t, s, edit)
	for name, payload := range map[string]map[string]any{"edit_response": edit, "mutation_status": status} {
		require.Contains(t, payload, derivedFanoutKeyComplete,
			"%s carried no fan-out verdict from the real watcher: %v", name, payload)
		assert.Equal(t, false, payload[derivedFanoutKeyComplete],
			"%s reported the cut pass as complete: %v", name, payload)
		assert.EqualValues(t, 1, payload[derivedFanoutKeyDropped],
			"%s did not name the size of the hole: %v", name, payload)
		assert.Equal(t, []any{"affected_by"}, payload[derivedFanoutKeyPasses],
			"%s did not name which derived pass was bounded: %v", name, payload)
	}
	assert.Equal(t, mutationGraphFresh, edit["graph_status"],
		"the bytes did reach the graph; the fan-out axis is additive to that")
}
