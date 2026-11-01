package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func controlledReadRepairAccounting() ReadRepairObservation {
	origin := time.Now()
	o := ReadRepairObservation{generation: 3, started: origin}
	o.beginPhaseAccounting()
	for kind := patchGateWait; kind <= patchDerived; kind++ {
		start := origin.Add(time.Duration(1+2*int(kind)) * time.Millisecond)
		end := start.Add(time.Millisecond)
		lap := o.collector.beginPatchLapAt(kind, start)
		lap.endAt(end)
		if kind == patchDerived {
			o.collector.markAt(PublicationPatchDerivedStarted, start)
			o.collector.markAt(PublicationPatchDerivedFinished, end)
		}
	}
	o.finished = origin.Add(20 * time.Millisecond)
	o.finishPhaseAccounting()
	return o
}

func TestReadRepairPhaseAccountingUsesOnlyItsOwnOrderedWindow(t *testing.T) {
	o := controlledReadRepairAccounting()
	require.Nil(t, o.collector, "mutable carrier must not reach completion callbacks")
	p := o.PhaseAccounting().Payload()
	require.EqualValues(t, 3, p["owner_generation"])
	require.Equal(t, true, p["observed_partition_complete"])
	require.EqualValues(t, 20e6, p["window_ns"])
	require.EqualValues(t, 20e6, p["observed_subtotal_ns"])
	require.EqualValues(t, 0, p["unattributed_ns"])
	require.Equal(t, false, p["additive_with_requested_background"])
	phases := p["phases_ms"].(map[string]any)
	require.Equal(t, 13.0, phases["repair_control"])
	for _, name := range canonicalPatchLapNames {
		require.Equal(t, 1.0, phases[name], name)
	}
	for _, name := range []string{"debounce", "cohort_admission", "batch_admission", "lane_wait", "attempt_setup", "attempt_finalize"} {
		require.NotContains(t, phases, name, "pre-R or P work cannot be borrowed")
	}
	phases["apply"] = 999
	require.Equal(t, 1.0, o.PhaseAccounting().Payload()["phases_ms"].(map[string]any)["apply"], "projection maps cannot mutate retained evidence")
	copy := o.PhaseAccounting()
	copy.evidence.laps[patchApply] = canonicalLapEvidence{}
	require.Equal(t, true, o.PhaseAccounting().Payload()["observed_partition_complete"])
}

func TestReadRepairPhaseAccountingRefusesMissingContradictoryAndUnboundedLaps(t *testing.T) {
	for _, name := range []string{"missing", "foreign_lane", "outside", "overflow", "duplicate", "unobserved", "gap"} {
		t.Run(name, func(t *testing.T) {
			a := controlledReadRepairAccounting().PhaseAccounting()
			want := "invalid_order"
			switch name {
			case "missing":
				a.evidence.laps[patchExecutorControl].totalNS += a.evidence.laps[patchApply].totalNS
				a.evidence.laps[patchApply] = canonicalLapEvidence{}
				want = "missing_laps"
			case "foreign_lane":
				a.evidence.laps[patchRepositoryLane].begun = 1
			case "outside":
				a.evidence.laps[patchParse].lastNS = a.windowNS + 1
			case "overflow":
				a.evidence.laps[patchExecutorControl].totalNS = 1<<63 - 1
			case "duplicate":
				a.evidence.nativeMtimeDuplicate = true
			case "unobserved":
				a.evidence.observed = false
				want = "not_observed"
			case "gap":
				a.evidence.laps[patchExecutorControl].totalNS--
				want = "unclassified_gap"
			}
			p := a.Payload()
			require.Equal(t, false, p["observed_partition_complete"])
			require.Equal(t, want, p["sum_state"])
		})
	}
}

func TestReadRepairPhaseAccountingRealCoordinatorExecutorAndInertRefusal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	before := "package fixture\nfunc Value() int { return 1 }\n"
	writeFile(t, path, before)
	mi, _ := newSqliteMultiIndexer(t, []config.RepoEntry{{Path: root, Name: "fixture"}})
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.TrackRepo(config.RepoEntry{Path: root, Name: "fixture"})
	require.NoError(t, err)
	idx := mi.GetIndexer("fixture")
	require.NotNil(t, idx)
	after := before + "func Added() int { return Value() }\n"
	writeFile(t, path, after)
	var observation ReadRepairObservation
	observer := &ReadRepairObserver{Session: "reader", RequestIDKey: "n:7", Complete: func(o ReadRepairObservation) { observation = o }}
	_, err = mi.IncrementalReindexRepoObserved("fixture", []string{path}, observer)
	require.NoError(t, err)
	require.True(t, observation.AcceptedFor(idx, readRepairBlobSHA([]byte(after))))
	require.False(t, observation.AcceptedFor(&Indexer{}, readRepairBlobSHA([]byte(after))))
	require.False(t, observation.AcceptedFor(idx, readRepairBlobSHA([]byte("newer bytes"))))
	p := observation.PhaseAccounting().Payload()
	require.Equal(t, true, p["observed_partition_complete"], "%+v", p)
	require.EqualValues(t, observation.finished.Sub(observation.started).Nanoseconds(), p["observed_subtotal_ns"])
	prior := observation
	_, err = mi.IncrementalReindexRepoObserved("fixture", []string{path}, observer)
	require.NoError(t, err)
	require.False(t, observation.AcceptedFor(idx, readRepairBlobSHA([]byte(after))), "same-byte inert validation cannot claim earlier publication")
	require.Equal(t, p, prior.PhaseAccounting().Payload(), "later execution cannot mutate an earlier scalar snapshot")
}
