//go:build !race

package indexer

import "time"

// Match the Store fold tests: race instrumentation uses fivefold timing slack.
const indexerRaceDetectorOn = false

// testTimingSlack allows host scheduling and I/O latency in wall-clock assertions.
// It does not scale the production budgets those assertions check.
func testTimingSlack(d time.Duration) time.Duration { return 3 * d }
