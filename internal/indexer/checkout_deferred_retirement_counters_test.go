package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A generation is counted owed once, whichever path meets it first —
// discovery, a coordinator's deferral or a refused offer — and once per
// Store.
func TestRetirementOwedIsCountedOncePerGeneration(t *testing.T) {
	// Only the pointers are compared; neither Store is opened.
	first, second := new(store_sqlite.Store), new(store_sqlite.Store)
	key := viewmetrics.GenerationRetireOwedTotal + "{why=" + viewmetrics.OwedDiscovered + "}"
	released := viewmetrics.GenerationRetireOwedTotal + "{why=" + viewmetrics.OwedReleased + "}"
	before := viewmetrics.Read()
	countRetirementOwed(first, 7, viewmetrics.OwedDiscovered)
	countRetirementOwed(first, 7, viewmetrics.OwedReleased)
	countRetirementOwed(first, 7, viewmetrics.OwedDiscovered)
	countRetirementOwed(second, 7, viewmetrics.OwedReleased)
	after := viewmetrics.Read()
	require.EqualValues(t, 1, counterDeltaKey(before, after, key))
	require.EqualValues(t, 1, counterDeltaKey(before, after, released), "another Store's generation 7 is another generation")
}

// A pass the WAL pause holds back says so, once per pause, like a burst; it
// is counted as a stand-down and as a pass stand-down, and the pause ends
// with the next committed burst.
func TestAPassHeldByTheWALPauseLogsThePause(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	l := &CheckoutLifecycle{logger: zap.New(core), walBytes: func() int64 { return deferredRetirementWALPause + 1 }}
	before := viewmetrics.Read()
	passes := deferredRetirementPassStandDowns.Load()
	l.standDownRetirementPass("wal_pause")
	l.standDownRetirementPass("wal_pause")
	require.Len(t, logs.FilterMessage("indexer: deferred retirement paused until the WAL reclaim resets the log").All(), 1)
	require.EqualValues(t, 2, counterDeltaKey(before, viewmetrics.Read(), viewmetrics.RetirementStandDownTotal+"{reason=wal_pause}"))
	require.EqualValues(t, 2, deferredRetirementPassStandDowns.Load()-passes)
}

// Generations the lifecycle is owed with no coordinator left to offer them —
// an idle checkout's layers, a torn-down coordinator's drain, a released ref
// view — are counted owed like a coordinator's deferral, once each, and wake
// an idle retirement worker.
func TestALifecycleOwedGenerationIsCountedAndWakesTheWorker(t *testing.T) {
	// Only the pointer is used; the Store is not opened.
	l := &CheckoutLifecycle{store: new(store_sqlite.Store), owed: map[int64]struct{}{}}
	released := viewmetrics.GenerationRetireOwedTotal + "{why=" + viewmetrics.OwedReleased + "}"
	wake := DeferredRetirementWake()
	before := viewmetrics.Read()
	l.oweRetirement(901, 902, 0)
	select {
	case <-wake:
	default:
		t.Fatal("owing the lifecycle did not wake the worker")
	}
	l.oweRetirement(901)
	require.EqualValues(t, 2, counterDeltaKey(before, viewmetrics.Read(), released))
	require.Equal(t, map[int64]struct{}{901: {}, 902: {}}, l.owed)
}
