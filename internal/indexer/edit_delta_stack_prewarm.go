package indexer

import (
	"sync"
)

// editDeltaInflight single-flights the whole-repository reads the pre-warm
// and a delta may both start: the second waits for the first's answer.
var editDeltaInflight sync.Map // key -> chan struct{}

// singleFlight runs load unless another caller is running it for key, in
// which case it waits for that one; ready reports whether the answer is
// already kept (then nothing runs).
func singleFlight(key string, ready func() bool, load func()) {
	for {
		if ready() {
			return
		}
		done := make(chan struct{})
		if other, running := editDeltaInflight.LoadOrStore(key, done); running {
			<-other.(chan struct{})
			continue
		}
		func() {
			defer func() {
				editDeltaInflight.Delete(key)
				close(done)
			}()
			if !ready() {
				load()
			}
		}()
		return
	}
}
