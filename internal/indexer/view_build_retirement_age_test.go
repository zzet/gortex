package indexer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

type retirementAgeAcquisition struct {
	ready  chan func()
	done   chan error
	cancel context.CancelFunc
}

// Every test acquisition owns cancellation and a join, including intended-red
// assertions. No assertion holds the gate mutex.
func startRetirementAgeAcquisition(t *testing.T, g *ViewBuildGate, priority ViewBuildPriority, credit *time.Duration) *retirementAgeAcquisition {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := &retirementAgeAcquisition{ready: make(chan func(), 1), done: make(chan error, 1), cancel: cancel}
	go func() {
		var r func()
		var err error
		if credit == nil {
			r, err = g.Acquire(ctx, priority)
		} else {
			r, err = g.acquireRetirement(ctx, *credit)
		}
		if err == nil {
			a.ready <- r
		}
		a.done <- err
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-a.done; err == nil {
			(<-a.ready)()
		}
	})
	return a
}

func (a *retirementAgeAcquisition) release(t *testing.T) {
	t.Helper()
	select {
	case r := <-a.ready:
		r()
		// Cleanup still joins the acquisition but must not release twice.
		a.ready <- func() {}
	default:
		t.Fatal("expected admitted acquisition")
	}
}

func TestRetirementAdmissionCarriesAgeAcrossRepeatedTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewViewBuildGate()
		g.Open()
		holder := startRetirementAgeAcquisition(t, g, ViewBuildInteractive, nil)
		synctest.Wait()
		credit := viewBuildBackgroundStarvation
		for turn := 0; turn < 2; turn++ {
			debt := startRetirementAgeAcquisition(t, g, ViewBuildBackground, &credit)
			foreground := make([]*retirementAgeAcquisition, maxInteractiveBuildBurst+1)
			for i := range foreground {
				foreground[i] = startRetirementAgeAcquisition(t, g, ViewBuildInteractive, nil)
				synctest.Wait()
			}
			// Start a fresh foreground burst before releasing the actual holder.
			g.mu.Lock()
			g.interactiveBurst = 0
			g.mu.Unlock()
			holder.release(t)
			synctest.Wait()
			for i := 0; i < maxInteractiveBuildBurst; i++ {
				select {
				case r := <-debt.ready:
					debt.ready <- r
					t.Fatal("debt bypassed foreground burst")
				default:
				}
				foreground[i].release(t)
				synctest.Wait()
			}
			debt.release(t)
			synctest.Wait()
			holder = foreground[maxInteractiveBuildBurst]
		}
		holder.release(t)
	})
}

func TestRetirementAdmissionClampAndOrdinaryBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, credit := range []time.Duration{-time.Hour, 0, time.Second, time.Hour} {
			g := NewViewBuildGate()
			g.Open()
			holder := startRetirementAgeAcquisition(t, g, ViewBuildInteractive, nil)
			synctest.Wait()
			debt := startRetirementAgeAcquisition(t, g, ViewBuildBackground, &credit)
			synctest.Wait()
			g.mu.Lock()
			actual := g.background[0].backgroundAgeCredit
			enqueued := g.background[0].enqueuedAt
			g.mu.Unlock()
			want := max(time.Duration(0), min(credit, g.backgroundStarvation))
			if actual != want || time.Since(enqueued) != 0 {
				t.Fatalf("credit=%s enqueue age=%s; want %s and zero", actual, time.Since(enqueued), want)
			}
			debt.cancel()
			synctest.Wait()
			holder.release(t)
		}
		g := NewViewBuildGate()
		g.Open()
		holder := startRetirementAgeAcquisition(t, g, ViewBuildInteractive, nil)
		synctest.Wait()
		ordinary := startRetirementAgeAcquisition(t, g, ViewBuildBackground, nil)
		foreground := startRetirementAgeAcquisition(t, g, ViewBuildInteractive, nil)
		synctest.Wait()
		g.mu.Lock()
		g.interactiveBurst = maxInteractiveBuildBurst
		g.mu.Unlock()
		holder.release(t)
		synctest.Wait()
		select {
		case r := <-ordinary.ready:
			ordinary.ready <- r
			t.Fatal("ordinary background skipped fresh aging")
		default:
		}
		foreground.release(t)
		synctest.Wait()
		ordinary.release(t)
	})
}

func TestRetirementAdmissionCancellationAndRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := NewViewBuildGate()
		g.Open()
		holder := startRetirementAgeAcquisition(t, g, ViewBuildInteractive, nil)
		synctest.Wait()
		credit := time.Hour
		debt := startRetirementAgeAcquisition(t, g, ViewBuildBackground, &credit)
		synctest.Wait()
		debt.cancel()
		synctest.Wait()
		if g.Stats().BackgroundQueued != 0 {
			t.Fatal("canceled waiter remains queued")
		}
		// Force the supported queue limit refusal without adding fake waiters.
		g.mu.Lock()
		g.backgroundLimit = 0
		g.mu.Unlock()
		_, err := g.acquireRetirement(context.Background(), credit)
		if !errors.Is(err, ErrViewBuildQueueFull) {
			t.Fatalf("queue refusal=%v", err)
		}
		holder.release(t)
		admitted := startRetirementAgeAcquisition(t, g, ViewBuildBackground, &credit)
		synctest.Wait()
		admitted.cancel()
		admitted.release(t)
		synctest.Wait()
		if g.Stats().Active {
			t.Fatal("cancellation lost an already-granted permit")
		}
	})
}
