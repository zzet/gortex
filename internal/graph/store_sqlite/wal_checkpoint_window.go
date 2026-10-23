package store_sqlite

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Issue #843: the periodic PASSIVE checkpoint ran every attempt under the
// same fixed one-second window. On multi-GiB stores a checkpoint that wins
// the writer gate mid-burst cannot finish folding in one second, so the
// context killed an in-flight PRAGMA, the partial fold was wasted, and the
// retry re-ran the identical lottery — the WAL sat pinned at its
// journal_size_limit cap while every fold landed as one larger, later
// write. The retry cadence (runCheckpointLoopWithAttempt) already handles
// *when* to retry; the policy below fixes *how long* the next attempt gets:
// only an execute-phase deadline grows the window (the PRAGMA demonstrably
// needed more than the current budget), every other outcome restores the
// base. Growth is capped at the retry cadence's maximum so a pathological
// store cannot turn a "best-effort, bounded" checkpoint into an unbounded
// writer-pool residency.

// maxPassiveCheckpointWindow caps GORTEX_WAL_CHECKPOINT_WINDOW_MS so a typo
// cannot pin the sole writer connection to a half-hour PRAGMA window.
const maxPassiveCheckpointWindow = 60 * time.Second

// walCheckpointWindowFromEnv resolves the base passive-checkpoint window
// from GORTEX_WAL_CHECKPOINT_WINDOW_MS (milliseconds), mirroring the
// sqliteMmapBytes idiom: an unset, unparseable, or non-positive value fails
// open to walPassiveCheckpointTimeout, and an absurd value clamps to
// maxPassiveCheckpointWindow. The env var sets the *base* window only; the
// adaptive growth from issue #843 still doubles toward the retry cap on
// execute-phase timeouts. Read once per store open via
// passiveCheckpointWindow, so a test-set storeCore.passiveCheckpointTimeout
// override keeps precedence over the environment.
func walCheckpointWindowFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GORTEX_WAL_CHECKPOINT_WINDOW_MS"))
	if raw == "" {
		return walPassiveCheckpointTimeout
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return walPassiveCheckpointTimeout
	}
	window := time.Duration(ms) * time.Millisecond
	if window > maxPassiveCheckpointWindow {
		return maxPassiveCheckpointWindow
	}
	return window
}

// passiveCheckpointWindowPolicy carries the adaptive execution window for
// the periodic PASSIVE checkpoint. Exactly one goroutine (the checkpoint
// loop) observes outcomes and reads windows, so the struct needs no lock;
// direct storeCore construction in tests leaves the zero value, which
// window() resolves defensively back to the base/default.
type passiveCheckpointWindowPolicy struct {
	base    time.Duration
	max     time.Duration
	current time.Duration
}

// newPassiveCheckpointWindowPolicy clamps its inputs: a non-positive base
// selects walPassiveCheckpointTimeout, and a max below the base is raised to
// the base so growth can never shrink an explicitly configured window.
func newPassiveCheckpointWindowPolicy(base, max time.Duration) passiveCheckpointWindowPolicy {
	if base <= 0 {
		base = walPassiveCheckpointTimeout
	}
	if max < base {
		max = base
	}
	return passiveCheckpointWindowPolicy{base: base, max: max, current: base}
}

// window returns the budget the next attempt should run under.
func (p *passiveCheckpointWindowPolicy) window() time.Duration {
	if p == nil {
		return walPassiveCheckpointTimeout
	}
	if p.current > 0 {
		return p.current
	}
	if p.base > 0 {
		return p.base
	}
	return walPassiveCheckpointTimeout
}

// observeTimeout records an execute-phase deadline: the PRAGMA started but
// did not finish inside the current window, so the next attempt gets double
// the budget, capped at the policy maximum.
func (p *passiveCheckpointWindowPolicy) observeTimeout() {
	if p == nil {
		return
	}
	next := p.window() * 2
	ceiling := p.max
	if ceiling <= 0 {
		ceiling = walCheckpointRetryMax
	}
	if next > ceiling {
		next = ceiling
	}
	p.current = next
}

// observeReset restores the base window after any outcome that does not
// implicate the window itself: a completed checkpoint, an incomplete drain
// with real counters, a writer-gate or bulk-writer deferral (nothing
// executed), or a hard driver failure.
func (p *passiveCheckpointWindowPolicy) observeReset() {
	if p == nil {
		return
	}
	p.current = p.base
}
