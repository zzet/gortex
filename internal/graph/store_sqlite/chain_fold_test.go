package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// foldChain builds a three-member chain with every layer rule at work and a
// bulk of rows per member, each member ready (as a fold finds them).
func foldChain(t *testing.T, store *Store, bulk int) []int64 {
	t.Helper()
	ctx := context.Background()
	many := func(prefix string, n int) ([]*graph.Node, []*graph.Edge) {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for i := 0; i < n; i++ {
			file := fmt.Sprintf("repo/%s%03d.go", prefix, i%50)
			id := fmt.Sprintf("%s::%s%05d", file, prefix, i)
			nodes = append(nodes, flatNode(id, file))
			edges = append(edges, flatEdge(id, fmt.Sprintf("repo/t.go::T%d", i%7), file))
		}
		return nodes, edges
	}
	bn, be := many("bulk", bulk)
	bottom := flattenMember(t, store, "bottom",
		append([]*graph.Node{flatNode("repo/a.go::A", "repo/a.go"), flatNode("repo/b.go::B", "repo/b.go"), flatNode("repo/gone.go::G", "repo/gone.go")}, bn...),
		append([]*graph.Edge{flatEdge("repo/a.go::A", "repo/t.go::T1", "repo/a.go"), flatEdge("repo/b.go::B", "repo/t.go::T1", "repo/b.go")}, be...),
		[]FileMask{
			{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/b.go", Mode: OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/gone.go", Mode: OwnershipReplace},
		}, nil, nil)
	mn, me := many("mid", bulk/2)
	middle := flattenMember(t, store, "middle",
		append([]*graph.Node{flatNode("repo/a.go::A", "repo/a.go")}, mn...),
		append([]*graph.Edge{flatEdge("repo/a.go::A", "repo/t.go::T3", "repo/a.go")}, me...),
		[]FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace}, {RepoPrefix: "repo", FilePath: "repo/bulk007.go", Mode: OwnershipDelete}},
		nil, nil)
	top := flattenMember(t, store, "top",
		[]*graph.Node{flatNode("repo/a.go::A", "repo/a.go")},
		[]*graph.Edge{flatEdge("repo/a.go::A", "repo/t.go::T2", "repo/a.go"), flatEdge("repo/b.go::B", "repo/t.go::T2", "repo/b.go")},
		[]FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace}},
		[]string{"repo/gone.go::G"}, []string{"repo/b.go::B"})
	for _, id := range []int64{bottom, middle, top} {
		var items []graph.SymbolFTSItem
		for _, n := range store.AtGeneration(id).AllNodes() {
			items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "tok " + n.Name})
		}
		require.NoError(t, store.AtGeneration(id).BatchUpsertSymbolFTS(items))
		require.NoError(t, store.Catalog().SetViewGenerationState(ctx, id, ViewGenerationReady, ViewGenerationBuilding))
	}
	return []int64{bottom, middle, top}
}

func renderFoldFTS(t *testing.T, store *Store, id int64) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT m.node_id, f.tokens FROM symbol_fts_rowid m JOIN symbol_fts f ON f.rowid = m.fts_rowid WHERE m.view_gen = ? ORDER BY m.node_id`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var node, tokens string
		require.NoError(t, rows.Scan(&node, &tokens))
		out = append(out, node+"="+tokens)
	}
	require.NoError(t, rows.Err())
	return out
}

func renderFoldMasks(t *testing.T, store *Store, id int64) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	files, err := store.AtGeneration(id).FileMasksContext(ctx)
	require.NoError(t, err)
	for _, m := range files {
		out = append(out, "file:"+m.FilePath+":"+string(m.Mode))
	}
	ids, err := store.AtGeneration(id).NodeIdentityMasksContext(ctx)
	require.NoError(t, err)
	for _, m := range ids {
		out = append(out, "id:"+m.NodeID)
	}
	marks, err := store.AtGeneration(id).EdgeSourceMasksContext(ctx)
	require.NoError(t, err)
	for _, m := range marks {
		out = append(out, "src:"+m.SourceID)
	}
	slices.Sort(out)
	return out
}

func smallFoldSteps(t *testing.T) {
	t.Helper()
	prevFirst, prevMin, prevMax, prevTarget := chainFoldFirstRows, chainFoldMinRows, chainFoldMaxRows, chainFoldStepTarget
	chainFoldFirstRows, chainFoldMinRows, chainFoldMaxRows, chainFoldStepTarget = 150, 64, 300, time.Millisecond
	t.Cleanup(func() {
		chainFoldFirstRows, chainFoldMinRows, chainFoldMaxRows, chainFoldStepTarget = prevFirst, prevMin, prevMax, prevTarget
	})
}

func runFold(t *testing.T, fold *ChainFold, between func(step int)) {
	t.Helper()
	ctx := context.Background()
	yielded, overMark := 0, 0
	for step := 0; step < 100000; step++ {
		done, err := fold.Step(ctx)
		if errors.Is(err, ErrChainFoldYielded) {
			yielded++
			time.Sleep(time.Millisecond)
			continue
		}
		if errors.Is(err, ErrChainFoldWALMark) {
			overMark++
			if overMark%1000 == 0 {
				t.Logf("over the WAL mark %d times: wal=%d", overMark, walFileSize(fold.s.dbPath+"-wal"))
			}
			time.Sleep(time.Millisecond)
			continue
		}
		require.NoError(t, err)
		if done {
			return
		}
		if between != nil {
			between(step)
		}
	}
	t.Fatalf("the fold did not finish: yielded=%d over_mark=%d", yielded, overMark)
}

// The stepped fold writes exactly what the one-shot flatten writes: nodes,
// edges, masks and symbol documents, over a chain whose members hide each
// other's rows by every layer rule, in many small steps — and still when the
// writer connection's temp tables are lost between steps.
func TestChainFoldInStepsEqualsTheOneShotFlatten(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	chain := foldChain(t, store, 1500)
	oneShot := reservedGeneration(t, store, "one-shot")
	_, err := store.FlattenGenerationChain(ctx, chain, oneShot)
	require.NoError(t, err)

	smallFoldSteps(t)
	stepped := reservedGeneration(t, store, "stepped")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: stepped, Owner: "test"})
	require.NoError(t, err)
	runFold(t, fold, func(step int) {
		if step%7 == 3 {
			// A replaced writer connection: the fold's scratch is gone.
			store.writeMu.Lock()
			for _, table := range []string{"paths", "ids", "sources", "endpoints"} {
				_, _ = store.writerDB.Exec(`DROP TABLE IF EXISTS temp.fold_hidden_` + table)
			}
			store.writeMu.Unlock()
		}
	})
	counts, steps, _ := fold.Counts()
	require.NoError(t, fold.Release(ctx))
	t.Logf("stepped fold: steps=%d rows=%d nodes=%d edges=%d", steps, counts.Rows, counts.Nodes, counts.Edges)
	require.Greater(t, steps, 20, "precondition: the fold ran in many steps")
	require.Equal(t, renderGenerationNodes(t, store, oneShot), renderGenerationNodes(t, store, stepped))
	require.Equal(t, renderGenerationEdges(t, store, oneShot), renderGenerationEdges(t, store, stepped))
	require.Equal(t, renderFoldMasks(t, store, oneShot), renderFoldMasks(t, store, stepped))
	require.Equal(t, renderFoldFTS(t, store, oneShot), renderFoldFTS(t, store, stepped))
	require.NotEmpty(t, renderFoldFTS(t, store, stepped))
}

// A step gives way to an edit: an announced write interrupts the step in
// flight, the gate is free within milliseconds, the step is rolled back, and
// the fold continues to the same result.
func TestChainFoldStepGivesWayToAnEdit(t *testing.T) {
	for _, announced := range []bool{true, false} {
		t.Run(fmt.Sprintf("announced=%t", announced), func(t *testing.T) {
			store := openCatalogStore(t)
			ctx := context.Background()
			chain := foldChain(t, store, 20000)
			prevFirst, prevMax := chainFoldFirstRows, chainFoldMaxRows
			chainFoldFirstRows, chainFoldMaxRows = 20000, 20000 // one long step
			t.Cleanup(func() { chainFoldFirstRows, chainFoldMaxRows = prevFirst, prevMax })
			to := reservedGeneration(t, store, "folded")
			fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
			require.NoError(t, err)
			// Step up to the bottom member's rows: the long part.
			for fold.member != 0 || fold.phase != foldPhaseRows {
				_, err = fold.Step(ctx)
				if errors.Is(err, ErrChainFoldYielded) { // a background writer (index build, counters)
					time.Sleep(time.Millisecond)
					continue
				}
				require.NoError(t, err)
			}
			checkout := store.AnnounceCheckoutRefresh()
			defer checkout()
			before := renderGenerationNodes(t, store, to)
			cursor := fold.cursor()
			// The next step is long: 20,000 rows with no time target to stop it.
			prevTarget, prevRate := chainFoldStepTarget, fold.usPerRow
			chainFoldStepTarget, fold.usPerRow = 10*time.Second, 0
			t.Cleanup(func() { chainFoldStepTarget = prevTarget })

			var wait time.Duration
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(30 * time.Millisecond) // inside the long step
				if announced {
					release := store.AnnounceWrite()
					defer release()
				}
				announced := time.Now()
				store.writeMu.Lock()
				wait = time.Since(announced)
				store.writeMu.Unlock()
			}()
			started := time.Now()
			_, err = fold.Step(ctx)
			stepTook := time.Since(started)
			wg.Wait()
			t.Logf("interrupted step: err=%v after %s; the edit waited %s for the gate", err, stepTook.Round(time.Millisecond), wait.Round(time.Microsecond))
			require.ErrorIs(t, err, ErrChainFoldYielded)
			require.Equal(t, cursor, fold.cursor(), "an interrupted step must not advance its cursor")
			limit := 20 * time.Millisecond
			if raceDetectorOn {
				limit = 100 * time.Millisecond
			}
			require.Less(t, wait, limit, "the edit waited for the fold's step")
			require.Equal(t, before, renderGenerationNodes(t, store, to), "the interrupted step's rows were not rolled back")
			chainFoldStepTarget, fold.usPerRow = prevTarget, prevRate

			runFold(t, fold, nil)
			require.NoError(t, fold.Release(ctx))
			oneShot := reservedGeneration(t, store, "one-shot")
			_, err = store.FlattenGenerationChain(ctx, chain, oneShot)
			require.NoError(t, err)
			require.Equal(t, renderGenerationNodes(t, store, oneShot), renderGenerationNodes(t, store, to))
			require.Equal(t, renderGenerationEdges(t, store, oneShot), renderGenerationEdges(t, store, to))

		})
	}
}

// Steps are sized by time: after the first, a step holds the gate for about
// chainFoldStepTarget.
func TestChainFoldStepsAreSizedByTime(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	chain := foldChain(t, store, 20000)
	to := reservedGeneration(t, store, "folded")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(ctx) }()
	var holds []time.Duration
	for i := 0; i < 12; i++ {
		started := time.Now()
		done, err := fold.Step(ctx)
		require.NoError(t, err)
		holds = append(holds, time.Since(started))
		if done {
			break
		}
	}
	t.Logf("step holds: %v (µs/row %.1f)", holds, fold.usPerRow)
	late := holds[len(holds)/2:]
	slices.Sort(late)
	limit := 3 * chainFoldStepTarget
	if raceDetectorOn {
		limit = 10 * chainFoldStepTarget
	}
	require.Less(t, late[len(late)/2], limit, "steps are not sized to the target hold")
}

// A step does not start over the fold's own mark (chainFoldWALMark), edited
// or idle; under it the step runs, edited or idle.
func TestChainFoldStepRespectsItsWALMark(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	chain := foldChain(t, store, 200)
	to := reservedGeneration(t, store, "folded")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(ctx) }()
	prev := chainFoldWALMark
	chainFoldWALMark = 1
	t.Cleanup(func() { chainFoldWALMark = prev })
	require.True(t, store.WALWriteMark().Valid && store.WALWriteMark().MxFrame > 0, "precondition: the log holds frames")

	store.walCopy.sawBusy(time.Now()) // being edited
	_, err = fold.Step(ctx)
	require.ErrorIs(t, err, ErrChainFoldWALMark)
	store.walCopy.lastBusy.Store(time.Now().Add(-2 * walCopyEditSession).UnixNano()) // idle
	_, err = fold.Step(ctx)
	require.ErrorIs(t, err, ErrChainFoldWALMark)

	chainFoldWALMark = prev // the log is far under 1 GiB
	store.walCopy.sawBusy(time.Now())
	_, err = fold.Step(ctx)
	require.NoError(t, err)
}

// While a fold runs its members cannot be retired, one fold runs at a time,
// the destination is a payload build in flight (so the orphaned-building
// recovery leaves it), and Abandon hands the partial payload to the sweep.
func TestChainFoldHoldsItsMembersAndOwnsItsDestination(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	chain := foldChain(t, store, 100)
	to := reservedGeneration(t, store, "folded")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
	require.NoError(t, err)
	_, err = store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: reservedGeneration(t, store, "other"), Owner: "test"})
	require.ErrorIs(t, err, ErrChainFoldBusy)
	require.True(t, store.PayloadBuildFlightActive(to))
	for _, member := range chain {
		require.True(t, store.chainFoldHolds(member))
		require.ErrorIs(t, store.RetirePayloadGeneration(ctx, chain[0], nil), ErrPayloadGenerationInUse, "a held member was retired")
	}
	_, err = fold.Step(ctx)
	require.NoError(t, err)
	require.NoError(t, fold.Abandon(ctx))
	row, found, err := store.Catalog().GetViewGeneration(ctx, to)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ViewGenerationFailed, row.State)
	require.False(t, store.PayloadBuildFlightActive(to))
	require.False(t, store.chainFoldHolds(chain[0]))

	// A member that is not ready makes the fold stale.
	building := reservedGeneration(t, store, "building")
	_, err = store.BeginChainFold(ctx, ChainFoldRequest{Chain: []int64{building}, To: reservedGeneration(t, store, "x"), Owner: "test"})
	require.ErrorIs(t, err, ErrChainFoldStale)
}

// RebaseViewGeneration moves a ready layer from one base to another under its
// guards, and the "based" reference moves with it.
func TestRebaseViewGenerationMovesTheBase(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	ready := func(layer string, base int64) int64 {
		id := reservedGeneration(t, store, layer)
		if base > 0 {
			_, err := store.writerDB.Exec(`UPDATE view_generations SET base_generation_id = ? WHERE generation_id = ?`, base, id)
			require.NoError(t, err)
		}
		require.NoError(t, catalog.SetViewGenerationState(ctx, id, ViewGenerationReady, ViewGenerationBuilding))
		return id
	}
	chainTop := ready("chain-top", 0)
	folded := ready("folded", 0)
	layer := ready("layer", chainTop)

	require.ErrorIs(t, catalog.RebaseViewGeneration(ctx, RebaseViewGenerationRequest{GenerationID: layer, FromBase: folded, ToBase: chainTop}),
		ErrCatalogStaleGuard, "a wrong FromBase must miss the guard")
	notReady := reservedGeneration(t, store, "not-ready")
	require.ErrorIs(t, catalog.RebaseViewGeneration(ctx, RebaseViewGenerationRequest{GenerationID: layer, FromBase: chainTop, ToBase: notReady}),
		ErrCatalogStaleGuard, "a base that is not ready")
	require.ErrorIs(t, catalog.RebaseViewGeneration(ctx, RebaseViewGenerationRequest{GenerationID: folded, FromBase: 0, ToBase: layer}),
		ErrCatalogInvalidValue)

	refs, err := catalog.ViewGenerationReferences(ctx, chainTop)
	require.NoError(t, err)
	require.True(t, refs.Based, "precondition: the layer is based on the chain top")
	require.NoError(t, catalog.RebaseViewGeneration(ctx, RebaseViewGenerationRequest{GenerationID: layer, FromBase: chainTop, ToBase: folded}))
	row, _, err := catalog.GetViewGeneration(ctx, layer)
	require.NoError(t, err)
	require.Equal(t, folded, row.BaseGenerationID)
	refs, err = catalog.ViewGenerationReferences(ctx, chainTop)
	require.NoError(t, err)
	require.False(t, refs.Based, "the old base is still referenced")
	refs, err = catalog.ViewGenerationReferences(ctx, folded)
	require.NoError(t, err)
	require.True(t, refs.Based)
	// Again: the guard now misses.
	require.ErrorIs(t, catalog.RebaseViewGeneration(ctx, RebaseViewGenerationRequest{GenerationID: layer, FromBase: chainTop, ToBase: folded}), ErrCatalogStaleGuard)
}

// A step that fails after some of its phases ran puts the cursor back: the
// transaction rolled back, so the next step redoes those phases, and the
// result is still the one-shot flatten's.
func TestChainFoldFailedStepPutsTheCursorBack(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	chain := foldChain(t, store, 1500)
	oneShot := reservedGeneration(t, store, "one-shot")
	_, err := store.FlattenGenerationChain(ctx, chain, oneShot)
	require.NoError(t, err)
	smallFoldSteps(t)
	chainFoldStepTarget = time.Second // many phases per step
	injected := errors.New("injected step failure")
	failures := 0
	chainFoldInjectFailure = func(phase int) error {
		if phase == 3 && failures < 20 {
			failures++
			return injected
		}
		return nil
	}
	t.Cleanup(func() { chainFoldInjectFailure = nil })
	to := reservedGeneration(t, store, "folded")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: chain, To: to, Owner: "test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(ctx) }()
	for step := 0; step < 100000; step++ {
		before := fold.cursor()
		done, err := fold.Step(ctx)
		if errors.Is(err, injected) || errors.Is(err, ErrChainFoldYielded) || errors.Is(err, ErrChainFoldWALMark) {
			require.Equal(t, before, fold.cursor(), "failed or deferred steps cannot advance their cursor")
			if !errors.Is(err, injected) {
				// Retry the API's writer/WAL deferrals just as runFold does.
				// The twenty injected failures and final reference parity
				// remain mandatory; unexpected errors still fail below.
				time.Sleep(time.Millisecond)
			}
			continue
		}
		require.NoError(t, err)
		if done {
			break
		}
	}
	require.NoError(t, fold.Release(ctx))
	require.Equal(t, 20, failures, "precondition: steps failed after some of their phases")
	require.Equal(t, renderGenerationNodes(t, store, oneShot), renderGenerationNodes(t, store, to))
	require.Equal(t, renderGenerationEdges(t, store, oneShot), renderGenerationEdges(t, store, to))
	require.Equal(t, renderFoldFTS(t, store, oneShot), renderFoldFTS(t, store, to))
}
