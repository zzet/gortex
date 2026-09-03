package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func scheduleOnlyCoordinator(checkoutID string, poll, quiet time.Duration, cycleDone func()) *CheckoutCoordinator {
	c := &CheckoutCoordinator{
		checkoutID: checkoutID,
		quiet:      quiet,
		poll:       poll,
		signal:     make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) {
			return CheckoutCycle{}, true
		},
		cycleDone: func(CheckoutCycle) {
			if cycleDone != nil {
				cycleDone()
			}
		},
	}
	go c.run()
	return c
}

func TestInitialCheckoutPollDelayIsStableAndDistributed(t *testing.T) {
	const interval = 15 * time.Second
	if got := initialCheckoutPollDelay("disabled", 0); got != 0 {
		t.Fatalf("disabled phase = %s, want 0", got)
	}
	for _, size := range []int{1, 64, 512} {
		t.Run(fmt.Sprintf("fleet_%d", size), func(t *testing.T) {
			seen := make(map[time.Duration]struct{}, size)
			var min, max time.Duration
			for i := 0; i < size; i++ {
				id := fmt.Sprintf("checkout-%04d", i)
				delay := initialCheckoutPollDelay(id, interval)
				if delay <= 0 || delay > interval {
					t.Fatalf("%s phase = %s, want (0,%s]", id, delay, interval)
				}
				if again := initialCheckoutPollDelay(id, interval); again != delay {
					t.Fatalf("%s phase moved from %s to %s", id, delay, again)
				}
				seen[delay] = struct{}{}
				if i == 0 || delay < min {
					min = delay
				}
				if delay > max {
					max = delay
				}
			}
			if size > 1 {
				if len(seen) < size*9/10 {
					t.Fatalf("only %d/%d initial deadlines are distinct", len(seen), size)
				}
				if min > interval/4 || max < 3*interval/4 {
					t.Fatalf("phases occupy only %s..%s of %s", min, max, interval)
				}
			}
		})
	}
}

func TestCheckoutPollFleetUsesPhasedInitialDeadlinesOnFakeClock(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 10 * time.Millisecond
	)
	for _, size := range []int{1, 64, 512} {
		t.Run(fmt.Sprintf("fleet_%d", size), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				var mu sync.Mutex
				first := make(map[string]time.Duration, size)
				coordinators := make([]*CheckoutCoordinator, 0, size)
				for i := 0; i < size; i++ {
					id := fmt.Sprintf("checkout-%04d", i)
					coordinator := scheduleOnlyCoordinator(id, interval, quiet, func() {
						mu.Lock()
						if _, recorded := first[id]; !recorded {
							first[id] = time.Since(started)
						}
						mu.Unlock()
					})
					coordinators = append(coordinators, coordinator)
				}

				time.Sleep(interval + quiet + time.Nanosecond)
				synctest.Wait()
				for _, coordinator := range coordinators {
					if err := coordinator.Close(); err != nil {
						t.Fatalf("Close: %v", err)
					}
				}

				mu.Lock()
				defer mu.Unlock()
				if len(first) != size {
					t.Fatalf("only %d/%d coordinators reached their first poll", len(first), size)
				}
				for i := 0; i < size; i++ {
					id := fmt.Sprintf("checkout-%04d", i)
					want := initialCheckoutPollDelay(id, interval) + quiet
					if got := first[id]; got != want {
						t.Fatalf("%s first cycle = %s, want phased deadline %s", id, got, want)
					}
					if first[id] > interval+quiet {
						t.Fatalf("%s exceeded the %s poll bound: %s", id, interval, first[id])
					}
				}
			})
		})
	}
}

func TestCheckoutExplicitSignalDoesNotWaitForPollPhase(t *testing.T) {
	const (
		interval = 15 * time.Second
		quiet    = 100 * time.Millisecond
	)
	id := ""
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("signal-checkout-%d", i)
		if initialCheckoutPollDelay(candidate, interval) > 10*time.Second {
			id = candidate
			break
		}
	}

	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		cycleAt := make(chan time.Duration, 1)
		coordinator := scheduleOnlyCoordinator(id, interval, quiet, func() {
			select {
			case cycleAt <- time.Since(started):
			default:
			}
		})
		coordinator.Signal("checkout registered")
		time.Sleep(quiet + time.Nanosecond)
		synctest.Wait()

		select {
		case got := <-cycleAt:
			if got != quiet {
				t.Fatalf("explicit signal cycle = %s, want debounce %s", got, quiet)
			}
			if got >= initialCheckoutPollDelay(id, interval) {
				t.Fatalf("explicit signal waited for poll phase %s", initialCheckoutPollDelay(id, interval))
			}
		default:
			t.Fatal("explicit signal did not run a cycle")
		}
		if err := coordinator.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
