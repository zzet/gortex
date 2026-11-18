package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type promotionTestAcquisition struct {
	done    chan struct{}
	release func()
	err     error
	cancel  context.CancelFunc
}

func queuePromotionTestAcquisition(t *testing.T, g *ViewBuildGate, priority ViewBuildPriority, demand <-chan struct{}) *promotionTestAcquisition {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	acquisition := &promotionTestAcquisition{done: make(chan struct{}), cancel: cancel}
	go func() {
		acquisition.release, acquisition.err = g.AcquirePromotable(ctx, priority, demand)
		close(acquisition.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-acquisition.done:
			if acquisition.release != nil {
				acquisition.release()
			}
		case <-time.After(5 * time.Second):
			t.Error("gate acquisition did not stop after cancellation")
		}
	})
	return acquisition
}

func waitPromotionTestQueued(t *testing.T, g *ViewBuildGate, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats := g.Stats()
		if stats.InteractiveQueued+stats.BackgroundQueued == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue never reached %d: %+v", count, g.Stats())
}

func (a *promotionTestAcquisition) admitted(t *testing.T) func() {
	t.Helper()
	select {
	case <-a.done:
		if a.err != nil || a.release == nil {
			t.Fatalf("acquisition failed: %v", a.err)
		}
		return a.release
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition was not admitted")
		return nil
	}
}

func (a *promotionTestAcquisition) stillQueued(t *testing.T) {
	t.Helper()
	select {
	case <-a.done:
		t.Fatalf("acquisition admitted early: %v", a.err)
	default:
	}
}

func holdPromotionTestGate(t *testing.T, g *ViewBuildGate) func() {
	t.Helper()
	g.Open()
	release, err := g.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return release
}

func assertPromotionTestDrained(t *testing.T, g *ViewBuildGate) {
	t.Helper()
	stats := g.Stats()
	g.mu.Lock()
	promotable := g.promotableQueued
	g.mu.Unlock()
	if stats.Active || stats.InteractiveQueued != 0 || stats.BackgroundQueued != 0 || promotable != 0 {
		t.Fatalf("gate not drained: %+v promotable=%d", stats, promotable)
	}
}

func TestViewBuildGateQueuedDemandPromotesAtGrant(t *testing.T) {
	g := NewViewBuildGate()
	releaseHolder := holdPromotionTestGate(t, g)
	background := queuePromotionTestAcquisition(t, g, ViewBuildBackground, nil)
	waitPromotionTestQueued(t, g, 1)
	demand := make(chan struct{}, 1)
	selected := queuePromotionTestAcquisition(t, g, ViewBuildBackground, demand)
	waitPromotionTestQueued(t, g, 2)
	demand <- struct{}{}
	selected.stillQueued(t)
	background.stillQueued(t)
	if !g.Stats().Active {
		t.Fatal("selection preempted the active build")
	}
	releaseHolder()
	releaseSelected := selected.admitted(t)
	background.stillQueued(t)
	releaseSelected()
	background.admitted(t)()
	assertPromotionTestDrained(t, g)
	stats := g.Stats()
	if stats.AdmittedInteractive != 1 || stats.AdmittedBackground != 2 || stats.WaitSamples != 2 {
		t.Fatalf("promotion double-counted admission or wait: %+v", stats)
	}
}

func TestViewBuildGatePromotionPreservesInteractiveFIFO(t *testing.T) {
	g := NewViewBuildGate()
	releaseHolder := holdPromotionTestGate(t, g)
	first := queuePromotionTestAcquisition(t, g, ViewBuildInteractive, nil)
	waitPromotionTestQueued(t, g, 1)
	demand := make(chan struct{}, 1)
	selected := queuePromotionTestAcquisition(t, g, ViewBuildBackground, demand)
	waitPromotionTestQueued(t, g, 2)
	close(demand)
	releaseHolder()
	releaseFirst := first.admitted(t)
	selected.stillQueued(t)
	releaseFirst()
	selected.admitted(t)()
	assertPromotionTestDrained(t, g)
}

func TestViewBuildGatePromotionsPreserveBackgroundBurstFairness(t *testing.T) {
	g := newViewBuildGateWithLimits(8, 8)
	// The burst mechanics alone: the background waiter counts as starved at
	// once (the age bound is TestViewBuildGateStarvedBackgroundStillRuns).
	g.backgroundStarvation = 0
	releaseHolder := holdPromotionTestGate(t, g)
	background := queuePromotionTestAcquisition(t, g, ViewBuildBackground, nil)
	waitPromotionTestQueued(t, g, 1)
	var selected []*promotionTestAcquisition
	for i := 0; i < maxInteractiveBuildBurst+1; i++ {
		demand := make(chan struct{}, 1)
		selected = append(selected, queuePromotionTestAcquisition(t, g, ViewBuildBackground, demand))
		waitPromotionTestQueued(t, g, i+2)
		demand <- struct{}{}
	}
	releaseHolder()
	for i := 0; i < maxInteractiveBuildBurst; i++ {
		release := selected[i].admitted(t)
		background.stillQueued(t)
		release()
	}
	releaseBackground := background.admitted(t)
	selected[maxInteractiveBuildBurst].stillQueued(t)
	releaseBackground()
	selected[maxInteractiveBuildBurst].admitted(t)()
	assertPromotionTestDrained(t, g)
}

func TestViewBuildGateFullInteractiveQueueRetainsPendingPromotion(t *testing.T) {
	g := newViewBuildGateWithLimits(1, 2)
	releaseHolder := holdPromotionTestGate(t, g)
	first := queuePromotionTestAcquisition(t, g, ViewBuildInteractive, nil)
	waitPromotionTestQueued(t, g, 1)
	demand := make(chan struct{}, 1)
	demand <- struct{}{}
	selected := queuePromotionTestAcquisition(t, g, ViewBuildBackground, demand)
	waitPromotionTestQueued(t, g, 2)
	background := queuePromotionTestAcquisition(t, g, ViewBuildBackground, nil)
	waitPromotionTestQueued(t, g, 3)
	stats := g.Stats()
	if stats.InteractiveQueued != 1 || stats.BackgroundQueued != 2 {
		t.Fatalf("promotion exceeded queue limits: %+v", stats)
	}
	releaseHolder()
	releaseFirst := first.admitted(t)
	selected.stillQueued(t)
	releaseFirst()
	releaseSelected := selected.admitted(t)
	background.stillQueued(t)
	releaseSelected()
	background.admitted(t)()
	assertPromotionTestDrained(t, g)
	stats = g.Stats()
	if stats.InteractiveHighWater > 1 || stats.BackgroundHighWater > 2 ||
		stats.AdmittedInteractive != 2 || stats.WaitSamples != 3 {
		t.Fatalf("pending promotion accounting: %+v", stats)
	}
}

func TestViewBuildGatePromotionOverloadDoesNotConsumeDemand(t *testing.T) {
	g := newViewBuildGateWithLimits(0, 0)
	demand := make(chan struct{}, 1)
	demand <- struct{}{}
	release, err := g.AcquirePromotable(context.Background(), ViewBuildBackground, demand)
	if release != nil || !errors.Is(err, ErrViewBuildQueueFull) || len(demand) != 1 {
		t.Fatalf("overload lost demand: release=%v err=%v buffered=%d", release != nil, err, len(demand))
	}
	g.Open()
	release, err = g.AcquirePromotable(context.Background(), ViewBuildBackground, demand)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(demand) != 0 || g.Stats().AdmittedInteractive != 1 {
		t.Fatalf("retry ignored demand: %+v buffered=%d", g.Stats(), len(demand))
	}
	assertPromotionTestDrained(t, g)
}

func TestViewBuildGateDemandCancellationAndGrantRace(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		g := newViewBuildGateWithLimits(1, 1)
		releaseHolder := holdPromotionTestGate(t, g)
		demand := make(chan struct{}, 1)
		selected := queuePromotionTestAcquisition(t, g, ViewBuildBackground, demand)
		waitPromotionTestQueued(t, g, 1)
		close(demand)
		cancelled := make(chan struct{})
		go func() {
			selected.cancel()
			close(cancelled)
		}()
		releaseHolder()
		<-cancelled
		select {
		case <-selected.done:
			if selected.err != nil && !errors.Is(selected.err, context.Canceled) {
				t.Fatal(selected.err)
			}
			if selected.release != nil {
				selected.release()
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancel/grant race did not finish")
		}
		assertPromotionTestDrained(t, g)
		stats := g.Stats()
		if stats.WaitSamples != 1 || stats.AdmittedInteractive+stats.AdmittedBackground+
			stats.CanceledInteractive+stats.CanceledBackground != 2 {
			t.Fatalf("race accounting: %+v", stats)
		}
	}
}

func TestViewBuildGateCancelledPendingPromotionKeepsActiveHolder(t *testing.T) {
	g := newViewBuildGateWithLimits(0, 1)
	releaseHolder := holdPromotionTestGate(t, g)
	demand := make(chan struct{}, 1)
	close(demand)
	selected := queuePromotionTestAcquisition(t, g, ViewBuildBackground, demand)
	waitPromotionTestQueued(t, g, 1)
	selected.cancel()
	select {
	case <-selected.done:
		if !errors.Is(selected.err, context.Canceled) {
			t.Fatalf("cancel result: %v", selected.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closed demand channel blocked cancellation")
	}
	if !g.Stats().Active {
		t.Fatal("queued cancellation released another build's slot")
	}
	releaseHolder()
	assertPromotionTestDrained(t, g)
}

func BenchmarkViewBuildGatePromotableIdle(b *testing.B) {
	for _, mode := range []string{"ordinary", "undemanded", "prebuffered"} {
		b.Run(mode, func(b *testing.B) {
			g := NewViewBuildGate()
			g.Open()
			ctx := context.Background()
			demand := make(chan struct{}, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var release func()
				var err error
				switch mode {
				case "ordinary":
					release, err = g.Acquire(ctx, ViewBuildBackground)
				case "undemanded":
					release, err = g.AcquirePromotable(ctx, ViewBuildBackground, demand)
				case "prebuffered":
					demand <- struct{}{}
					release, err = g.AcquirePromotable(ctx, ViewBuildBackground, demand)
				}
				if err != nil {
					b.Fatal(err)
				}
				release()
			}
		})
	}
}

func BenchmarkViewBuildGatePendingDemandScan(b *testing.B) {
	for _, count := range []int{1, 128, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			g := newViewBuildGateWithLimits(1, count)
			g.active = true
			demand := make(chan struct{}, 1)
			for i := 0; i < count; i++ {
				g.background = append(g.background, &viewBuildWaiter{demand: demand})
			}
			g.promotableQueued = count
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				g.mu.Lock()
				g.grantNextLocked()
				g.mu.Unlock()
			}
		})
	}
}
