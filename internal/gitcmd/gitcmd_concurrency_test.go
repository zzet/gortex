package gitcmd

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The limiter's weight follows the host's CPUs, not the daemon's GOMAXPROCS:
// at GOMAXPROCS=1 a second git child still starts while one is running.
func TestGitLimiterAtGOMAXPROCSOneAdmitsTwoConcurrentChildren(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("a single-CPU host has a weight of one by design")
	}
	t.Setenv(concurrencyEnv, "")
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() {
		runtime.GOMAXPROCS(previous)
		SetConcurrency(int(defaultConcurrency()))
	})
	SetConcurrency(int(defaultConcurrency()))
	if got := defaultConcurrency(); got < 2 {
		t.Fatalf("weight at GOMAXPROCS=1 on %d CPUs = %d, want at least 2", runtime.NumCPU(), got)
	}

	// Hold one slot the way a running git child does, then run another.
	held := currentSem()
	if err := held.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	defer held.Release(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Run(ctx, t.TempDir(), "--version"); err != nil {
		t.Fatalf("a second git child did not start while one slot was held: %v", err)
	}
}

// GORTEX_GIT_CONCURRENCY overrides the weight; a malformed value is ignored.
func TestGitLimiterWeightIsEnvOverridable(t *testing.T) {
	t.Setenv(concurrencyEnv, "3")
	if got := defaultConcurrency(); got != 3 {
		t.Fatalf("weight with %s=3 is %d", concurrencyEnv, got)
	}
	t.Setenv(concurrencyEnv, "zero")
	if got := defaultConcurrency(); got < 1 || got > maxDefaultConcurrency {
		t.Fatalf("a malformed override gave weight %d", got)
	}
}
