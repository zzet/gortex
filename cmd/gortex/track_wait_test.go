package main

// PURPOSE — unit tests for `gortex track --wait`: the per-repo "index settled"
// classification and the poll loop, exercised without a running daemon by
// stubbing the trackStatusFn seam and shrinking the poll interval.
// KEYWORDS — track, wait, daemon, indexing, poll

import (
	"bytes"
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
