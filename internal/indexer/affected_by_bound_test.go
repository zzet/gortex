package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The affected-by fan-out cap has to bound ONE PASS, not one changed file in
// it. Before this item the batch path applied the cap per changed path and
// then unioned the results, so a batch of N changed paths re-resolved (and
// re-persisted reference facts for) up to N*cap files and logged nothing —
// the shape behind "1,218 indexed instances for 391 changed paths".
//
// These tests drive the real entry point (IncrementalReindexPaths) over a
// corpus where every per-changed-file set is UNDER the cap while their union
// is over it, so they fail the moment the bound goes back to being per-file.

// affectedByBoundObservation is everything one pass told the outside world
// about its fan-out: the files it actually re-resolved and the truncation
// facts it emitted.
type affectedByBoundObservation struct {
	mu        sync.Mutex
	resolved  []string
	dropped   [][]string
	uncarried [][]string
	factCount int
}

func (o *affectedByBoundObservation) record(kind string, files []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch kind {
	case "affected_by":
		o.resolved = append(o.resolved, files...)
	case "affected_by_truncated":
		o.factCount++
		o.dropped = append(o.dropped, files)
	case "affected_by_truncated_uncarried":
		o.uncarried = append(o.uncarried, files)
	}
}

// uncarriedCuts is every cut the pass reported as reaching NO receipt carrier.
// On a backend that carries the fact this must stay empty: a non-empty value
// is the indexer itself saying the completeness fact went nowhere.
func (o *affectedByBoundObservation) uncarriedCuts() [][]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][]string(nil), o.uncarried...)
}

func (o *affectedByBoundObservation) resolvedSet() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := make(map[string]struct{}, len(o.resolved))
	out := make([]string, 0, len(o.resolved))
	for _, f := range o.resolved {
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// affectedByBoundFixture writes a corpus of `defs` definition files, each
// referenced by `callersPer` distinct caller files, indexes it with the given
// fan-out cap, and returns the indexer plus the absolute paths of the
// definition files (in corpus order).
//
// Every definition takes one parameter so the signature edit in the tests is a
// genuine contract change, which is what arms the affected-by delta at all.
func affectedByBoundFixture(
	t *testing.T, defs, callersPer, maxFiles int,
) (*Indexer, graph.Store, *observer.ObservedLogs, *affectedByBoundObservation, string, []string) {
	t.Helper()
	return affectedByBoundFixtureOn(t, graph.New(), defs, callersPer, maxFiles)
}

// affectedByBoundFixtureOn is the same fixture over an EXPLICIT store, so the
// bounded fan-out can be exercised on the backend the daemon actually runs
// (store_sqlite) and not only on the in-memory graph. The two backends
// implement the receipt — and now the fan-out completeness sink — separately,
// so a fixture that can only build one of them cannot see a gap in the other.
func affectedByBoundFixtureOn(
	t *testing.T, g graph.Store, defs, callersPer, maxFiles int,
) (*Indexer, graph.Store, *observer.ObservedLogs, *affectedByBoundObservation, string, []string) {
	t.Helper()
	return affectedByBoundFixtureFor(t, g, "", defs, callersPer, maxFiles)
}

// affectedByBoundFixtureFor is the same fixture under an explicit repository
// prefix. The prefix has to be stamped BEFORE the corpus is indexed — node IDs
// carry it — so the MultiIndexer arm, which keys everything by prefix, cannot
// reuse an unprefixed fixture.
func affectedByBoundFixtureFor(
	t *testing.T, g graph.Store, repoPrefix string, defs, callersPer, maxFiles int,
) (*Indexer, graph.Store, *observer.ObservedLogs, *affectedByBoundObservation, string, []string) {
	t.Helper()
	dir := t.TempDir()
	defPaths := make([]string, 0, defs)
	for d := 0; d < defs; d++ {
		name := fmt.Sprintf("F%d", d)
		defPath := filepath.Join(dir, fmt.Sprintf("def%d.go", d))
		writeFile(t, defPath, fmt.Sprintf("package p\n\nfunc %s(x int) int { return x }\n", name))
		defPaths = append(defPaths, defPath)
		for c := 0; c < callersPer; c++ {
			writeFile(t,
				filepath.Join(dir, fmt.Sprintf("caller%d_%d.go", d, c)),
				fmt.Sprintf("package p\n\nfunc Caller%d_%d() int { return %s(%d) }\n", d, c, name, c))
		}
	}

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.AffectedByReresolveMax = maxFiles
	core, observed := observer.New(zap.DebugLevel)
	idx := New(g, reg, cfg, zap.New(core))
	if repoPrefix != "" {
		idx.SetRepoPrefix(repoPrefix)
	}
	obs := &affectedByBoundObservation{}
	idx.incrementalCatchupHook = obs.record
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Fixture precondition: every caller really is bound to its definition,
	// otherwise the fan-out this test measures would be vacuously empty.
	for d := 0; d < defs; d++ {
		target := fnNodeID(t, g, idx.prefixPath(fmt.Sprintf("def%d.go", d)), fmt.Sprintf("F%d", d))
		for c := 0; c < callersPer; c++ {
			callerFile := idx.prefixPath(fmt.Sprintf("caller%d_%d.go", d, c))
			from := fnNodeID(t, g, callerFile, fmt.Sprintf("Caller%d_%d", d, c))
			require.Equal(t, target, callTargetFrom(t, g, from),
				"fixture precondition: %s must bind %s", callerFile, target)
		}
	}
	return idx, g, observed, obs, dir, defPaths
}

// bumpAffectedByDefs rewrites every definition file with a second parameter —
// a real signature change on every changed path at once.
func bumpAffectedByDefs(t *testing.T, defPaths []string) {
	t.Helper()
	for d, defPath := range defPaths {
		bumpMtime(t, defPath, fmt.Sprintf(
			"package p\n\nfunc F%d(x int, y int) int { return x + y }\n", d))
	}
}

func truncationEntries(observed *observer.ObservedLogs) []map[string]any {
	entries := observed.FilterMessage("affected-by: re-resolve set truncated").All()
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ContextMap())
	}
	return out
}

// Two changed definition files, three callers each, cap 4. Each per-changed-
// file set (3) is UNDER the cap; their union (6) is over it. The batch must
// truncate ONCE, at batch level, re-resolve exactly cap files, and say so.
//
// Revert-red: with the pre-change per-changed-path cap, both sets pass the cap
// untouched, all six files are re-resolved and no truncation is reported.
func TestAffectedByWholeBatchBoundTruncatesOnceAcrossChangedFiles(t *testing.T) {
	idx, _, observed, obs, dir, defPaths := affectedByBoundFixture(t, 2, 3, 4)

	bumpAffectedByDefs(t, defPaths)
	_, err := idx.IncrementalReindexPaths(dir, defPaths)
	require.NoError(t, err)

	resolved := obs.resolvedSet()
	assert.Len(t, resolved, 4,
		"the whole batch must re-resolve at most the cap, not cap per changed file (got %v)", resolved)

	facts := truncationEntries(observed)
	require.Len(t, facts, 1, "the batch must record the truncation exactly once")
	assert.EqualValues(t, 6, facts[0]["affected"], "the fact must name the full union it considered")
	assert.EqualValues(t, 4, facts[0]["cap"])
	assert.EqualValues(t, 2, facts[0]["dropped"])

	require.Len(t, obs.dropped, 1, "the dropped set must reach the pass observation")
	assert.Len(t, obs.dropped[0], 2)

	// The fact has to be actionable: the files it names are exactly the ones
	// the pass did not re-resolve.
	for _, droppedFile := range obs.dropped[0] {
		assert.NotContains(t, resolved, droppedFile,
			"a file reported dropped must not also have been re-resolved")
	}
	assert.Equal(t, 6, len(resolved)+len(obs.dropped[0]),
		"kept + dropped must account for the whole union")
}

// The same corpus shape under the bound: two changed definition files, two
// callers each, cap 8. Nothing is cut, so nothing may be reported — a bound
// that reports on a complete pass is as useless as one that stays silent on an
// incomplete one.
func TestAffectedByWholeBatchBoundRecordsNoTruncationUnderTheCap(t *testing.T) {
	idx, _, observed, obs, dir, defPaths := affectedByBoundFixture(t, 2, 2, 8)

	bumpAffectedByDefs(t, defPaths)
	_, err := idx.IncrementalReindexPaths(dir, defPaths)
	require.NoError(t, err)

	assert.Len(t, obs.resolvedSet(), 4, "every referencing file must be re-resolved")
	assert.Empty(t, truncationEntries(observed),
		"a complete fan-out must record no truncation")
	assert.Empty(t, obs.dropped)
	assert.Zero(t, obs.factCount)
}

// planAffectedByStages has TWO consumers and the batch bound has to hold at
// both. IncrementalReindexPaths takes the deferred one (mergeDeferredAffected),
// which the two tests above drive; this one pins the other —
// reresolveAffectedByStages, the arm commitStructuralIncrementalBatch takes
// when the caller did not defer the resolver catch-up.
func TestReresolveAffectedByStagesBoundsTheWholeBatch(t *testing.T) {
	g := graph.New()
	prior := []*graph.Node{{
		ID: "def.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "def.go",
		Meta: map[string]any{"signature": "func Foo()"},
	}}
	callers := make([]*graph.Node, 0, 6)
	edges := make([]*graph.Edge, 0, 6)
	for c := 0; c < 6; c++ {
		file := fmt.Sprintf("caller%d.go", c)
		id := file + "::Bar"
		callers = append(callers, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: "Bar", FilePath: file,
		})
		edges = append(edges, &graph.Edge{
			From: id, To: "def.go::Foo", Kind: graph.EdgeCalls, FilePath: file, Line: 3,
		})
	}
	g.AddBatch(append(append([]*graph.Node{}, prior...), callers...), edges)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.AffectedByReresolveMax = 4
	core, observed := observer.New(zap.DebugLevel)
	idx := New(g, reg, cfg, zap.New(core))
	obs := &affectedByBoundObservation{}
	idx.incrementalCatchupHook = obs.record

	stage := &incrementalBatchStage{graphPath: "def.go", priorNodes: prior}
	view := loadIncrementalPriorView(g, []*incrementalBatchStage{stage})
	stage.abSnap = snapshotAffectedByFromView(prior, view)
	require.NotNil(t, stage.abSnap)
	// A real contract change: same identity, different signature.
	stage.result = &parser.ExtractionResult{Nodes: []*graph.Node{{
		ID: "def.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "def.go",
		Meta: map[string]any{"signature": "func Foo(n int)"},
	}}}

	idx.reresolveAffectedByStages([]*incrementalBatchStage{stage})

	assert.Len(t, obs.resolvedSet(), 4,
		"the non-deferred batch arm must bound the union to the cap")
	facts := truncationEntries(observed)
	require.Len(t, facts, 1)
	assert.EqualValues(t, 6, facts[0]["affected"])
	assert.EqualValues(t, 4, facts[0]["cap"])
	assert.EqualValues(t, 2, facts[0]["dropped"])
}

// boundAffectedByFiles is the single bound primitive both the point path
// (reresolveAffectedBy) and the batch paths share. Its determinism rule —
// keep the lexicographically smallest cap entries — is what makes the
// incremental bound mergeDeferredAffected applies identical to a one-shot
// bound over the complete union.
func TestBoundAffectedByFilesKeepsTheSortedPrefixAndNamesTheRest(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go", "d.go"}

	kept, fact := boundAffectedByFiles(files, 2)
	assert.Equal(t, []string{"a.go", "b.go"}, kept)
	assert.True(t, fact.Truncated)
	assert.Equal(t, 2, fact.Cap)
	assert.Equal(t, 4, fact.Considered)
	assert.Equal(t, []string{"c.go", "d.go"}, fact.Dropped)

	kept, fact = boundAffectedByFiles(files, 4)
	assert.Equal(t, files, kept)
	assert.False(t, fact.Truncated)
	assert.Empty(t, fact.Dropped)
	assert.Equal(t, 4, fact.Considered)

	// A non-positive cap must mean "no bound", never "re-resolve nothing":
	// affectedByMaxFiles never produces one, but a direct caller might.
	kept, fact = boundAffectedByFiles(files, 0)
	assert.Equal(t, files, kept)
	assert.False(t, fact.Truncated)
}

// The deferred path commits its stages in chunks and merges each chunk's
// affected set into one batch-wide union. The bound has to survive that: a
// per-chunk bound over C chunks is a bound of C*cap.
func TestMergeDeferredAffectedBoundsTheWholeBatchNotEachChunk(t *testing.T) {
	batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
	var reported []affectedByTruncation
	applications := 0
	chunk := func(files ...string) affectedByBatchPlan {
		return affectedByBatchPlan{
			files:    files,
			maxFiles: 3,
			notify: func(f affectedByTruncation) {
				// bounded() notifies on EVERY application; only a truncated
				// fact is a cut. The complete notifications are what tell a
				// mutation's fan-out observer that the pass ran at all, which
				// is what separates "finished" from "never attempted".
				applications++
				if f.Truncated {
					reported = append(reported, f)
				}
			},
		}
	}

	first := batch.mergeDeferredAffected(chunk("b.go", "c.go"))
	assert.False(t, first.truncation.Truncated, "two files under a cap of three is complete")
	assert.Equal(t, []string{"b.go", "c.go"}, first.files)
	assert.Empty(t, reported)
	assert.Equal(t, 1, applications, "a complete application must still announce that the pass ran")

	second := batch.mergeDeferredAffected(chunk("a.go", "d.go", "e.go"))
	assert.True(t, second.truncation.Truncated,
		"the union across chunks (5) exceeds the cap and must be cut once")
	assert.Equal(t, 5, second.truncation.Considered)
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, second.files,
		"the bound keeps the sorted prefix of the whole union")
	assert.Equal(t, []string{"d.go", "e.go"}, second.truncation.Dropped)
	require.Len(t, reported, 1, "a merge that cuts must report the cut even when the caller drops the plan")
	assert.Equal(t, []string{"d.go", "e.go"}, reported[0].Dropped)
	assert.Equal(t, 2, applications)

	// What the catch-up executes — and therefore what it persists reference
	// facts for — is the bounded union, not the accumulated one.
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, batch.deferredAffectedPlan().files)

	// A pruned file must not re-enter on the next merge, or the bound
	// degrades back to a per-chunk bound.
	third := batch.mergeDeferredAffected(chunk("f.go"))
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, third.files)
	assert.Equal(t, []string{"f.go"}, third.truncation.Dropped)
	assert.Len(t, reported, 2)
	assert.Equal(t, 3, applications)
}

// Deletion stages carry no fresh extraction, so every restub frontier there is
// conservative and every surviving in-edge is parked. Nothing on that path
// re-states a carried edge — there is no AddBatch after the eviction — so a
// non-empty carry would silently destroy those edges. Pin the precondition the
// call site depends on.
func TestDeletionShapedStagesCarryNoInEdges(t *testing.T) {
	st := newRestubCountingStore()
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"signature": "func Foo()"})}
	st.AddBatch(append([]*graph.Node{restubCallerNode()}, prior...),
		[]*graph.Edge{restubCallEdge("def.go::Foo")})

	// evictDeletedFilesBatched builds exactly this stage shape: prior nodes,
	// no result.
	stages := []*incrementalBatchStage{{graphPath: restubDefPath, priorNodes: prior}}
	view := loadIncrementalPriorView(st, stages)

	carried := restubIncomingRefsFromView(st, stages, view)
	require.Empty(t, carried,
		"a deletion stage must park every in-edge; a carried edge has no re-state path here")
	assert.Equal(t, 1, st.restubWrites("Foo"),
		"the deletion path must still park the surviving in-edge")
}

// ---------------------------------------------------------------------------
// The completeness fact on the wire: the mutation receipt
// ---------------------------------------------------------------------------
//
// Bounding the fan-out is half the contract; the other half is that a caller
// can TELL. Before this the fact reached a Warn line and a pass-observation
// hook whose only setters are test files, so no caller, API or receipt
// consumer could distinguish a truncated fan-out from a complete one — the
// graph was knowingly left with stale dependants and nothing on the wire said
// so. The fact now rides the mutation receipt the incremental pipeline already
// returns for every batch.
//
// incrementalReindexPathsWithReceiptMode is the production receipt entrypoint:
// incremental_watcher_batch.go incrementalWatcherPaths and multi.go both reach
// the bounded pipeline exclusively through it, and it is the only place a
// MutationReceipt for an incremental batch is opened and closed.
//
// Both tests below run over BOTH receipt backends, and the sqlite arm is the
// load-bearing one: *store_sqlite.Store is the only graph.Store the daemon
// ever holds (internal/serverstack/backend.go openSqliteBackend ->
// store_sqlite.Open), so a fact that only *graph.Graph can carry reaches no
// shipped consumer at all. The two accumulators are separate implementations;
// exercising one proves nothing about the other.

// receiptBackends are the two graph.Store implementations that bear mutation
// receipts. "sqlite" is the production backend.
func receiptBackends() []struct {
	name  string
	build func(t *testing.T) graph.Store
} {
	return []struct {
		name  string
		build func(t *testing.T) graph.Store
	}{
		{"memory", func(t *testing.T) graph.Store { return graph.New() }},
		{"sqlite", newSqliteGraph},
	}
}

// A batch that truncates must yield a receipt whose fan-out completeness names
// the pass, the cap, the union it considered and every file it left stale.
//
// Revert-red: drop carryAffectedByTruncationOnReceipt from
// reportAffectedByTruncation (or the FanoutTruncations drain from either
// accumulator, or *Store.RecordMutationFanoutTruncation) and the receipt
// reports a complete fan-out over a graph that is missing two files' worth of
// re-resolution.
func TestAffectedByTruncationReachesTheMutationReceipt(t *testing.T) {
	for _, backend := range receiptBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.build(t)
			require.True(t, graph.ReceiptFanoutCarrier(store),
				"%T cannot carry a bounded fan-out completeness fact; a truncated "+
					"affected-by pass would read as a complete one on this backend", store)
			idx, _, observed, obs, dir, defPaths := affectedByBoundFixtureOn(t, store, 2, 3, 4)

			bumpAffectedByDefs(t, defPaths)
			_, receipt, _, err := idx.incrementalReindexPathsWithReceiptMode(dir, defPaths, incrementalPathMode{})
			require.NoError(t, err)
			require.NotNil(t, receipt, "the bounded pipeline must return a receipt for a receipt-bearing store")

			// The pass really was cut in this run: without this the assertions
			// below could pass vacuously on a backend that fanned out fully.
			require.Len(t, truncationEntries(observed), 1,
				"the fixture did not truncate on %T; the receipt assertions would be vacuous", store)
			require.Empty(t, obs.uncarriedCuts(),
				"the indexer announced the cut as reaching no receipt carrier on %T", store)

			require.False(t, receipt.DerivedFanoutComplete(),
				"a truncated fan-out reported a complete receipt: %+v", receipt.FanoutTruncations)
			fact, ok := receipt.FanoutTruncationFor(affectedByFanoutPass)
			require.True(t, ok, "the receipt does not name the affected-by pass: %+v", receipt.FanoutTruncations)
			assert.Equal(t, 4, fact.Cap)
			assert.Equal(t, 6, fact.Considered, "the fact must name the full union the bound fired on")
			assert.Equal(t, 2, fact.Dropped)
			assert.Equal(t, 2, receipt.DroppedFanoutFiles())

			// The names on the receipt are exactly the names the in-process pass
			// observation reported, and exactly the files that were NOT re-resolved.
			require.Len(t, obs.dropped, 1)
			assert.ElementsMatch(t, obs.dropped[0], fact.DroppedFiles,
				"the receipt must carry the same dropped set the pass computed")
			resolved := obs.resolvedSet()
			for _, droppedFile := range fact.DroppedFiles {
				assert.NotContains(t, resolved, droppedFile,
					"a file named on the receipt as dropped was re-resolved anyway")
			}

			// The fan-out axis is additive. Voiding Complete would make every
			// truncated batch take the conservative whole-frontier fallback this
			// path exists to avoid — and would misreport: the store described the
			// delta exactly, it was the derived pass OVER that delta that was cut.
			assert.True(t, receipt.Complete,
				"a bounded fan-out voided the delta receipt: %+v", *receipt)
			assert.NotContains(t, receipt.IncompleteReason, "fanout")
			assert.Len(t, truncationEntries(observed), 1,
				"carrying the fact must not cost the operator-facing log line")
		})
	}
}

// The same shape under the bound: nothing is cut, so the receipt must say the
// fan-out was complete and carry no fact at all. A receipt that reports a hole
// on a complete pass is as useless as one that stays silent on an incomplete
// one.
func TestAffectedByUnderTheBoundYieldsACompleteFanoutReceipt(t *testing.T) {
	for _, backend := range receiptBackends() {
		t.Run(backend.name, func(t *testing.T) {
			idx, _, observed, _, dir, defPaths := affectedByBoundFixtureOn(t, backend.build(t), 2, 2, 8)

			bumpAffectedByDefs(t, defPaths)
			_, receipt, _, err := idx.incrementalReindexPathsWithReceiptMode(dir, defPaths, incrementalPathMode{})
			require.NoError(t, err)
			require.NotNil(t, receipt)

			assert.True(t, receipt.DerivedFanoutComplete(),
				"an untruncated pass reported a hole: %+v", receipt.FanoutTruncations)
			assert.Empty(t, receipt.FanoutTruncations)
			assert.Zero(t, receipt.DroppedFanoutFiles())
			assert.Empty(t, truncationEntries(observed))
		})
	}
}

// receiptOnlyIndexerStore bears mutation receipts but cannot carry a fan-out
// completeness fact — the shape every backend has until it implements
// graph.MutationFanoutRecorder. Embedding the INTERFACE (not *graph.Graph) is
// what withholds the sink while keeping every Store method.
type receiptOnlyIndexerStore struct {
	graph.Store
}

func (s *receiptOnlyIndexerStore) BeginMutationReceipt() graph.MutationReceiptToken {
	return 1
}

func (s *receiptOnlyIndexerStore) EndMutationReceipt(graph.MutationReceiptToken) graph.MutationReceipt {
	return graph.MutationReceipt{Complete: true}
}

// A cut that reaches no carrier must be announced by name, never swallowed. A
// silent drop here would rebuild, one layer down, exactly the defect this item
// removes: a truncated fan-out that reads as a complete one.
func TestAffectedByTruncationAnnouncesAMissingReceiptCarrier(t *testing.T) {
	g := graph.New()
	core, observed := observer.New(zap.DebugLevel)
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.New(core))
	obs := &affectedByBoundObservation{}
	var uncarried [][]string
	idx.incrementalCatchupHook = func(kind string, files []string) {
		obs.record(kind, files)
		if kind == "affected_by_truncated_uncarried" {
			uncarried = append(uncarried, files)
		}
	}
	fact := affectedByTruncation{
		Truncated: true, Cap: 1, Considered: 3,
		Dropped: []string{"src/y.go", "src/z.go"},
	}

	// A carrier takes it silently.
	idx.reportAffectedByTruncation("scope", fact)
	require.Empty(t, uncarried, "a carrier store must not report a missing carrier")
	require.Len(t, observed.FilterMessage("affected-by: truncation has no mutation-receipt carrier").All(), 0)

	// A receipt-bearing store without the sink is named.
	idx.graph = &receiptOnlyIndexerStore{Store: g}
	idx.reportAffectedByTruncation("scope", fact)
	require.Len(t, uncarried, 1, "an uncarried cut must reach the pass observation")
	assert.ElementsMatch(t, fact.Dropped, uncarried[0])
	warns := observed.FilterMessage("affected-by: truncation has no mutation-receipt carrier").All()
	require.Len(t, warns, 1, "an uncarried cut must be logged by name")
	assert.Equal(t, zap.WarnLevel, warns[0].Level)
	assert.EqualValues(t, 2, warns[0].ContextMap()["dropped"])
}

// ---------------------------------------------------------------------------
// The completeness fact on the wire: the admitted mutation's result
// ---------------------------------------------------------------------------
//
// The receipt is an in-process object the indexer discards at its own function
// boundary, so carrying the fact there is necessary but not sufficient: no
// consumer outside internal/indexer ever sees a graph.MutationReceipt. What a
// consumer DOES see is MutationResult — the terminal value every admitted file
// mutation reports on its ticket, and the value internal/mcp turns into the
// graph half of an edit response and of mutation_status.
//
// Watcher.EnqueueFileMutation is the production entrypoint: a daemon-backed
// MCP edit reaches the graph through it and through nothing else
// (internal/mcp/edit_serialization.go mutationReindexState -> mutationScheduler
// -> Watcher.EnqueueFileMutation), and it does not depend on fsnotify
// delivery. Driving it here is what makes these tests a wiring proof rather
// than another primitive test.
//
// Both run on *store_sqlite.Store — the only graph.Store the daemon ever holds
// (internal/serverstack/backend.go openSqliteBackend -> store_sqlite.Open) — so
// a chain that only the in-memory graph can complete fails here.

// enqueueAffectedByPointMutation indexes a one-definition corpus on store under
// cap, rewrites the definition's signature, and drives the rewrite through the
// watcher's admitted-mutation path, returning the terminal ticket result.
func enqueueAffectedByPointMutation(
	t *testing.T, store graph.Store, callers, maxFiles int,
) (MutationResult, *observer.ObservedLogs, *affectedByBoundObservation) {
	t.Helper()
	return enqueueAffectedByPointMutationWith(t, store, callers, maxFiles, nil)
}

// enqueueAffectedByPointMutationWith is the same drive with a hook that can
// adjust the Indexer and the Watcher — the point-executor seam included —
// before the edit is admitted.
func enqueueAffectedByPointMutationWith(
	t *testing.T, store graph.Store, callers, maxFiles int,
	arrange func(idx *Indexer, w *Watcher),
) (MutationResult, *observer.ObservedLogs, *affectedByBoundObservation) {
	t.Helper()
	idx, _, observed, obs, dir, defPaths := affectedByBoundFixtureOn(t, store, 1, callers, maxFiles)
	idx.SetRootPath(dir)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	if arrange != nil {
		arrange(idx, w)
	}

	bumpMtime(t, defPaths[0], "package p\n\nfunc F0(x int, y int) int { return x + y }\n")
	ticket, err := w.EnqueueFileMutation(context.Background(), defPaths[0])
	require.NoError(t, err)
	require.NotNil(t, ticket, "the fixture path was not admitted; the mutation never reached the graph")

	select {
	case result := <-ticket.Done:
		return result, observed, obs
	case <-time.After(60 * time.Second):
		t.Fatalf("mutation ticket generation %d did not complete", ticket.Generation)
		return MutationResult{}, nil, nil
	}
}

// A point mutation whose affected-by fan-out is cut must report the cut on the
// ticket the caller is holding — the graph read the new bytes, and the files
// the bound dropped still carry edges and persisted reference facts derived
// from the OLD signature.
//
// Revert-red: remove the derived-fan-out window from
// patchGraphWithReceiptStateRawModern (or stop threading it into
// completeMutationWaitersWithFanout, or drop the field from MutationResult)
// and a knowingly incomplete mutation reports Observed=false — the shape in
// which the fact reached the receipt and then reached no consumer at all.
func TestAdmittedPointMutationReportsTheDerivedFanoutCut(t *testing.T) {
	// Three callers of one definition against a cap of two: the union (3)
	// exceeds the bound, so exactly one referencing file is left stale.
	result, observed, obs := enqueueAffectedByPointMutation(t, newSqliteGraph(t), 3, 2)

	// Anti-vacuity: the pass really was cut in this run, and the indexer did
	// not announce the cut as reaching no carrier.
	require.Len(t, truncationEntries(observed), 1,
		"the fixture did not truncate; the assertions below would be vacuous")
	require.Empty(t, obs.uncarriedCuts(),
		"the indexer announced the cut as reaching no receipt carrier")

	require.NoError(t, result.Err)
	require.True(t, result.Reindexed, "the mutation did not apply: %+v", result)
	require.True(t, result.DerivedFanout.Observed,
		"the admitted mutation reported no fan-out verdict at all: %+v", result.DerivedFanout)
	require.False(t, result.DerivedFanout.Complete,
		"a truncated fan-out reported as complete: %+v", result.DerivedFanout)
	assert.Equal(t, 1, result.DerivedFanout.Dropped,
		"the result must name the size of the hole: %+v", result.DerivedFanout)
	assert.Equal(t, []string{affectedByFanoutPass}, result.DerivedFanout.Passes,
		"the result must name WHICH derived pass was bounded")

	// The fan-out axis is additive: a cut pass must not make the mutation
	// itself look failed, or every bounded fan-out would read as a broken edit.
	assert.True(t, result.Reindexed)
}

// The same shape under the bound. "Complete" has to be a POSITIVE answer:
// a consumer must be able to tell "the derived pass finished" from "this path
// never measured it", and only Observed draws that line.
func TestAdmittedPointMutationReportsACompleteFanout(t *testing.T) {
	result, observed, obs := enqueueAffectedByPointMutation(t, newSqliteGraph(t), 3, 8)

	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	require.Empty(t, truncationEntries(observed), "a complete fan-out must record no truncation")
	require.Empty(t, obs.dropped)

	require.True(t, result.DerivedFanout.Observed,
		"a complete pass must still be reported as observed, or it is indistinguishable "+
			"from a path that never measured the fan-out: %+v", result.DerivedFanout)
	assert.True(t, result.DerivedFanout.Complete,
		"an untruncated pass reported a hole: %+v", result.DerivedFanout)
	assert.Zero(t, result.DerivedFanout.Dropped)
	assert.Empty(t, result.DerivedFanout.Passes)
}

// The verdict must be produced WITHOUT an open store mutation receipt.
//
// The first shape of this item read the fact back out of a store-wide receipt
// the watcher opened around the whole point mutation. That worked, and it cost:
// a receipt-active store leaves its read-free reindex path
// (store_sqlite/reindex_receipt.go) and its cheap AddBatch/evict arms for as
// long as the window is open, and the window covered the resolver and derived
// catch-up that incremental_watcher_batch.go keeps deliberately OUTSIDE the
// receipt boundary — taxing every other repository writing to that store for
// the mutation's full duration.
//
// Revert-red: go back to a receipt-backed window and this test cannot produce a
// verdict at all, because no receipt is open here.
func TestDerivedFanoutObservationNeedsNoOpenStoreReceipt(t *testing.T) {
	g := graph.New()
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())

	observation := beginDerivedFanoutObservation(idx)
	require.NotNil(t, observation)
	idx.reportAffectedByTruncation("scope", affectedByTruncation{
		Truncated: true, Cap: 1, Considered: 3,
		Dropped: []string{"src/z.go", "src/y.go"},
	})
	fact := observation.close()

	require.True(t, fact.Observed, "the pass reported nothing without a store receipt: %+v", fact)
	assert.False(t, fact.Complete)
	assert.Equal(t, 2, fact.Dropped)
	assert.Equal(t, []string{affectedByFanoutPass}, fact.Passes)
}

// A window in which no bounded pass ever applied its bound must report NOTHING
// observed. "Observed" draws the line between "the derived pass finished" and
// "this path never measured it", and a window that only proves a watcher was
// listening proves nothing about the pass.
//
// close() is deferred as a panic leak guard AND called inline for the verdict,
// so it runs twice on every mutation: the second run must also report nothing.
func TestDerivedFanoutObservationReportsNothingWithoutAPass(t *testing.T) {
	idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())

	silent := beginDerivedFanoutObservation(idx)
	require.NotNil(t, silent)
	assert.Equal(t, DerivedFanoutCompleteness{}, silent.close(),
		"a window nobody ran a pass in certified a complete fan-out")

	ran := beginDerivedFanoutObservation(idx)
	idx.reportAffectedByTruncation("scope", affectedByTruncation{Cap: 8, Considered: 3})
	assert.Equal(t, DerivedFanoutCompleteness{Observed: true, Complete: true}, ran.close(),
		"a pass that ran under its bound must publish a POSITIVE complete verdict")
	assert.Equal(t, DerivedFanoutCompleteness{}, ran.close(),
		"a second close reported a verdict from an already-closed window")

	// A pass that reports after the window closed cannot resurrect it.
	idx.reportAffectedByTruncation("scope", affectedByTruncation{
		Truncated: true, Cap: 1, Considered: 2, Dropped: []string{"src/late.go"},
	})
	assert.Equal(t, DerivedFanoutCompleteness{}, ran.close())

	// And nothing leaks: the side table is empty again.
	derivedFanoutObservers.mu.Lock()
	live := len(derivedFanoutObservers.by[idx])
	derivedFanoutObservers.mu.Unlock()
	assert.Zero(t, live, "a closed window stayed registered on its Indexer")
}

// The verdict is per-REPOSITORY. A store mutation receipt is store-wide —
// store_sqlite RecordMutationFanoutTruncation writes into every receipt open on
// the store — so under MultiWatcher, where sibling repositories share one
// *store_sqlite.Store, a receipt-backed window would let repository B's cut land
// on repository A's edit response and tell A's caller to re-resolve dependants
// that are not A's.
//
// Revert-red: key the observation on the store instead of on the Indexer and
// A's verdict inherits B's hole.
func TestDerivedFanoutObservationIsScopedToItsIndexer(t *testing.T) {
	shared := newSqliteGraph(t)
	repoA := New(shared, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	repoA.SetRepoPrefix("a")
	repoB := New(shared, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	repoB.SetRepoPrefix("b")

	windowA := beginDerivedFanoutObservation(repoA)
	windowB := beginDerivedFanoutObservation(repoB)

	// A ran its pass cleanly; B's pass was cut, concurrently, on the same store.
	repoA.reportAffectedByTruncation("a", affectedByTruncation{Cap: 8, Considered: 2})
	repoB.reportAffectedByTruncation("b", affectedByTruncation{
		Truncated: true, Cap: 1, Considered: 3,
		Dropped: []string{"b/one.go", "b/two.go"},
	})

	factA := windowA.close()
	factB := windowB.close()

	require.True(t, factA.Observed)
	assert.True(t, factA.Complete,
		"a sibling repository's bounded pass landed on this mutation's verdict: %+v", factA)
	assert.Zero(t, factA.Dropped)

	require.True(t, factB.Observed)
	assert.False(t, factB.Complete, "the cut repository lost its own verdict: %+v", factB)
	assert.Equal(t, 2, factB.Dropped)
}

// DerivedFanoutFromReceipt is the single lowering from the receipt axis to the
// fact a mutation caller reads. Pin the shape: passes sorted and named, the
// dropped count taken from the receipt's cross-pass UNION (never a sum), and a
// clean receipt lowered to a positive complete answer.
func TestDerivedFanoutFromReceiptLowersThePerPassFacts(t *testing.T) {
	complete := DerivedFanoutFromReceipt(graph.MutationReceipt{Complete: true})
	assert.Equal(t, DerivedFanoutCompleteness{Observed: true, Complete: true}, complete)

	cut := DerivedFanoutFromReceipt(graph.MutationReceipt{
		Complete: true,
		FanoutTruncations: []graph.ReceiptFanoutTruncation{
			{Pass: "incoming_sources", Dropped: 1, DroppedFiles: []string{"pkg/b.go"}},
			{Pass: affectedByFanoutPass, Cap: 2, Considered: 3,
				Dropped: 2, DroppedFiles: []string{"pkg/a.go", "pkg/b.go"}},
		},
	})
	assert.True(t, cut.Observed)
	assert.False(t, cut.Complete)
	assert.Equal(t, []string{affectedByFanoutPass, "incoming_sources"}, cut.Passes,
		"passes must be sorted so two runs of one batch render identically")
	assert.Equal(t, 2, cut.Dropped,
		"the count must be the union of the named files across passes, not their sum")
}

// A FAILED point mutation must publish NO fan-out verdict.
//
// This is the defect class the item exists to close, in the arm that is easiest
// to get wrong: the bounded pass really did run and really was cut, but the
// mutation the caller asked for did not apply. Publishing "complete" there
// certifies derived work over bytes the graph never accepted; publishing the
// cut is barely better, because the caller is being told about a hole in a
// mutation that never landed.
//
// Both failure shapes are covered because two independent gates carry them:
//
//   - the executor itself fails (the daemon's incrementalPointRepoRaw errors on
//     "repository not found", "no live indexer" and "reindexing %s: %w"), which
//     patchGraphWithReceiptStateRawModern must refuse to stamp; and
//   - the executor succeeds but the patch fails afterwards (a failed file in
//     the result), which the completion choke point must drop.
//
// Revert-red: remove either gate and the matching sub-test reports
// Observed=true over a mutation that returned an error.
func TestFailedPointMutationPublishesNoDerivedFanoutVerdict(t *testing.T) {
	t.Run("executor fails after the bounded pass ran", func(t *testing.T) {
		result, observed, _ := enqueueAffectedByPointMutationWith(t, newSqliteGraph(t), 3, 2,
			func(idx *Indexer, w *Watcher) {
				w.pointReindexRaw = func(path string) (*IndexResult, error) {
					// Run the real executor first so the bounded pass records a
					// genuine cut into this mutation's window, then fail.
					_, _ = idx.incrementalPointWatcherPath(idx.rootPath, path)
					return nil, errors.New("watcher-test: executor failed after the bounded pass")
				}
			})

		// Anti-vacuity: the pass really was cut in this run, so a verdict was
		// available to publish and was deliberately withheld.
		require.Len(t, truncationEntries(observed), 1,
			"the fixture did not truncate; the assertion below would be vacuous")
		require.Error(t, result.Err, "the fixture did not fail the mutation: %+v", result)
		require.False(t, result.Reindexed)
		assert.Equal(t, DerivedFanoutCompleteness{}, result.DerivedFanout,
			"a failed mutation published a fan-out verdict: %+v", result.DerivedFanout)
	})

	t.Run("patch fails after the executor returned", func(t *testing.T) {
		result, observed, _ := enqueueAffectedByPointMutationWith(t, newSqliteGraph(t), 3, 2,
			func(idx *Indexer, w *Watcher) {
				w.pointReindexRaw = func(path string) (*IndexResult, error) {
					out, err := idx.incrementalPointWatcherPath(idx.rootPath, path)
					if out != nil {
						out.FailedFiles = append(out.FailedFiles, path)
					}
					return out, err
				}
			})

		require.Len(t, truncationEntries(observed), 1,
			"the fixture did not truncate; the assertion below would be vacuous")
		require.Error(t, result.Err, "the fixture did not fail the mutation: %+v", result)
		require.False(t, result.Reindexed)
		assert.Equal(t, DerivedFanoutCompleteness{}, result.DerivedFanout,
			"a mutation that failed after the executor published a fan-out verdict: %+v",
			result.DerivedFanout)
	})
}

// A repository whose global passes are deferred never runs the bounded
// affected-by pass at all (commitStructuralIncrementalBatch merges the deferred
// plan only when deferGlobalPasses is clear). That mutation must report NOTHING
// observed: it is indistinguishable from a complete pass only to a discriminator
// that means "a window was open" instead of "the pass ran".
//
// Revert-red: set Observed from the window's existence rather than from a
// bounded pass applying its bound, and this deferred mutation certifies derived
// work that was never attempted.
func TestPointMutationWithDeferredGlobalPassesPublishesNoVerdict(t *testing.T) {
	result, observed, _ := enqueueAffectedByPointMutationWith(t, newSqliteGraph(t), 3, 2,
		func(idx *Indexer, _ *Watcher) { idx.SetDeferGlobalPasses(true) })

	require.NoError(t, result.Err)
	require.True(t, result.Reindexed, "the mutation did not apply: %+v", result)
	require.Empty(t, truncationEntries(observed),
		"the deferred run executed the bounded pass after all; the premise is gone")
	assert.Equal(t, DerivedFanoutCompleteness{}, result.DerivedFanout,
		"a mutation that never ran the bounded pass published a verdict: %+v", result.DerivedFanout)
}

// The daemon does NOT run the standalone point executor. MultiWatcher replaces
// the seam with MultiIndexer.incrementalPointRepoRaw (multi_watcher.go), which
// reaches the bounded pass through incrementalReindexRepoRawMode instead. Drive
// that arm end to end so a change to the MultiIndexer executor's receipt or
// catch-up handling cannot silently drop the verdict the shipped daemon
// publishes.
func TestDaemonPointExecutorReportsTheDerivedFanoutCut(t *testing.T) {
	const prefix = "repo"
	store := newSqliteGraph(t)
	idx, _, observed, obs, dir, defPaths := affectedByBoundFixtureFor(t, store, prefix, 1, 3, 2)
	idx.SetRootPath(dir)

	multi := NewMultiIndexer(store, newTestRegistry(), nil, nil, zap.NewNop())
	multi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: dir}
	multi.indexers[prefix] = idx
	idx.attachRepositoryMutationCoordinator(multi.repositoryMutationCoordinator(prefix))

	mw, err := NewMultiWatcher(multi, map[string]config.WatchConfig{
		prefix: {Enabled: true, DebounceMs: 5},
	}, zap.NewNop())
	require.NoError(t, err)
	w := mw.watchers[prefix]
	require.NotNil(t, w, "MultiWatcher registered no watcher for %s", prefix)
	require.NotNil(t, w.pointReindexRaw,
		"the daemon arm under test is not installed; this would silently run the standalone executor")

	bumpMtime(t, defPaths[0], "package p\n\nfunc F0(x int, y int) int { return x + y }\n")
	ticket, err := w.EnqueueFileMutation(context.Background(), defPaths[0])
	require.NoError(t, err)
	require.NotNil(t, ticket, "the fixture path was not admitted on the daemon arm")

	var result MutationResult
	select {
	case result = <-ticket.Done:
	case <-time.After(60 * time.Second):
		t.Fatalf("daemon-arm mutation generation %d did not complete", ticket.Generation)
	}

	require.Len(t, truncationEntries(observed), 1,
		"the daemon arm did not truncate; the assertions below would be vacuous")
	require.Empty(t, obs.uncarriedCuts())
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed, "the daemon-arm mutation did not apply: %+v", result)
	require.True(t, result.DerivedFanout.Observed,
		"the daemon's point executor reported no fan-out verdict: %+v", result.DerivedFanout)
	assert.False(t, result.DerivedFanout.Complete)
	assert.Equal(t, 1, result.DerivedFanout.Dropped)
	assert.Equal(t, []string{affectedByFanoutPass}, result.DerivedFanout.Passes)
}

// The refusal is made where the patch is, not only where the ticket is
// completed. patchGraphObservingFanout is the function runPointMutation hands
// the out-parameter to, and it must leave that parameter untouched when the
// patch fails — otherwise the completion choke point is the only thing standing
// between a failed edit and a positive certification, and a future caller of
// this function would inherit the defect.
//
// Revert-red: stamp the observation before the error check and the out-parameter
// comes back {Observed:true, ...} for a patch that returned an error.
func TestPatchGraphObservingFanoutStampsNothingOnAFailedPatch(t *testing.T) {
	idx, _, observed, _, dir, defPaths := affectedByBoundFixtureOn(t, newSqliteGraph(t), 1, 3, 2)
	idx.SetRootPath(dir)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	w.pointReindexRaw = func(path string) (*IndexResult, error) {
		// The bounded pass runs for real and is cut, so a verdict exists.
		_, _ = idx.incrementalPointWatcherPath(idx.rootPath, path)
		return nil, errors.New("watcher-test: executor failed after the bounded pass")
	}

	bumpMtime(t, defPaths[0], "package p\n\nfunc F0(x int, y int) int { return x + y }\n")
	var fanout DerivedFanoutCompleteness
	patchErr := w.patchGraphObservingFanout(defPaths[0], ChangeModified, 0, &fanout)

	require.Error(t, patchErr, "the fixture did not fail the patch")
	require.Len(t, truncationEntries(observed), 1,
		"the bounded pass did not run; the assertion below would be vacuous")
	assert.Equal(t, DerivedFanoutCompleteness{}, fanout,
		"a failed patch stamped a fan-out verdict: %+v", fanout)
}

// ---------------------------------------------------------------------------
// The receipt's fan-out axis is per-MUTATION, bounded to the receipt batch
// ---------------------------------------------------------------------------
//
// The verdict on the ticket is already per-Indexer (the observation window
// above). The RECEIPT is the other carrier, and it was not: a bounded pass
// publishes its cut through a store-wide broadcast
// (carryAffectedByTruncationOnReceipt -> Store.RecordMutationFanoutTruncation)
// that names the pass and the files it dropped but never the repository or the
// mutation that ran it. Every repository the daemon indexes mutates ONE
// *store_sqlite.Store, so several receipt windows are open at once and the
// broadcast lands on all of them.
//
// incrementalReindexPathsWithReceiptMode now opens a window that OWNS its
// fan-out axis and hands it exactly the facts this Indexer observed inside the
// batch the window brackets.

// A sibling repository's cut, taken on the same store while this mutation's
// receipt window is open, must not appear on this mutation's receipt.
//
// Revert-red: open the window with BeginMutationReceipt instead of
// BeginMutationReceiptOwningFanout (or drop the fanoutOwned skip in
// store_sqlite RecordMutationFanoutTruncation) and repository A's receipt
// reports four dropped files, two of which are B's.
func TestBatchReceiptFanoutRefusesASiblingRepositorysCut(t *testing.T) {
	shared := newSqliteGraph(t)
	repoA, _, observedA, obsA, dirA, defPathsA := affectedByBoundFixtureFor(t, shared, "a", 2, 3, 4)
	repoB := New(shared, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	repoB.SetRepoPrefix("b")

	// The watcher's wider window, open across the whole mutation. The receipt
	// window nests inside it and must not steal its facts: the mutation's own
	// verdict spans the resolver/derived catch-up too, and is carried
	// separately from the receipt axis.
	outer := beginDerivedFanoutObservation(repoA)

	foreign := affectedByTruncation{
		Truncated: true, Cap: 1, Considered: 3,
		Dropped: []string{"b/one.go", "b/two.go"},
	}
	repoA.incrementalCatchupHook = func(kind string, files []string) {
		obsA.record(kind, files)
		if kind == "affected_by_truncated" {
			// B's bounded pass is cut while A's receipt window is open.
			repoB.reportAffectedByTruncation("b", foreign)
		}
	}

	bumpAffectedByDefs(t, defPathsA)
	_, receipt, _, err := repoA.incrementalReindexPathsWithReceiptMode(dirA, defPathsA, incrementalPathMode{})
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.Len(t, truncationEntries(observedA), 1,
		"repository A did not truncate; the assertions below would be vacuous")
	require.Len(t, obsA.dropped, 1)

	fact, ok := receipt.FanoutTruncationFor(affectedByFanoutPass)
	require.True(t, ok, "the receipt lost this mutation's own cut: %+v", receipt.FanoutTruncations)
	assert.ElementsMatch(t, obsA.dropped[0], fact.DroppedFiles,
		"the receipt must carry exactly the set this mutation's pass dropped")
	for _, foreignFile := range foreign.Dropped {
		assert.NotContains(t, fact.DroppedFiles, foreignFile,
			"a sibling repository's dropped file landed on this mutation's receipt: %+v", fact)
	}
	assert.Equal(t, 2, fact.Dropped)
	assert.Equal(t, 2, receipt.DroppedFanoutFiles())
	assert.Equal(t, 4, fact.Cap)
	assert.Equal(t, 6, fact.Considered)
	assert.True(t, receipt.Complete,
		"a bounded fan-out voided the delta receipt: %+v", *receipt)

	// The wider window still saw A's own cut: the receipt window consumed it
	// without absorbing it.
	verdict := outer.close()
	require.True(t, verdict.Observed,
		"the nested receipt window swallowed the mutation's verdict: %+v", verdict)
	assert.False(t, verdict.Complete)
	assert.Equal(t, 2, verdict.Dropped)
}

// The axis is bounded to the RECEIPT BATCH: the window admits only the cuts it
// OBSERVED, never an unattributed broadcast that merely overlapped it in time.
//
// The broadcast below is the shape every producer outside this batch takes on
// the shared store — a sibling repository's deferred resolver catch-up, which
// incremental_watcher_batch.go keeps outside the receipt boundary on purpose,
// or a concurrent full-repo reindex (multi.go) whose passes carry their cuts
// the same way. None of them is this batch's, and none of them may make this
// mutation's receipt name files this batch never touched.
//
// Revert-red: leave the window unowned and the broadcast lands, so a batch that
// truncated nothing reports a hole.
// fanoutBroadcastingStore is the production backend plus one unattributed
// fan-out broadcast, fired from inside a graph WRITE so it provably lands while
// the mutation's receipt window is open. The broadcast is the store-wide
// channel every bounded pass publishes through
// (graph.NoteMutationFanoutTruncation -> Store.RecordMutationFanoutTruncation);
// what makes it foreign here is only that this window never observed the pass.
type fanoutBroadcastingStore struct {
	*store_sqlite.Store
	armed atomic.Bool
	fact  graph.ReceiptFanoutTruncation
}

func (s *fanoutBroadcastingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if s.armed.CompareAndSwap(true, false) {
		s.RecordMutationFanoutTruncation(s.fact)
	}
	s.Store.AddBatch(nodes, edges)
}

func TestBatchReceiptFanoutAdmitsOnlyWhatItObserved(t *testing.T) {
	base := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	t.Cleanup(func() { _ = base.Close() })
	shared := &fanoutBroadcastingStore{Store: base, fact: graph.ReceiptFanoutTruncation{
		Pass: affectedByFanoutPass, Cap: 1, Considered: 4, Dropped: 1,
		DroppedFiles: []string{"elsewhere/stale.go"},
	}}

	idx, _, observed, _, dir, defPaths := affectedByBoundFixtureOn(t, shared, 2, 2, 8)
	shared.armed.Store(true)

	bumpAffectedByDefs(t, defPaths)
	_, receipt, _, err := idx.incrementalReindexPathsWithReceiptMode(dir, defPaths, incrementalPathMode{})
	require.NoError(t, err)
	require.NotNil(t, receipt)
	require.False(t, shared.armed.Load(),
		"the foreign broadcast never fired inside the window; the assertions would be vacuous")
	require.Empty(t, truncationEntries(observed),
		"this batch was supposed to stay under its bound; the assertions would be vacuous")

	assert.True(t, receipt.DerivedFanoutComplete(),
		"a cut this batch never ran landed on its receipt: %+v", receipt.FanoutTruncations)
	assert.Empty(t, receipt.FanoutTruncations)
	assert.Zero(t, receipt.DroppedFanoutFiles())

	// The window is a batch's, so it must be gone with the batch. A stranded
	// one would silently absorb every later pass's facts on this Indexer — and
	// one per incremental batch never returns.
	derivedFanoutObservers.mu.Lock()
	live := len(derivedFanoutObservers.by[idx])
	derivedFanoutObservers.mu.Unlock()
	assert.Zero(t, live, "the batch's fan-out window stayed registered on its Indexer")
}
