package main

// PURPOSE — unit tests for `gortex track --wait`: the per-repo "index settled"
// classification and the poll loop, exercised without a running daemon by
// stubbing the trackStatusFn seam and shrinking the poll interval.
// KEYWORDS — track, wait, daemon, indexing, poll

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func statusWithRepo(absPath string, nodes int, ready bool) daemon.StatusResponse {
	return daemon.StatusResponse{
		Ready:        ready,
		TrackedRepos: []daemon.TrackedRepoStatus{{Path: absPath, Nodes: nodes}},
	}
}

func useLegacyTrackReadiness(t *testing.T) {
	t.Helper()
	original := trackReadinessFn
	trackReadinessFn = func(string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{State: daemon.TrackReadinessLegacy}, nil
	}
	t.Cleanup(func() { trackReadinessFn = original })
}

func TestRepoNodeCount(t *testing.T) {
	abs := t.TempDir()
	st := statusWithRepo(abs, 1234, true)
	if got := repoNodeCount(st, abs); got != 1234 {
		t.Errorf("repoNodeCount = %d, want 1234", got)
	}
	if got := repoNodeCount(st, abs+"/other"); got != -1 {
		t.Errorf("repoNodeCount(absent) = %d, want -1", got)
	}
}

func TestIndexSettled(t *testing.T) {
	abs := t.TempDir()
	cases := []struct {
		name      string
		st        daemon.StatusResponse
		prevNodes int
		want      bool
	}{
		{"absent", daemon.StatusResponse{Ready: true}, -1, false},
		{"not ready", statusWithRepo(abs, 100, false), 100, false},
		{"zero nodes first reading", statusWithRepo(abs, 0, true), -1, false},
		{"zero nodes stable", statusWithRepo(abs, 0, true), 0, true},
		{"count still moving", statusWithRepo(abs, 200, true), 100, false},
		{"stable and ready", statusWithRepo(abs, 200, true), 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := indexSettled(tc.st, abs, tc.prevNodes); got != tc.want {
				t.Errorf("indexSettled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWaitForRepoIndexed_Settles(t *testing.T) {
	useLegacyTrackReadiness(t)
	abs := t.TempDir()
	origFn, origInterval := trackStatusFn, trackPollInterval
	t.Cleanup(func() { trackStatusFn, trackPollInterval = origFn, origInterval })
	trackPollInterval = time.Millisecond

	// not-present -> growing -> stable+ready: settles on the repeated reading.
	seq := []daemon.StatusResponse{
		{Ready: false},
		statusWithRepo(abs, 100, true),
		statusWithRepo(abs, 500, true),
		statusWithRepo(abs, 500, true),
	}
	i := 0
	trackStatusFn = func() (daemon.StatusResponse, error) {
		st := seq[i]
		if i < len(seq)-1 {
			i++
		}
		return st, nil
	}

	var buf bytes.Buffer
	if err := waitForRepoIndexed(&buf, abs, time.Second); err != nil {
		t.Fatalf("waitForRepoIndexed: %v", err)
	}
}

func TestWaitForRepoIndexed_EmptyRepoSettles(t *testing.T) {
	useLegacyTrackReadiness(t)
	abs := t.TempDir()
	origFn, origInterval := trackStatusFn, trackPollInterval
	t.Cleanup(func() { trackStatusFn, trackPollInterval = origFn, origInterval })
	trackPollInterval = time.Millisecond

	trackStatusFn = func() (daemon.StatusResponse, error) {
		return statusWithRepo(abs, 0, true), nil
	}

	var buf bytes.Buffer
	if err := waitForRepoIndexed(&buf, abs, time.Second); err != nil {
		t.Fatalf("empty repository should settle: %v", err)
	}
}

func TestWaitForRepoIndexed_Timeout(t *testing.T) {
	useLegacyTrackReadiness(t)
	abs := t.TempDir()
	origFn, origInterval := trackStatusFn, trackPollInterval
	t.Cleanup(func() { trackStatusFn, trackPollInterval = origFn, origInterval })
	trackPollInterval = time.Millisecond
	// Never settles: zero nodes and not ready forever.
	trackStatusFn = func() (daemon.StatusResponse, error) {
		return statusWithRepo(abs, 0, false), nil
	}

	var buf bytes.Buffer
	if err := waitForRepoIndexed(&buf, abs, 5*time.Millisecond); err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestWaitForRepoIndexed_RoutedReadySettlesWithoutStatus(t *testing.T) {
	abs := t.TempDir()
	originalReadiness, originalStatus := trackReadinessFn, trackStatusFn
	t.Cleanup(func() {
		trackReadinessFn, trackStatusFn = originalReadiness, originalStatus
	})
	calls := 0
	trackReadinessFn = func(path string) (daemon.TrackReadiness, error) {
		calls++
		if path != abs {
			t.Fatalf("readiness path = %q, want %q", path, abs)
		}
		return daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  &daemon.ProbeView{Kind: daemon.ProbeViewBase, Exact: true},
		}, nil
	}
	trackStatusFn = func() (daemon.StatusResponse, error) {
		t.Fatal("routed readiness must not scan status counters")
		return daemon.StatusResponse{}, nil
	}

	var buf bytes.Buffer
	if err := waitForRepoIndexed(&buf, abs, time.Second); err != nil {
		t.Fatalf("ready routed view should settle: %v", err)
	}
	if calls != 1 {
		t.Fatalf("readiness calls = %d, want immediate first-poll success", calls)
	}
}

func TestWaitForRepoIndexed_BuildingAndFallbackDoNotSettle(t *testing.T) {
	abs := t.TempDir()
	originalReadiness, originalStatus, originalInterval := trackReadinessFn, trackStatusFn, trackPollInterval
	t.Cleanup(func() {
		trackReadinessFn, trackStatusFn, trackPollInterval = originalReadiness, originalStatus, originalInterval
	})
	trackPollInterval = time.Millisecond
	calls := 0
	trackReadinessFn = func(string) (daemon.TrackReadiness, error) {
		calls++
		if calls == 1 {
			return daemon.TrackReadiness{
				State: daemon.TrackReadinessBuilding,
				View:  &daemon.ProbeView{Kind: daemon.ProbeViewUnrouted, Exact: false, FallbackReason: daemon.FallbackViewBuilding},
			}, nil
		}
		if calls == 2 {
			// Even an inconsistent `ready` label cannot override the honesty bit.
			return daemon.TrackReadiness{
				State: daemon.TrackReadinessReady,
				View:  &daemon.ProbeView{Kind: daemon.ProbeViewBase, Exact: false, FallbackReason: daemon.FallbackViewBuilding},
			}, nil
		}
		return daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  &daemon.ProbeView{Kind: daemon.ProbeViewWorktree, Exact: true},
		}, nil
	}
	trackStatusFn = func() (daemon.StatusResponse, error) {
		t.Fatal("building routed view must not use legacy status counters")
		return daemon.StatusResponse{}, nil
	}

	var buf bytes.Buffer
	if err := waitForRepoIndexed(&buf, abs, time.Second); err != nil {
		t.Fatalf("wait for routed publication: %v", err)
	}
	if calls < 3 {
		t.Fatalf("readiness calls = %d, want building and fallback polls before ready", calls)
	}
}

func TestWaitForRepoIndexed_PromotionFailureExitsImmediately(t *testing.T) {
	abs := t.TempDir()
	originalReadiness, originalStatus := trackReadinessFn, trackStatusFn
	t.Cleanup(func() {
		trackReadinessFn, trackStatusFn = originalReadiness, originalStatus
	})
	calls := 0
	trackReadinessFn = func(string) (daemon.TrackReadiness, error) {
		calls++
		return daemon.TrackReadiness{
			State: daemon.TrackReadinessFailed,
			Error: "synthetic promotion failure",
		}, nil
	}
	trackStatusFn = func() (daemon.StatusResponse, error) {
		t.Fatal("failed promotion must not use legacy status counters")
		return daemon.StatusResponse{}, nil
	}

	var buf bytes.Buffer
	err := waitForRepoIndexed(&buf, abs, time.Second)
	if err == nil || !strings.Contains(err.Error(), "synthetic promotion failure") {
		t.Fatalf("failure = %v, want promotion error", err)
	}
	if calls != 1 {
		t.Fatalf("readiness calls = %d, want immediate failure", calls)
	}
}

func BenchmarkWaitForRepoIndexed_RoutedReady(b *testing.B) {
	abs := b.TempDir()
	originalReadiness, originalStatus := trackReadinessFn, trackStatusFn
	b.Cleanup(func() {
		trackReadinessFn, trackStatusFn = originalReadiness, originalStatus
	})
	trackReadinessFn = func(string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  &daemon.ProbeView{Kind: daemon.ProbeViewBase, Exact: true},
		}, nil
	}
	trackStatusFn = func() (daemon.StatusResponse, error) {
		b.Fatal("routed readiness benchmark reached the graph-count status path")
		return daemon.StatusResponse{}, nil
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := waitForRepoIndexed(io.Discard, abs, time.Second); err != nil {
			b.Fatal(err)
		}
	}
}
