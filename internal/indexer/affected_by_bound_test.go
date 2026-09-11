package indexer

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
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
	}
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

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.AffectedByReresolveMax = maxFiles
	core, observed := observer.New(zap.DebugLevel)
	idx := New(g, reg, cfg, zap.New(core))
	obs := &affectedByBoundObservation{}
	idx.incrementalCatchupHook = obs.record
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Fixture precondition: every caller really is bound to its definition,
	// otherwise the fan-out this test measures would be vacuously empty.
	for d := 0; d < defs; d++ {
		target := fnNodeID(t, g, fmt.Sprintf("def%d.go", d), fmt.Sprintf("F%d", d))
		for c := 0; c < callersPer; c++ {
			callerFile := fmt.Sprintf("caller%d_%d.go", d, c)
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
	chunk := func(files ...string) affectedByBatchPlan {
		return affectedByBatchPlan{
			files:    files,
			maxFiles: 3,
			notify:   func(f affectedByTruncation) { reported = append(reported, f) },
		}
	}

	first := batch.mergeDeferredAffected(chunk("b.go", "c.go"))
	assert.False(t, first.truncation.Truncated, "two files under a cap of three is complete")
	assert.Equal(t, []string{"b.go", "c.go"}, first.files)
	assert.Empty(t, reported)

	second := batch.mergeDeferredAffected(chunk("a.go", "d.go", "e.go"))
	assert.True(t, second.truncation.Truncated,
		"the union across chunks (5) exceeds the cap and must be cut once")
	assert.Equal(t, 5, second.truncation.Considered)
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, second.files,
		"the bound keeps the sorted prefix of the whole union")
	assert.Equal(t, []string{"d.go", "e.go"}, second.truncation.Dropped)
	require.Len(t, reported, 1, "a merge that cuts must report the cut even when the caller drops the plan")
	assert.Equal(t, []string{"d.go", "e.go"}, reported[0].Dropped)

	// What the catch-up executes — and therefore what it persists reference
	// facts for — is the bounded union, not the accumulated one.
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, batch.deferredAffectedPlan().files)

	// A pruned file must not re-enter on the next merge, or the bound
	// degrades back to a per-chunk bound.
	third := batch.mergeDeferredAffected(chunk("f.go"))
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, third.files)
	assert.Equal(t, []string{"f.go"}, third.truncation.Dropped)
	assert.Len(t, reported, 2)
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
