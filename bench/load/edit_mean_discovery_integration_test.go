package main

import (
	"strings"
	"testing"
)

// Discovery has its own fixed baseline. Adding the edit-and-probe cadence
// option must not silently alter the newly integrated discovery workload.
func TestEditMeanRemainsScopedWhenDiscoveryFlagsArePresent(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", checkoutDiscoveryScenario, "--agents", "2", "--edit-agents", "0", "--worktrees", "0"}
	baseline, err := parseRun(args)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.EditMean != 0 || baseline.Scenario != checkoutDiscoveryScenario {
		t.Fatal("discovery baseline changed")
	}
	for _, mean := range []string{"250ms", "-1ms"} {
		overridden := append(append([]string(nil), args...), "--edit-mean", mean)
		if _, err := parseRun(overridden); err == nil || !strings.Contains(err.Error(), "--edit-mean requires edit-and-probe") {
			t.Fatalf("discovery accepted unrelated edit cadence %s: %v", mean, err)
		}
	}
}
