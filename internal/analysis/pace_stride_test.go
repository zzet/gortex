package analysis

import (
	"testing"
	"time"
)

// A slow consumer must not turn the row-count gate into a long unobserved edit.
// Virtual elapsed time removes host scheduling from this check; the existing
// real-analyzer test independently retains its 100 ms wall-time assertion.
func TestPaceChecksSlowIterationsWithinTheResponsivenessBound(t *testing.T) {
	now := time.Unix(1, 0)
	editing := true
	p := NewPace(func() bool { return editing })
	p.clock = func() time.Time { return now }
	p.lastCheck = now // The edit starts immediately after a predicate check.
	var parkedAt time.Time
	p.onPark = func() {
		parkedAt = now
		editing = false
	}
	started := now
	for row := 0; row < 64 && parkedAt.IsZero(); row++ {
		now = now.Add(4 * time.Millisecond)
		p.Tick()
	}
	if parkedAt.IsZero() || parkedAt.Sub(started) > 100*time.Millisecond {
		t.Fatalf("slow iterations parked after %s, want at most 100ms", parkedAt.Sub(started))
	}
}

func TestPaceBusyPredicateStillAllowsProgressBetweenParks(t *testing.T) {
	now := time.Unix(1, 0)
	p := NewPace(func() bool {
		now = now.Add(time.Millisecond)
		return true
	})
	if p.checkInterval != 10*time.Millisecond || p.pollInterval != 5*time.Millisecond || p.parkCap != 30*time.Second {
		t.Fatal("default pacing intervals changed")
	}
	p.clock = func() time.Time { return now }
	// Exercise the same cap/resume logic without spending 30 real seconds.
	p.parkCap, p.pollInterval = 30*time.Millisecond, 0
	rows, previousRows, parks := 0, 0, 0
	p.onPark = func() {
		if parks > 0 && (rows-previousRows < p.checkEvery || now.Sub(p.lastUnpark) < p.checkInterval) {
			t.Fatal("a continuously busy predicate prevented the bounded progress window")
		}
		previousRows = rows
		parks++
	}
	for rows = 1; rows <= 128; rows++ {
		now = now.Add(time.Millisecond)
		p.Tick()
	}
	if parks < 2 {
		t.Fatalf("continuous demand produced %d parks, want repeated cap and resume", parks)
	}
}

func BenchmarkPaceTick(b *testing.B) {
	for _, name := range []string{"nil", "false_predicate"} {
		b.Run(name, func(b *testing.B) {
			var p *Pace
			if name == "false_predicate" {
				p = NewPace(func() bool { return false })
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p.Tick()
			}
		})
	}
}

func BenchmarkComputeHITSOrderedInputPaced(b *testing.B) {
	store := projectionFixture(400)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ComputeHITSPaced(store, NewPace(func() bool { return false }))
	}
}
