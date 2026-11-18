//go:build !race

package store_sqlite

import "time"

const raceDetectorOn = false

// testTimingSlack allows host scheduling and I/O latency in wall-clock assertions.
// It does not scale the production budgets those assertions check.
func testTimingSlack(d time.Duration) time.Duration { return 3 * d }
