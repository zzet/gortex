package main

import (
	"runtime"
	"sync"
	"testing"
)

// BenchmarkContentionProfileRates measures what the harness's block and
// mutex profile rates cost the daemon: one operation is a contended mutex
// handoff and a channel handoff between goroutines (the events the two
// profiles sample), with profiling off, with each rate alone, and with both.
// The difference against "off" is the rates' own cost per blocking event.
func BenchmarkContentionProfileRates(b *testing.B) {
	for _, c := range []struct {
		name          string
		blockRate     int
		mutexFraction int
	}{
		{"off", 0, 0},
		{"block_100us", 100000, 0},
		{"mutex_1in10", 0, 10},
		{"both", 100000, 10},
	} {
		b.Run(c.name, func(b *testing.B) {
			runtime.SetBlockProfileRate(c.blockRate)
			prev := runtime.SetMutexProfileFraction(c.mutexFraction)
			defer func() {
				runtime.SetBlockProfileRate(0)
				runtime.SetMutexProfileFraction(prev)
			}()
			var mu sync.Mutex
			ch := make(chan struct{})
			done := make(chan struct{})
			go func() {
				for {
					select {
					case <-ch:
						mu.Lock()
						mu.Unlock() //nolint:staticcheck // an empty critical section is the contended handoff being measured
					case <-done:
						return
					}
				}
			}()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// The helper takes the mutex as soon as it receives: the
				// two goroutines contend on it every operation. Nothing
				// sends while holding it.
				ch <- struct{}{}
				mu.Lock()
				mu.Unlock() //nolint:staticcheck // an empty critical section is the contended handoff being measured
			}
			b.StopTimer()
			close(done)
		})
	}
}
