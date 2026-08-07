package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func probeController(t *testing.T, configBody string) *realController {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(configBody), 0o600))
	cm, err := config.NewConfigManager(path)
	require.NoError(t, err)
	return &realController{configManager: cm}
}

// The reason Probe exists: c.mu is held for the whole of a track / reload /
// enrichment — minutes on a large workspace — and Status takes it. A caller on
// a sub-second budget therefore times out against a healthy daemon and renders
// its "unreachable" fallback.
//
// Probe must answer regardless of what the indexer is doing. Holding c.mu here
// simulates exactly that in-flight mutation.
func TestProbeAnswersDuringLongMutation(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	c.ready.Store(true)

	c.mu.Lock() // stand in for a track / reload / enrichment in flight
	defer c.mu.Unlock()

	type probeResult struct {
		repos int
		err   error
	}
	done := make(chan probeResult, 1)
	go func() {
		resp, err := c.Probe(context.Background())
		done <- probeResult{repos: len(resp.TrackedRepos), err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, 1, got.repos, "probe must still report the tracked registry")
	case <-time.After(2 * time.Second):
		t.Fatal("Probe blocked behind the controller mutex — it must not take c.mu, " +
			"or it reintroduces the stall it exists to avoid")
	}
}

// The contrast that makes the test above meaningful: Status does block on the
// same mutex. If this ever stops blocking, Status was fixed and Probe's
// justification should be revisited rather than silently kept.
func TestStatusBlocksDuringLongMutation(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")

	c.mu.Lock()
	done := make(chan struct{})
	go func() {
		_, _ = c.Status(context.Background())
		close(done)
	}()

	select {
	case <-done:
		c.mu.Unlock()
		t.Fatal("Status no longer blocks on c.mu — re-evaluate whether Probe is still needed")
	case <-time.After(300 * time.Millisecond):
		// Expected: still queued behind the mutex.
	}
	c.mu.Unlock()
	<-done // let the goroutine finish so the test does not leak it
}

// Probe reports identity for every tracked repo, including project-nested
// entries. Dropping those would report a tracked path as untracked, which
// downstream reads as "no repo owns this" — the misread this call prevents.
func TestProbeIncludesProjectNestedRepos(t *testing.T) {
	c := probeController(t, `
repos:
  - path: /work/alpha
    name: canonical
projects:
  side:
    repos:
      - path: /work/beta
`)
	c.ready.Store(true)
	c.enriched.Store(true)

	resp, err := c.Probe(context.Background())
	require.NoError(t, err)
	assert.True(t, resp.Ready)
	assert.True(t, resp.Enriched)

	got := map[string]string{}
	for _, r := range resp.TrackedRepos {
		got[r.Path] = r.Prefix
	}
	assert.Equal(t, map[string]string{
		"/work/alpha": "canonical", // explicit name wins, mirroring config.ResolvePrefix
		"/work/beta":  "beta",
	}, got)
}

// A cancelled caller gets nothing rather than work nobody will read.
func TestProbeHonoursCancelledContext(t *testing.T) {
	c := probeController(t, "repos:\n  - path: /work/alpha\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Probe(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// A daemon with no config manager still answers — liveness does not depend on
// a registry being present, and a nil-config crash here would take out the one
// call that is supposed to work when things are unhealthy.
func TestProbeSurvivesMissingConfig(t *testing.T) {
	c := &realController{}
	c.ready.Store(true)

	resp, err := c.Probe(context.Background())
	require.NoError(t, err)
	assert.True(t, resp.Ready)
	assert.Empty(t, resp.TrackedRepos)
}
