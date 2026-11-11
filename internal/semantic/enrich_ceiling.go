package semantic

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The outer ceiling the Manager places on a ContextEnricher's context is a
// backstop for a provider wedged in an uncancellable call. A pass that queues
// on the graph-wide resolve mutex is in exactly such a call, but the wait is
// another pass's work: the provider's own lazy deadline already excludes it,
// while a fixed ceiling kept counting. A pass queued behind a long apply
// therefore reached the ceiling while it waited and was cut as soon as it got
// the mutex, landing nothing — and because its marker stays unwritten, the
// same queueing repeats on the next start.
//
// A provider that implements QueueWaitExcludingEnricher gets a ceiling it can
// pause: ExcludeQueueWait stops the clock for the duration of an uncancellable
// wait and restarts it with the time that was left. Pausing only an
// uncancellable wait never weakens the backstop: cancelling the context could
// not end that wait anyway, it could only doom the pass that follows it.

// errEnrichCeiling is the cancellation cause recorded when a pausable outer
// ceiling expires. It wraps context.DeadlineExceeded so errors.Is callers
// still classify it as a deadline; a provider that records
// context.Cause of its context reports it as the reason the pass was cut.
var errEnrichCeiling = fmt.Errorf("semantic enrichment outer ceiling reached: %w", context.DeadlineExceeded)

type enrichCeilingKey struct{}

// enrichCeiling cancels its context once d of unpaused time has elapsed.
type enrichCeiling struct {
	cancel context.CancelCauseFunc
	// onLongWait, if set, is called once per pause that outlives the time the
	// ceiling had left, with how long the wait has lasted. A wedged lock
	// holder would otherwise leave the queued pass "running" with no signal.
	onLongWait func(queued time.Duration)

	mu       sync.Mutex
	timer    *time.Timer
	waitWarn *time.Timer   // while paused: the onLongWait alarm
	deadline time.Time     // while running: when the ceiling expires
	left     time.Duration // while paused: unpaused time still available
	paused   int
	fired    bool
	stopped  bool
}

// newEnrichCeiling returns a context cancelled with errEnrichCeiling after d
// of unpaused time, or when parent ends. stop releases the timers and the
// context; it must be called once the pass is over. onLongWait may be nil.
func newEnrichCeiling(parent context.Context, d time.Duration, onLongWait func(time.Duration)) (context.Context, *enrichCeiling) {
	ctx, cancel := context.WithCancelCause(parent)
	c := &enrichCeiling{cancel: cancel, onLongWait: onLongWait, deadline: time.Now().Add(d)}
	c.timer = time.AfterFunc(d, c.fire)
	return context.WithValue(ctx, enrichCeilingKey{}, c), c
}

func (c *enrichCeiling) fire() {
	c.mu.Lock()
	if c.paused > 0 || c.fired || c.stopped {
		c.mu.Unlock()
		return
	}
	// A pause and resume can race an already-started callback; honour the
	// deadline the resume set rather than the one this callback was armed for.
	if wait := time.Until(c.deadline); wait > 0 {
		c.timer.Reset(wait)
		c.mu.Unlock()
		return
	}
	c.fired = true
	c.mu.Unlock()
	c.cancel(errEnrichCeiling)
}

func (c *enrichCeiling) pause() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fired || c.stopped {
		return func() {}
	}
	if c.paused == 0 {
		c.timer.Stop()
		c.left = time.Until(c.deadline)
		if c.onLongWait != nil {
			since := time.Now()
			var warn *time.Timer
			warn = time.AfterFunc(max(c.left, 0), func() {
				// Stop cannot recall a callback that has already started, so
				// announce only while this pause is still the one queued.
				c.mu.Lock()
				current := c.paused > 0 && c.waitWarn == warn
				c.mu.Unlock()
				if current {
					c.onLongWait(time.Since(since))
				}
			})
			c.waitWarn = warn
		}
	}
	c.paused++
	var once sync.Once
	return func() { once.Do(c.resume) }
}

func (c *enrichCeiling) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused--
	if c.paused > 0 {
		return
	}
	if c.waitWarn != nil {
		c.waitWarn.Stop()
		c.waitWarn = nil
	}
	if c.fired || c.stopped {
		return
	}
	left := max(c.left, 0)
	c.deadline = time.Now().Add(left)
	c.timer.Reset(left)
}

func (c *enrichCeiling) stop() {
	c.mu.Lock()
	c.stopped = true
	c.timer.Stop()
	if c.waitWarn != nil {
		c.waitWarn.Stop()
		c.waitWarn = nil
	}
	c.mu.Unlock()
	c.cancel(context.Canceled)
}

// ExcludeQueueWait pauses the Manager's outer ceiling on ctx until the
// returned function is called. Bracket only a wait that cancellation cannot
// end — such as acquiring the graph-wide resolve mutex — and call the returned
// function as soon as it ends; calling it more than once is harmless. On a
// context without a pausable ceiling it does nothing.
func ExcludeQueueWait(ctx context.Context) (resume func()) {
	if ctx != nil {
		if c, ok := ctx.Value(enrichCeilingKey{}).(*enrichCeiling); ok {
			return c.pause()
		}
	}
	return func() {}
}
