package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

type buildLaneBusyStore struct {
	graph.Store
	busy func() bool
}

func (s *buildLaneBusyStore) SetBuildLaneBusy(busy func() bool) { s.busy = busy }

// TestInstallBuildLaneBusyTracksEditCycleHolders: the predicate the store gets
// is true exactly while an edit-driven cycle holds the lane; a background
// holder, or one that declared nothing, leaves it false.
func TestInstallBuildLaneBusyTracksEditCycleHolders(t *testing.T) {
	gate := indexer.NewViewBuildGate()
	gate.Open()
	store := &buildLaneBusyStore{}
	installBuildLaneBusy(store, gate)
	if store.busy == nil {
		t.Fatal("no predicate installed")
	}
	if store.busy() {
		t.Fatal("idle lane reported busy")
	}

	cases := []struct {
		name   string
		holder *indexer.ViewBuildLaneHolder
		busy   bool
	}{
		{"undeclared", nil, false},
		{"background checkout cycle", &indexer.ViewBuildLaneHolder{Kind: "checkout_cycle", Priority: viewmetrics.BuildPriorityBackground}, false},
		{"dirty chain compaction", &indexer.ViewBuildLaneHolder{Kind: "dirty_chain_compaction", Priority: viewmetrics.BuildPriorityBackground}, false},
		{"checkout transition", &indexer.ViewBuildLaneHolder{Kind: "checkout_transition", Priority: viewmetrics.BuildPriorityInteractive}, false},
		{"interactive checkout cycle", &indexer.ViewBuildLaneHolder{Kind: "checkout_cycle", Priority: viewmetrics.BuildPriorityInteractive}, true},
		{"checkout mutation", &indexer.ViewBuildLaneHolder{Kind: "checkout_mutation", Priority: viewmetrics.BuildPriorityInteractive}, true},
	}
	for _, tc := range cases {
		release, err := gate.Acquire(context.Background(), indexer.ViewBuildInteractive)
		if err != nil {
			t.Fatal(err)
		}
		if tc.holder != nil {
			gate.NoteHolder(*tc.holder)
		}
		if got := store.busy(); got != tc.busy {
			t.Errorf("%s: busy=%v, want %v", tc.name, got, tc.busy)
		}
		release()
		if store.busy() {
			t.Fatalf("%s: released lane reported busy", tc.name)
		}
	}
	installBuildLaneBusy(nil, gate) // no store: no panic
}

// TestStatusRendersTheEditCycleYieldCounters: the store's five build-lane
// counters and the WAL ceiling reach the status payload and its text form.
func TestStatusRendersTheEditCycleYieldCounters(t *testing.T) {
	reclaim := walReclaimStatus(store_sqlite.WALReclaimStats{
		ThresholdBytes: 256 << 20, CeilingBytes: 2 << 30,
		CycleDeferrals: 11, CycleRefusals: 7, CycleYields: 5, CycleForced: 2, CycleCeilingRuns: 1,
	})
	if reclaim.CycleDeferrals != 11 || reclaim.CycleRefusals != 7 || reclaim.CycleYields != 5 ||
		reclaim.CycleForced != 2 || reclaim.CycleCeilingRuns != 1 || reclaim.CeilingBytes != 2<<30 {
		t.Fatalf("counters lost on the way to the payload: %+v", reclaim)
	}
	st := countersStatus()
	st.Storage = &daemon.StorageStatus{DBBytes: 1 << 30, WALBytes: 1 << 20, WALReclaim: reclaim}
	var buf bytes.Buffer
	if err := renderDaemonStatusTo(&buf, st, "text"); err != nil {
		t.Fatal(err)
	}
	want := "edit-cycle yield: passive deferrals=11  forced=2  reclaim refusals=7  cut short=5  ceiling runs=1  ceiling=" + formatBytes(2<<30)
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("status text lacks %q:\n%s", want, buf.String())
	}
}
