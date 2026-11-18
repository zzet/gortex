//go:build !race

package excludes

// The matcher differentials run on one goroutine, so the race detector only
// slows them down: race builds check a prefix of each corpus instead.
const raceDetectorOn = false
