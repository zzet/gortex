package mcp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// pruneRecorder is an analysis-generation writer whose prune records when it
// ran and, when block is set, runs until its context ends.
type pruneRecorder struct {
	graph.AnalysisGenerationStore
	calls    atomic.Int64
	startedN atomic.Int64 // UnixNano of the last prune start
	block    bool
	ended    chan time.Time
}

func (p *pruneRecorder) PruneAnalysisGenerations(ctx context.Context, _, _ int) error {
	p.calls.Add(1)
	p.startedN.Store(time.Now().UnixNano())
	if p.block {
		<-ctx.Done()
		p.ended <- time.Now()
		return ctx.Err()
	}
	return nil
}

func withEditLane(t *testing.T, busy *atomic.Bool) {
	t.Helper()
	previous, previousWait := editLaneBusyForTest, analysisPruneLaneWait
	editLaneBusyForTest = busy.Load
	t.Cleanup(func() { editLaneBusyForTest, analysisPruneLaneWait = previous, previousWait })
}

// The analysis-generation prune is search-side maintenance: it does not start
// while an edit cycle holds the build lane, skips its round when the lane stays
// busy past its bound, and stops as soon as an edit cycle starts under it.
func TestAnalysisPruneStandsDownForEditCycles(t *testing.T) {
	var busy atomic.Bool
	withEditLane(t, &busy)

	t.Run("waits for the lane", func(t *testing.T) {
		s := &Server{}
		w := &pruneRecorder{}
		analysisPruneLaneWait = 5 * time.Second
		busy.Store(true)
		s.scheduleAnalysisGenerationPrune(w)
		time.Sleep(150 * time.Millisecond)
		if n := w.calls.Load(); n != 0 {
			t.Fatalf("the prune ran %d time(s) while an edit cycle held the lane", n)
		}
		freed := time.Now()
		busy.Store(false)
		s.DrainBackground()
		if w.calls.Load() != 1 {
			t.Fatalf("the prune did not run once the lane was free (%d calls)", w.calls.Load())
		}
		if started := time.Unix(0, w.startedN.Load()); started.Before(freed) {
			t.Fatalf("the prune started at %v, before the lane was free at %v", started, freed)
		}
	})

	t.Run("skips a round when the lane stays busy", func(t *testing.T) {
		s := &Server{}
		w := &pruneRecorder{}
		analysisPruneLaneWait = 100 * time.Millisecond
		busy.Store(true)
		before := analysisPruneStoodDown.Load()
		s.scheduleAnalysisGenerationPrune(w)
		s.DrainBackground()
		busy.Store(false)
		if w.calls.Load() != 0 || analysisPruneStoodDown.Load() != before+1 {
			t.Fatalf("calls %d, stood down %d: want the round skipped", w.calls.Load(), analysisPruneStoodDown.Load()-before)
		}
		if s.analysisPruneScheduled.Load() {
			t.Fatal("a skipped round left the prune marked as scheduled")
		}
	})

	t.Run("stops when an edit cycle starts", func(t *testing.T) {
		s := &Server{}
		w := &pruneRecorder{block: true, ended: make(chan time.Time, 1)}
		analysisPruneLaneWait = 5 * time.Second
		busy.Store(false)
		s.scheduleAnalysisGenerationPrune(w)
		deadline := time.Now().Add(5 * time.Second)
		for w.calls.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if w.calls.Load() == 0 {
			t.Fatal("the prune never started on a free lane")
		}
		began := time.Now()
		busy.Store(true)
		select {
		case ended := <-w.ended:
			if took := ended.Sub(began); took > time.Second {
				t.Fatalf("the prune ran on for %v after an edit cycle started", took)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the prune did not stop when an edit cycle started")
		}
		s.DrainBackground()
		busy.Store(false)
	})
}
