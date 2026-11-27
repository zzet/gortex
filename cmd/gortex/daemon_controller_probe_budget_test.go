package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestProbeKnownRootDoesNotReadGitMetadata(t *testing.T) {
	root := t.TempDir()
	c := &realController{probeGitRoot: func(string) (string, bool) {
		t.Error("a known checkout-root probe must stay passive")
		return "", false
	}}
	binding := indexer.ViewBinding{Matched: true, RootPath: root}
	got, _, err := c.probeCheckoutBinding(t.Context(), root, binding)
	require.NoError(t, err)
	require.Equal(t, binding, got)
}

func TestProbeMetadataCancellationAndRetryShareBoundedWork(t *testing.T) {
	root := t.TempDir()
	started, unblock := make(chan struct{}, 1), make(chan struct{})
	var calls atomic.Int32
	c := &realController{probeGitRoot: func(string) (string, bool) {
		calls.Add(1)
		started <- struct{}{}
		<-unblock // simulate a filesystem call which ignores cancellation
		return root, true
	}}
	defer finishProbeMetadataWorkers(t, c, unblock)
	binding := indexer.ViewBinding{Matched: true, RootPath: root}
	path := filepath.Join(root, "lib.go")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := c.probeCheckoutBinding(ctx, path, binding); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata worker never started")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation waited for filesystem metadata")
	}
	// The retry must return on the probe budget even with no caller deadline,
	// and must join the existing worker rather than start another blocked read.
	go func() { _, _, err := c.probeCheckoutBinding(t.Context(), path, binding); done <- err }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, err, indexer.ErrCheckoutMutationBusy)
	case <-time.After(3 * time.Second):
		t.Fatal("probe exceeded its own discovery budget")
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestProbeMetadataWorkerCapacityPreservesKnownRoot(t *testing.T) {
	root := t.TempDir()
	started, unblock := make(chan struct{}, probeDiscoveryCapacity), make(chan struct{})
	var calls atomic.Int32
	c := &realController{probeGitRoot: func(string) (string, bool) {
		calls.Add(1)
		started <- struct{}{}
		<-unblock
		return root, true
	}}
	defer finishProbeMetadataWorkers(t, c, unblock)
	binding := indexer.ViewBinding{Matched: true, RootPath: root}
	for i := range probeDiscoveryCapacity {
		familyBinding := binding
		familyBinding.FamilyID = fmt.Sprintf("family-%d", i)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, _, err := c.probeCheckoutBinding(ctx, filepath.Join(root, fmt.Sprintf("file-%d.go", i)), familyBinding)
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("metadata worker never started")
		}
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	}
	_, _, err := c.probeCheckoutBinding(t.Context(), filepath.Join(root, "overflow.go"), binding)
	require.ErrorIs(t, err, indexer.ErrCheckoutMutationBusy)
	require.EqualValues(t, probeDiscoveryCapacity, calls.Load())
	_, _, err = c.probeCheckoutBinding(t.Context(), root, binding)
	require.NoError(t, err, "blocked filesystem reads must leave passive known-root probes available")
}

func finishProbeMetadataWorkers(t *testing.T, c *realController, unblock chan struct{}) {
	t.Helper()
	c.probeDiscoveryMu.Lock()
	var jobs []*probeCheckoutDiscovery
	for _, job := range c.probeDiscoveries {
		jobs = append(jobs, job)
	}
	c.probeDiscoveryMu.Unlock()
	close(unblock)
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-time.After(3 * time.Second):
			t.Error("metadata worker did not finish after the filesystem recovered")
		}
	}
}

func TestProbeMetadataFamilyCapacityLeavesOtherFamiliesAvailable(t *testing.T) {
	for _, mode := range []string{"family", "root", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			started, unblock := make(chan struct{}, probeDiscoveryCapacity), make(chan struct{})
			c := &realController{probeGitRoot: func(path string) (string, bool) {
				if filepath.Base(path) == "healthy.go" {
					return filepath.Dir(path), true
				}
				started <- struct{}{}
				<-unblock
				return "", false
			}}
			defer finishProbeMetadataWorkers(t, c, unblock)
			binding := indexer.ViewBinding{Matched: true, RootPath: root, FamilyID: "blocked"}
			if mode != "family" {
				binding.FamilyID = ""
			}
			if mode == "unknown" {
				binding = indexer.ViewBinding{}
			}
			start := func(path string, binding indexer.ViewBinding) {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, _, err := c.probeCheckoutBinding(ctx, path, binding); done <- err }()
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("metadata worker never started")
				}
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
			}
			for i := range probeDiscoveryFamilyCapacity {
				// Different checkout roots still share the catalogued family cap.
				if mode == "family" {
					binding.RootPath = filepath.Join(root, fmt.Sprint(i))
				}
				start(filepath.Join(root, fmt.Sprintf("blocked-%d", i)), binding)
			}
			_, _, err := c.probeCheckoutBinding(t.Context(), filepath.Join(root, "overflow"), binding)
			require.ErrorIs(t, err, indexer.ErrCheckoutMutationBusy)
			healthy := indexer.ViewBinding{Matched: true, RootPath: filepath.Join(root, "other"), FamilyID: "healthy"}
			got, _, err := c.probeCheckoutBinding(t.Context(), filepath.Join(healthy.RootPath, "healthy.go"), healthy)
			require.NoError(t, err, "another family must complete discovery while the first remains blocked")
			require.Equal(t, healthy, got)
		})
	}
}
