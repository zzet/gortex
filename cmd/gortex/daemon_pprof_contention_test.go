package main

import (
	"testing"

	"go.uber.org/zap"
)

// The block and mutex profiles are set only when their variables name a
// positive value, and exactly to it.
func TestContentionProfilingIsOptIn(t *testing.T) {
	var block, mutex []int
	apply := func(env map[string]string) {
		block, mutex = nil, nil
		applyContentionProfiling(zap.NewNop(), func(k string) string { return env[k] },
			func(r int) { block = append(block, r) },
			func(f int) int { mutex = append(mutex, f); return 0 })
	}
	apply(nil)
	if len(block)+len(mutex) != 0 {
		t.Fatalf("profiling turned on unasked: block %v mutex %v", block, mutex)
	}
	apply(map[string]string{"GORTEX_DAEMON_BLOCK_PROFILE_RATE": "0", "GORTEX_DAEMON_MUTEX_PROFILE_FRACTION": "x"})
	if len(block)+len(mutex) != 0 {
		t.Fatalf("a non-positive or malformed value turned profiling on: block %v mutex %v", block, mutex)
	}
	apply(map[string]string{"GORTEX_DAEMON_BLOCK_PROFILE_RATE": "100000", "GORTEX_DAEMON_MUTEX_PROFILE_FRACTION": "10"})
	if len(block) != 1 || block[0] != 100000 || len(mutex) != 1 || mutex[0] != 10 {
		t.Fatalf("block %v mutex %v; want [100000] and [10]", block, mutex)
	}
}
