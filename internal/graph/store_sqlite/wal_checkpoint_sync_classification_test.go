package store_sqlite

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These helpers change only test classification;
// they never infer OS I/O duration or a SQLite result from a wall interval.
type reclaimCheckpointSpan struct{ start, end time.Time }

type reclaimCheckpointSyncInterval struct {
	span  reclaimCheckpointSpan
	tls   uintptr
	store *Store
	pacer *walCopyPacer
}

// Call attribution is conservative: the exact same live TLS/pacer/Store was
// observed at both VFS boundaries, and the span must be wholly contained in
// exactly one checkpoint call with no overlap with another recorded call.
// Sorting and merging clipped intervals prevents duplicate or overlapping
// observations from subtracting more than the actual elapsed interval.
func knownPreEditCheckpointSync(store *Store, calls []reclaimCheckpointSpan, callIndex int, syncs []reclaimCheckpointSyncInterval, editStart, end time.Time) time.Duration {
	if callIndex < 0 || callIndex >= len(calls) || !end.After(editStart) {
		return 0
	}
	call := calls[callIndex]
	var qualified []reclaimCheckpointSpan
	for _, p := range syncs {
		if p.store != store || p.pacer == nil || p.pacer.store != store || p.tls == 0 ||
			!p.span.end.After(p.span.start) || !p.span.start.Before(editStart) ||
			p.span.start.Before(call.start) || p.span.end.After(call.end) {
			continue
		}
		ambiguous := false
		for i, other := range calls {
			if i != callIndex && other.start.Before(p.span.end) && other.end.After(p.span.start) {
				ambiguous = true
				break
			}
		}
		if ambiguous {
			continue
		}
		lo, hi := maxTime(editStart, p.span.start), minTime(end, p.span.end)
		if hi.After(lo) {
			qualified = append(qualified, reclaimCheckpointSpan{lo, hi})
		}
	}
	sort.Slice(qualified, func(i, j int) bool { return qualified[i].start.Before(qualified[j].start) })
	var total time.Duration
	var union reclaimCheckpointSpan
	for i, p := range qualified {
		if i == 0 {
			union = p
		} else if !p.start.After(union.end) {
			union.end = maxTime(union.end, p.end)
		} else {
			total += union.end.Sub(union.start)
			union = p
		}
	}
	if len(qualified) != 0 {
		total += union.end.Sub(union.start)
	}
	return min(total, end.Sub(editStart))
}

func TestCheckpointSyncClassificationRejectsUnqualifiedIntervals(t *testing.T) {
	store, other := &Store{}, &Store{}
	pacer := &walCopyPacer{store: store}
	base := time.Now()
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	call := reclaimCheckpointSpan{at(0), at(1000)}
	good := reclaimCheckpointSyncInterval{span: reclaimCheckpointSpan{at(100), at(600)}, tls: 1, store: store, pacer: pacer}
	for _, tc := range []struct {
		name  string
		calls []reclaimCheckpointSpan
		syncs []reclaimCheckpointSyncInterval
		want  time.Duration
	}{
		{"qualified", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{good}, 400 * time.Millisecond},
		{"duplicates_union_once", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{good, good}, 400 * time.Millisecond},
		{"overlap_union_once", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{good, {span: reclaimCheckpointSpan{at(150), at(700)}, tls: 1, store: store, pacer: pacer}}, 500 * time.Millisecond},
		{"wrong_store", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: good.span, tls: 1, store: other, pacer: pacer}}, 0},
		{"unknown_TLS", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: good.span, store: store, pacer: pacer}}, 0},
		{"before_call", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: reclaimCheckpointSpan{at(-1), at(600)}, tls: 1, store: store, pacer: pacer}}, 0},
		{"after_call", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: reclaimCheckpointSpan{at(100), at(1001)}, tls: 1, store: store, pacer: pacer}}, 0},
		{"enters_after_edit", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: reclaimCheckpointSpan{at(201), at(600)}, tls: 1, store: store, pacer: pacer}}, 0},
		{"enters_at_edit", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: reclaimCheckpointSpan{at(200), at(600)}, tls: 1, store: store, pacer: pacer}}, 0},
		{"duplicate_call_ambiguity", []reclaimCheckpointSpan{call, call}, []reclaimCheckpointSyncInterval{good}, 0},
		{"concurrent_unrelated_call", []reclaimCheckpointSpan{call, {at(300), at(400)}}, []reclaimCheckpointSyncInterval{good}, 0},
		{"inverted_interval", []reclaimCheckpointSpan{call}, []reclaimCheckpointSyncInterval{{span: reclaimCheckpointSpan{at(600), at(100)}, tls: 1, store: store, pacer: pacer}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, knownPreEditCheckpointSync(store, tc.calls, 0, tc.syncs, at(200), at(1000)))
		})
	}
	// Even a valid pre-edit sync cannot exempt active work after its return.
	known := knownPreEditCheckpointSync(store, []reclaimCheckpointSpan{call}, 0, []reclaimCheckpointSyncInterval{good}, at(200), at(1000))
	active := at(1000).Sub(at(200)) - known
	bound := walCheckpointCycleYieldPoll + 250*time.Millisecond // existing wider race oracle
	require.Greater(t, active, bound, "post-sync active work must still fail the unchanged oracle")
	require.LessOrEqual(t, knownPreEditCheckpointSync(store, []reclaimCheckpointSpan{call}, 0, []reclaimCheckpointSyncInterval{good, good}, at(200), at(300)), 100*time.Millisecond)
}

func TestCheckpointSyncProbeRejectsWrongTLSAndStore(t *testing.T) {
	store, other := &Store{}, &Store{}
	// Synthetic map keys own no SQLite connection and never execute SQL.
	const matchedTLS, absentTLS, otherTLS = ^uintptr(0), ^uintptr(0) - 1, ^uintptr(0) - 2
	old := reclaimCheckpointSyncProbeState.Load()
	probe := &reclaimCheckpointSyncProbe{store: store}
	pacer := &walCopyPacer{store: store}
	for _, key := range []uintptr{matchedTLS, absentTLS, otherTLS} {
		_, exists := walCopyPacers.Load(key)
		require.False(t, exists, "negative control key must not shadow a real connection")
	}
	walCopyPacers.Store(matchedTLS, pacer)
	walCopyPacers.Store(otherTLS, &walCopyPacer{store: other})
	reclaimCheckpointSyncProbeState.Store(probe)
	t.Cleanup(func() {
		reclaimCheckpointSyncProbeState.Store(old)
		walCopyPacers.Delete(matchedTLS)
		walCopyPacers.Delete(otherTLS)
	})
	got, gotPacer := reclaimCheckpointSyncProbeAt(matchedTLS)
	require.Same(t, probe, got)
	require.Same(t, pacer, gotPacer)
	for _, key := range []uintptr{absentTLS, otherTLS, 0} {
		got, gotPacer = reclaimCheckpointSyncProbeAt(key)
		require.Nil(t, got)
		require.Nil(t, gotPacer)
	}
}

func TestCheckpointSyncClassificationDoesNotExemptActualPageWork(t *testing.T) {
	s, db := finalBackfillFixture(t) // no competing background reclaim
	growWAL(t, s, 4)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	defer lane.held.Store(false)
	editStart := time.Now()
	slack := 60 * time.Millisecond
	if raceDetectorOn {
		slack = 250 * time.Millisecond
	}
	// Only this negative control deliberately dispatches an unpaced direct
	// checkpoint inside a simulated edit; no production policy is changed.
	time.Sleep(slack + time.Millisecond)
	before := vfsIOMark()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	result, err := checkpointWALOnceOn(ctx, db, "PASSIVE")
	ended := time.Now()
	require.NoError(t, err)
	require.Positive(t, result.WALFrames)
	require.Positive(t, vfsIOMark().Since(before).OtherMainWriteBytes, "negative control must perform real checkpoint page writes")
	calls := []reclaimCheckpointSpan{{started, ended}}
	require.Zero(t, knownPreEditCheckpointSync(s, calls, 0, nil, editStart, ended), "unknown unpaced work cannot be exempt")
	require.True(t, started.After(editStart.Add(slack)), "independent started-inside oracle must reject this actual call")
}
