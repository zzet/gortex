//go:build performance

package mcp

import "testing"

// Run on a quiet measurement host, like the existing watcher admission bound.
// Default/acceptance CI exercises the same real population and exact oracles,
// while this separate gate requires every own receipt's measured threefold bound.
func TestSharedFileOwnReceiptLatencyWithinThreeIdleTimes(t *testing.T) {
	sharedReceiptComparison(t, 8, true)
}
