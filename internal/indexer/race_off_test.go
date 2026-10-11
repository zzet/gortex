//go:build !race

package indexer

// Match the Store fold tests: race instrumentation uses fivefold timing slack.
const indexerRaceDetectorOn = false
