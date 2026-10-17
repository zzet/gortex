package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The retirement reclaim-wait counters, the bulk-window overrides and the
// incremental shrink's counters reach the status payload and render on the
// storage block.
func TestStatusRendersTheRetirementWaitAndShrinkCounters(t *testing.T) {
	reclaim := walReclaimStatus(store_sqlite.WALReclaimStats{
		ThresholdBytes: 256 << 20, RetirementWaits: 7, RetirementWaitTimeouts: 2, LeaseOverrides: 4,
		ShrinkInPlaceResets: 1, ShrinkSlices: 150, ShrinkBytes: 3 << 30, ShrinkSliceHoldMax: 260 * time.Millisecond,
	})
	if reclaim.RetirementWaits != 7 || reclaim.RetirementWaitTimeouts != 2 || reclaim.LeaseOverrides != 4 ||
		reclaim.ShrinkInPlaceResets != 1 || reclaim.ShrinkSlices != 150 || reclaim.ShrinkBytes != 3<<30 || reclaim.ShrinkSliceHoldMaxMS != 260 {
		t.Fatalf("counters lost on the way to the payload: %+v", reclaim)
	}
	st := countersStatus()
	st.Storage = &daemon.StorageStatus{DBBytes: 1 << 30, WALBytes: 1 << 20, WALReclaim: reclaim}
	var buf bytes.Buffer
	if err := renderDaemonStatusTo(&buf, st, "text"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"retirement reclaim wait: waits=7  timeouts=2  bulk-window overrides=4",
		"incremental shrink: in-place resets=1  slices=150",
		"slice hold max=260.0ms",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("status text lacks %q:\n%s", want, buf.String())
		}
	}
}

// The exact status checks the row counters against a recount, repairs a
// drift, and renders both; the routine status only says whether they are in
// use.
func TestExactStatusChecksAndRepairsTheRowCounters(t *testing.T) {
	ctx := context.Background()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "counters.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/a.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "repo/a.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{From: "repo/a.go::A", To: "repo/a.go::B", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 3}})
	if err := s.EnsureRowCounters(ctx); err != nil {
		t.Fatal(err)
	}
	if routine := storageStatusFor(s); routine == nil || routine.RowCounters == nil || !routine.RowCounters.Ready || routine.RowCounters.Checked {
		t.Fatalf("routine status row counters = %+v", routine.RowCounters)
	}
	check := checkRowCounters(ctx, s)
	if check == nil || !check.Checked || check.Drifted != 0 || check.Generations != 1 {
		t.Fatalf("exact check = %+v", check)
	}
	st := countersStatus()
	st.Storage = &daemon.StorageStatus{DBBytes: 1 << 20, RowCounters: &daemon.RowCounterStatus{Ready: true, Checked: true,
		Generations: 3, Drifted: 1, Repaired: true, CheckMS: 12, FirstDrift: "generation 0: nodes 9 (counted 2), edges 1 (counted 1)"}}
	var buf bytes.Buffer
	if err := renderDaemonStatusTo(&buf, st, "text"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"row counters: ready=true  checked generations=3  drifted=1  repaired=true",
		"first drift: generation 0: nodes 9 (counted 2)",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("status text lacks %q:\n%s", want, buf.String())
		}
	}
}
