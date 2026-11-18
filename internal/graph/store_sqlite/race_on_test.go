//go:build race

package store_sqlite

import "time"

// raceDetectorOn reports a -race build: timing bounds in the tests scale
// with the detector's slowdown.
const raceDetectorOn = true

// testTimingSlack allows host scheduling and I/O latency in wall-clock assertions.
// It does not scale the production budgets those assertions check.
func testTimingSlack(d time.Duration) time.Duration { return 5 * d }
