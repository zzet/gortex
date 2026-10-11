package indexer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// seedRetirementPayload leaves one failed generation carrying nodes rows and
// the payload size its publication would have recorded.
func seedRetirementPayload(t *testing.T, store *store_sqlite.Store, template store_sqlite.PayloadGenerationRequest, layer string, nodes int, bytes int64) int64 {
	t.Helper()
	ctx := context.Background()
	request := template
	request.LayerID, request.ConfigHash = layer, layer
	request.CreatedAt = time.Now().Add(-time.Hour).Unix()
	id, handle, err := store.BeginPayloadGeneration(ctx, request)
	require.NoError(t, err)
	batch := make([]*graph.Node, 0, nodes)
	for i := 0; i < nodes; i++ {
		file := fmt.Sprintf("%s/%s.go", builderRepoPrefix, layer)
		batch = append(batch, &graph.Node{ID: fmt.Sprintf("%s::F%d", file, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("F%d", i), FilePath: file, RepoPrefix: builderRepoPrefix})
	}
	require.NoError(t, handle.AddBatchChecked(batch, nil))
	require.NoError(t, store.Catalog().UpdateViewGenerationRollup(ctx, id, 1, 1, bytes))
	require.NoError(t, store.Catalog().SetViewGenerationState(ctx, id, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding))
	return id
}

func retiringGenerationCount(t *testing.T, store *store_sqlite.Store) int {
	t.Helper()
	rows, err := store.Catalog().ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{
		States: []store_sqlite.ViewGenerationState{store_sqlite.ViewGenerationRetiring},
	})
	require.NoError(t, err)
	return len(rows)
}

// Several small generations that each need two bursts and one large newer
// one: a burst finishes the generation it started before fencing another, so
// the first catalog removal comes within two bursts and at most two
// generations are ever fenced at once. Rotating every burst fenced them all
// and removed the first only after a whole lap.
func TestRetirementBurstsFinishAFencedGenerationBeforeRotating(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	store := fixture.store
	template := payloadRequestForBuild(fixture.request)
	l := newGenerationRetirementLifecycle(store, time.Now())
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	// The quantum wrapper below bounds each burst; nothing else waits.
	l.foregroundWork = func() (string, time.Time) { return "", time.Time{} }
	l.interactiveDemand = func() bool { return false }
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	const smallNodes = 40
	// Calibrate: the quanta one small generation takes, retired directly.
	calibration := seedRetirementPayload(t, store, template, "calibration", smallNodes, 4<<10)
	quantaPerSmall := 0
	for ; quantaPerSmall < 512; quantaPerSmall++ {
		progress, _ := store.RetirePayloadGenerationQuantum(t.Context(), calibration, nil)
		if progress.CatalogRemoved {
			quantaPerSmall++
			break
		}
	}
	require.GreaterOrEqual(t, quantaPerSmall, 3)
	for i := 0; i < 6; i++ {
		l.owed[seedRetirementPayload(t, store, template, fmt.Sprintf("small-%d", i), smallNodes, 4<<10)] = struct{}{}
	}
	large := seedRetirementPayload(t, store, template, "large", 2000, 64<<20)
	l.owed[large] = struct{}{}

	// A bounded number of quanta per burst, then a waiting writer ends it:
	// each small generation takes exactly two bursts.
	quantaPerBurst := (quantaPerSmall + 1) / 2
	served := 0
	removedAt := 0
	burst := 0
	l.retireQuantum = func(ctx context.Context, id int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		if served == quantaPerBurst {
			return store_sqlite.PayloadRetirementProgress{}, store_sqlite.ErrPayloadRetirementWriteWanted
		}
		served++
		// Ordinary 16-row quanta, as calibrated: a fresh context with the
		// burst's deadline carries no wide-quantum mark.
		deadline, _ := ctx.Deadline()
		narrow, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		progress, err := l.store.RetirePayloadGenerationQuantum(narrow, id, inUse)
		if progress.CatalogRemoved && removedAt == 0 {
			removedAt = burst
		}
		return progress, err
	}
	maxRetiring := 0
	for burst = 1; burst <= 12 && removedAt == 0; burst++ {
		served = 0
		_, _, err := l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
		maxRetiring = max(maxRetiring, retiringGenerationCount(t, store))
	}
	require.NotZero(t, removedAt, "no generation was removed in twelve bursts")
	require.LessOrEqual(t, removedAt, 2, "the first removal took %d bursts", removedAt)
	require.LessOrEqual(t, maxRetiring, 2, "%d generations were fenced at once", maxRetiring)
	requireCatalogGenerationPresent(t, store, large)
}

// The generation a burst left fenced keeps priority only for its cap; after
// it, one choice goes to the cursor, and every cursor turn does as well.
func TestRetirementActivePriorityIsCappedAndTheCursorGetsTurns(t *testing.T) {
	l := &CheckoutLifecycle{}
	now := time.Now()
	ordered := []int64{9, 7, 5, 3}
	shapes := map[int64]store_sqlite.RetirementCandidateShape{
		9: {State: store_sqlite.ViewGenerationFailed},
		7: {State: store_sqlite.ViewGenerationFailed},
		5: {State: store_sqlite.ViewGenerationRetiring, StorageBytes: 1 << 30},
		3: {State: store_sqlite.ViewGenerationFailed, HasChildren: true},
	}
	// A fenced generation is chosen over smaller leaves.
	require.EqualValues(t, 5, l.pickDeferredRetirement(ordered, shapes, now))
	l.noteDeferredRetirementServed(5, true, false, now)
	require.EqualValues(t, 5, l.pickDeferredRetirement(ordered, shapes, now.Add(time.Second)))
	// Past the cap the cursor chooses, and never the capped generation.
	got := l.pickDeferredRetirement(ordered, shapes, now.Add(deferredRetirementActiveCap))
	require.NotEqualValues(t, 5, got)
	// Without fenced work, leaves go smallest first and a based parent last.
	delete(shapes, 5)
	shapes[9] = store_sqlite.RetirementCandidateShape{State: store_sqlite.ViewGenerationFailed, StorageBytes: 2 << 20}
	shapes[7] = store_sqlite.RetirementCandidateShape{State: store_sqlite.ViewGenerationFailed, StorageBytes: 1 << 10}
	l.deferredRetirementPicks = 0
	require.EqualValues(t, 7, l.pickDeferredRetirement([]int64{9, 7, 3}, shapes, now))
	// Every cursor turn goes to the cursor whatever the shapes say: from a
	// reset cursor its first turn is the newest candidate.
	l.deferredRetirementCursor = 0
	cursorTurns := 0
	for i := 0; i < 2*deferredRetirementCursorTurn; i++ {
		if l.pickDeferredRetirement([]int64{9, 7, 3}, shapes, now) != 7 {
			cursorTurns++
		}
	}
	require.GreaterOrEqual(t, cursorTurns, 1)
}

// A generation the catalog refuses as still referenced is parked and the burst
// moves on, whatever the refused quantum committed: a fenced continuation on a
// swept generation commits an empty quantum before the final delete refuses
// it, and staying on it repeated that refusal for the rest of the burst.
func TestABurstRotatesOffAGenerationRefusedAfterItCommitted(t *testing.T) {
	fixture := newSparseBuildFlightFixture(t)
	store := fixture.store
	template := payloadRequestForBuild(fixture.request)
	l := newGenerationRetirementLifecycle(store, time.Now())
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.foregroundWork = func() (string, time.Time) { return "", time.Time{} }
	l.interactiveDemand = func() bool { return false }
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	other := seedRetirementPayload(t, store, template, "other", 40, 4<<10)
	// Newer, so the burst starts on it.
	referenced := seedRetirementPayload(t, store, template, "referenced", 40, 4<<10)
	l.owed[other], l.owed[referenced] = struct{}{}, struct{}{}
	attempts := map[int64]int{}
	l.retireQuantum = func(ctx context.Context, id int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		attempts[id]++
		if id == referenced {
			// The final delete's refusal names a layer built on it, as the
			// Store's does; a park on that waits for a release naming it, so
			// the other generation's removal below does not lift it.
			return store_sqlite.PayloadRetirementProgress{ChunksCommitted: 1},
				&store_sqlite.GenerationReferencedError{GenerationID: id, Refs: store_sqlite.ViewGenerationReferences{Based: true}}
		}
		return store.RetirePayloadGenerationQuantum(ctx, id, inUse)
	}
	_, _, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, attempts[referenced], "the burst stayed on a generation it had parked")
	require.Positive(t, attempts[other], "the burst never reached the other generation")
	require.True(t, l.retirementParkedNow(referenced, time.Now()))
}
