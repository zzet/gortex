package indexer

import (
	"context"
	"testing"
	"time"
)

// A background cycle that fails is no evidence the working tree settled: it
// keeps the motion backoff the moving tree earned, where a cycle that ran to
// a good end resets it (TestAbandonedBackgroundBuildsBackOffTheirAdmission).
func TestAFailedBackgroundCycleKeepsTheMotionBackoff(t *testing.T) {
	root, sampler := motionRepo(t)
	gate := NewViewBuildGate()
	gate.Open()
	var baseQuiet, baseCap time.Duration
	c, outcomes := startMotionCycle(t, gate, root, sampler, func(c *CheckoutCoordinator) {
		baseQuiet, baseCap = c.backgroundAdmissionBounds()
		// Two background builds in a row were abandoned by the moving tree.
		c.motion.mu.Lock()
		c.motion.movedAborts = 2
		c.motion.mu.Unlock()
	}, func(context.Context) {})
	// The cycle is admitted, runs to its end and fails: the motion
	// fixture's catalog knows no such checkout.
	out := awaitMotionOutcome(t, outcomes)
	if out.Err == nil || out.Held || out.Rescheduled {
		t.Fatalf("cycle = %+v, want a failed background cycle", out)
	}
	if stats := gate.Stats(); stats.AdmittedBackground != 1 {
		t.Fatalf("background admissions = %d, want the failed cycle's", stats.AdmittedBackground)
	}
	c.motion.mu.Lock()
	moved := c.motion.movedAborts
	c.motion.mu.Unlock()
	if moved != 2 {
		t.Fatalf("a failed background cycle reset the abandoned-build run to %d, want 2", moved)
	}
	if quiet, capped := c.backgroundAdmissionBounds(); quiet != baseQuiet<<2 || capped != baseCap<<2 {
		t.Fatalf("after a failed cycle: quiet %s cap %s, want the backoff %s and %s", quiet, capped, baseQuiet<<2, baseCap<<2)
	}
}
