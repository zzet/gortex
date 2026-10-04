//go:build !race

package store_sqlite

import "time"

const raceDetectorOn = false

// raceSlack scales a wall-clock slack by the detector's slowdown.
func raceSlack(d time.Duration) time.Duration { return 1 * d }
