package reach

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLookupCoordinationWaitKeepsGateAndMutexSeparate(t *testing.T) {
	for _, held := range []string{"gate_wait", "resolve_mutex_wait"} {
		t.Run(held, func(t *testing.T) {
			g, ids := newCallChain(t, 3)
			var release func()
			if held == "gate_wait" {
				finish := BeginTopologyMutation(g)
				release = func() { finish(false) }
			} else {
				mu := g.ResolveMutex()
				mu.Lock()
				release = mu.Unlock
			}
			var once sync.Once
			unblock := func() { once.Do(release) }
			t.Cleanup(unblock)
			observed := make(chan CoordinationWait, 8)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			ctx = WithCoordinationWaitObserver(ctx, func(wait CoordinationWait) { observed <- wait })
			done := make(chan struct{})
			go func() { LookupContext(ctx, g, ids[2]); close(done) }()
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			var events []CoordinationWait
			waiting := true
			for waiting {
				select {
				case wait := <-observed:
					events = append(events, wait)
					if wait.Phase == held {
						t.Fatalf("%s reported acquired before its held lock released: %+v", held, wait)
					}
				case <-timer.C:
					waiting = false
				}
			}
			unblock()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("lookup did not complete after its lock released")
			}
			for len(observed) > 0 {
				events = append(events, <-observed)
			}
			found := false
			for _, wait := range events {
				if wait.Phase == held {
					found = true
					if !wait.Attempted || !wait.Acquired || wait.Duration <= 0 {
						t.Fatalf("held wait was not actually measured: %+v", wait)
					}
				}
			}
			if !found {
				t.Fatalf("missing %s observation: %+v", held, events)
			}
		})
	}
}

func TestLookupCoordinationWaitReportsCancelledGlobalGate(t *testing.T) {
	g, ids := newCallChain(t, 2)
	finish := BeginTopologyMutation(g)
	defer finish(false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var events []CoordinationWait
	ctx = WithCoordinationWaitObserver(ctx, func(wait CoordinationWait) { events = append(events, wait) })
	_, _, _, _, truncated := LookupContext(ctx, g, ids[1])
	if !truncated {
		t.Fatal("cancelled gate admission must retain its conservative result")
	}
	if len(events) != 1 || events[0].Phase != "gate_wait" || !events[0].Attempted || events[0].Acquired {
		t.Fatalf("cancelled global gate claimed successful or mutex admission: %+v", events)
	}
}
