package mcp

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitAnalysisIdle(t testing.TB, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.analysisRun.running.Load() {
		if time.Now().After(deadline) {
			t.Fatal("analysis runner did not become idle")
		}
		runtime.Gosched()
	}
}

func TestScheduledAnalysisStartupCohortLaunchesOnce(t *testing.T) {
	for _, notifications := range []int{1, 29, 257} {
		t.Run(fmt.Sprintf("notifications_%d", notifications), func(t *testing.T) {
			s := &Server{}
			var launches atomic.Int64
			s.analysisRunOverride = func() { launches.Add(1) }
			release, _ := s.HoldScheduledAnalysis()

			var wg sync.WaitGroup
			wg.Add(notifications)
			for range notifications {
				go func() {
					defer wg.Done()
					s.ScheduleAnalysis()
				}()
			}
			wg.Wait()
			if got := launches.Load(); got != 0 {
				t.Fatalf("launches while held = %d, want 0", got)
			}

			release()
			waitAnalysisIdle(t, s)
			if got := launches.Load(); got != 1 {
				t.Fatalf("launches after release = %d, want 1", got)
			}
		})
	}
}

func TestScheduledAnalysisNestedHoldsAndIdempotentRelease(t *testing.T) {
	s := &Server{}
	var launches atomic.Int64
	s.analysisRunOverride = func() { launches.Add(1) }
	releaseOuter, cancelOuter := s.HoldScheduledAnalysis()
	releaseInner, cancelInner := s.HoldScheduledAnalysis()
	s.ScheduleAnalysis()

	releaseInner()
	cancelInner()
	if got := launches.Load(); got != 0 {
		t.Fatalf("launches with outer hold = %d, want 0", got)
	}

	var wg sync.WaitGroup
	wg.Add(32)
	for range 32 {
		go func() {
			defer wg.Done()
			releaseOuter()
			cancelOuter()
		}()
	}
	wg.Wait()
	waitAnalysisIdle(t, s)
	if got := launches.Load(); got != 1 {
		t.Fatalf("launches after concurrent release = %d, want 1", got)
	}
}

func TestScheduledAnalysisCancelDoesNotLaunchOrLoseDirtyEpoch(t *testing.T) {
	s := &Server{}
	var launches atomic.Int64
	s.analysisRunOverride = func() { launches.Add(1) }
	release, cancel := s.HoldScheduledAnalysis()
	for range 29 {
		s.ScheduleAnalysis()
	}

	cancel()
	release()
	if got := launches.Load(); got != 0 {
		t.Fatalf("launches after cancellation = %d, want 0", got)
	}
	if !s.analysisSchedulePending() {
		t.Fatal("cancellation discarded the dirty lifecycle epoch")
	}

	// A later runtime notification covers the retained startup work.
	s.ScheduleAnalysis()
	waitAnalysisIdle(t, s)
	if got := launches.Load(); got != 1 {
		t.Fatalf("launches after runtime notification = %d, want 1", got)
	}
}

func TestEnsureAnalysisBypassesStartupHoldAndSatisfiesIt(t *testing.T) {
	s := &Server{}
	var launches atomic.Int64
	s.analysisRunOverride = func() { launches.Add(1) }
	release, _ := s.HoldScheduledAnalysis()
	s.ScheduleAnalysis()

	availability := s.ensureAnalysis()
	if !availability.Running || availability.Ready {
		t.Fatalf("availability = %+v, want running", availability)
	}
	waitAnalysisIdle(t, s)
	if got := launches.Load(); got != 1 {
		t.Fatalf("on-demand launches = %d, want 1", got)
	}

	release()
	waitAnalysisIdle(t, s)
	if got := launches.Load(); got != 1 {
		t.Fatalf("release duplicated on-demand pass: launches = %d", got)
	}
	if s.analysisSchedulePending() {
		t.Fatal("on-demand pass did not satisfy held lifecycle work")
	}
}

func TestScheduledAnalysisAddsOneFollowupAfterSnapshot(t *testing.T) {
	s := &Server{}
	var launches atomic.Int64
	started := make(chan int64, 2)
	resumeFirst := make(chan struct{})
	s.analysisRunOverride = func() {
		n := launches.Add(1)
		started <- n
		if n == 1 {
			<-resumeFirst
		}
	}

	s.ScheduleAnalysis()
	select {
	case n := <-started:
		if n != 1 {
			t.Fatalf("first launch = %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first analysis did not start")
	}

	var wg sync.WaitGroup
	wg.Add(257)
	for range 257 {
		go func() {
			defer wg.Done()
			s.ScheduleAnalysis()
		}()
	}
	wg.Wait()
	close(resumeFirst)

	select {
	case n := <-started:
		if n != 2 {
			t.Fatalf("follow-up launch = %d, want 2", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up analysis did not start")
	}
	waitAnalysisIdle(t, s)
	if got := launches.Load(); got != 2 {
		t.Fatalf("launches = %d, want exactly 2", got)
	}
}

func BenchmarkScheduledAnalysisStartupCohort(b *testing.B) {
	for _, notifications := range []int{1, 29, 257} {
		b.Run(fmt.Sprintf("notifications_%d", notifications), func(b *testing.B) {
			var launches int64
			b.ResetTimer()
			for range b.N {
				s := &Server{}
				s.analysisRunOverride = func() { atomic.AddInt64(&launches, 1) }
				release, _ := s.HoldScheduledAnalysis()
				for range notifications {
					s.ScheduleAnalysis()
				}
				release()
				for s.analysisRun.running.Load() {
					runtime.Gosched()
				}
			}
			b.ReportMetric(float64(atomic.LoadInt64(&launches))/float64(b.N), "launches/startup")
		})
	}
}
