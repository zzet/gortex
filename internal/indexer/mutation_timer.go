package indexer

import "time"

// armObservedMutationTimer publishes the timer identity and arm observation
// before invoking its callback. Even a zero-delay AfterFunc may otherwise enter
// before it returns. The callback keeps its own actual entry clock reading;
// this handoff delays execution, never rewrites an observed timestamp.
func armObservedMutationTimer(delay time.Duration, callback func(), publish func(*time.Timer)) {
	published := make(chan struct{})
	defer close(published)
	timer := time.AfterFunc(delay, func() {
		<-published
		callback()
	})
	publish(timer)
}
