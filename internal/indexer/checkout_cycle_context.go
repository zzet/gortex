package indexer

import ()

// Per-cycle context the coordinator hands down its own call tree: the instant
// the cycle started (so every step of one cycle can share one working-copy
// sample taken after it), and the refresh tickets the cycle serves (so the
// builder can mark publication phases on the records those tickets opened).

type cycleStartKey struct{}
