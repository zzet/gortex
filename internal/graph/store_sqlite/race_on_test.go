//go:build race

package store_sqlite

import "time"

// raceDetectorOn reports a -race build: timing bounds in the tests scale
// with the detector's slowdown.
const raceDetectorOn = true

// raceSlack scales a wall-clock slack by the detector's slowdown.
func raceSlack(d time.Duration) time.Duration { return 5 * d }
