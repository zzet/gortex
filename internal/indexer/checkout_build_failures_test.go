package indexer

import (
	"fmt"
	"sync"
	"testing"
)

func TestCheckoutBuildFailuresAreGenerationFenced(t *testing.T) {
	failures := newCheckoutBuildFailures()
	failures.start("checkout", 10)
	failures.record("checkout", 10, "first")
	if reason, ok := failures.failure("checkout", 10); !ok || reason != "first" {
		t.Fatalf("failure = (%q, %t), want first", reason, ok)
	}

	// Retrying the same generation clears the terminal bit while the adopted
	// partial generation is re-derived.
	failures.start("checkout", 10)
	if reason, ok := failures.failure("checkout", 10); ok || reason != "" {
		t.Fatalf("same-generation retry retained failure (%q, %t)", reason, ok)
	}
	failures.record("checkout", 10, "again")
	failures.start("checkout", 11)
	failures.record("checkout", 10, "stale completion")
	failures.clearThrough("checkout", 10)
	if reason, ok := failures.failure("checkout", 11); ok || reason != "" {
		t.Fatalf("older completion affected generation 11: (%q, %t)", reason, ok)
	}
	failures.record("checkout", 11, "newest")
	if reason, ok := failures.failure("checkout", 11); !ok || reason != "newest" {
		t.Fatalf("newest failure = (%q, %t)", reason, ok)
	}
	failures.clearThrough("checkout", 11)
	if _, ok := failures.failure("checkout", 11); ok {
		t.Fatal("clearThrough did not clear the matching generation")
	}
}

func TestCheckoutBuildFailuresConcurrentAccess(t *testing.T) {
	failures := newCheckoutBuildFailures()
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for generation := int64(1); generation <= 200; generation++ {
				failures.start("checkout", generation)
				failures.record("checkout", generation, fmt.Sprintf("w%d", worker))
				_, _ = failures.failure("checkout", generation)
				if generation%3 == 0 {
					failures.clearThrough("checkout", generation-1)
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestCheckoutBuildFailuresKeepBaseRefreshIndependentFromReplacementGeneration(t *testing.T) {
	failures := newCheckoutBuildFailures()
	failures.startBaseRefresh("graph", 10)

	// The replacement build is newer and uses the same owner checkout, but it
	// is not the identity TrackReadiness represents while the old base remains
	// active as fallback.
	failures.start("owner", 11)
	failures.record("owner", 11, "replacement")
	failures.recordBaseRefresh("graph", 10, "refresh")

	if reason, ok := failures.baseRefreshFailure("graph", 10); !ok || reason != "refresh" {
		t.Fatalf("base refresh failure = (%q, %t), want refresh", reason, ok)
	}
	if reason, ok := failures.failure("owner", 11); !ok || reason != "replacement" {
		t.Fatalf("replacement failure = (%q, %t), want replacement", reason, ok)
	}

	failures.startBaseRefresh("graph", 10)
	if reason, ok := failures.baseRefreshFailure("graph", 10); ok || reason != "" {
		t.Fatalf("same-base retry retained refresh failure (%q, %t)", reason, ok)
	}
	failures.recordBaseRefresh("graph", 10, "retry")
	failures.clearBaseRefresh("graph", 10)
	if _, ok := failures.baseRefreshFailure("graph", 10); ok {
		t.Fatal("successful refresh did not clear its graph/base verdict")
	}
}
