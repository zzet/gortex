package main

import (
	"context"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

const (
	probeDiscoveryWait           = 75 * time.Millisecond
	probeDiscoveryWork           = 5 * time.Second
	probeDiscoveryCapacity       = 16
	probeDiscoveryFamilyCapacity = 4
)

// No marker evidence has been obtained yet. A known binding may be served
// read-only with exact:false; observed nested/invalid evidence still refuses.
var errProbeDiscoveryDeferred = errors.New("probe discovery deferred")

type probeCheckoutDiscovery struct {
	done    chan struct{}
	family  string
	binding indexer.ViewBinding
	root    string
	err     error
	// Marker evidence is published before potentially slow registration.
	// Both fields below are guarded by probeDiscoveryMu.
	markerRoot     string
	rejectFallback bool
}

// probeCheckoutBinding keeps filesystem discovery out of the passive known-root
// path. All added filesystem work, including the lifecycle's preflight, runs in
// a bounded worker: filesystem syscalls themselves may ignore cancellation.
func (c *realController) probeCheckoutBinding(ctx context.Context, path string, binding indexer.ViewBinding) (indexer.ViewBinding, string, error) {
	if err := ctx.Err(); err != nil {
		return binding, "", err
	}
	if binding.Matched && pathkey.EqualPaths(path, binding.RootPath) {
		return binding, binding.RootPath, nil
	}
	wait, cancel := context.WithTimeout(ctx, probeDiscoveryWait)
	defer cancel()
	c.probeDiscoveryMu.Lock()
	job := c.probeDiscoveries[path]
	if job == nil {
		// Family identity is already catalogued; never read Git metadata to
		// choose an admission bucket. Undiscovered paths share one bucket.
		family := "unknown"
		if binding.FamilyID != "" {
			family = "family:" + binding.FamilyID
		} else if binding.Matched {
			family = "root:" + binding.RootPath
		}
		familyJobs := 0
		for _, pending := range c.probeDiscoveries {
			if pending.family == family {
				familyJobs++
			}
		}
		if len(c.probeDiscoveries) >= probeDiscoveryCapacity || familyJobs >= probeDiscoveryFamilyCapacity {
			c.probeDiscoveryMu.Unlock()
			return binding, "", errors.Join(errProbeDiscoveryDeferred, indexer.ErrCheckoutMutationBusy)
		}
		if c.probeDiscoveries == nil {
			c.probeDiscoveries = make(map[string]*probeCheckoutDiscovery)
		}
		job = &probeCheckoutDiscovery{done: make(chan struct{}), family: family}
		c.probeDiscoveries[path] = job
		work, stop := context.WithTimeout(context.WithoutCancel(ctx), probeDiscoveryWork)
		go func() {
			defer stop()
			job.binding, job.root, job.err = c.observeProbeCheckout(work, path, binding, job)
			c.probeDiscoveryMu.Lock()
			delete(c.probeDiscoveries, path)
			close(job.done)
			c.probeDiscoveryMu.Unlock()
		}()
	}
	c.probeDiscoveryMu.Unlock()
	select {
	case <-wait.Done():
		c.probeDiscoveryMu.Lock()
		root, refuse := job.markerRoot, job.rejectFallback
		c.probeDiscoveryMu.Unlock()
		if refuse {
			return binding, root, errors.Join(indexer.ErrCheckoutMutationBusy, wait.Err())
		}
		return binding, root, errors.Join(errProbeDiscoveryDeferred, indexer.ErrCheckoutMutationBusy, wait.Err())
	case <-job.done:
		return job.binding, job.root, job.err
	}
}

func (c *realController) observeProbeCheckout(ctx context.Context, path string, binding indexer.ViewBinding, job *probeCheckoutDiscovery) (indexer.ViewBinding, string, error) {
	readRoot := c.probeGitRoot
	if readRoot == nil {
		readRoot = func(path string) (string, bool) {
			root, _, ok := gitstate.MarkerCommonDir(path)
			if root != "" {
				root = pathkey.CanonicalExistingRoot(root)
			}
			return root, ok
		}
	}
	root, gitPath := readRoot(path)
	c.probeDiscoveryMu.Lock()
	job.markerRoot = root
	job.rejectFallback = root != "" && (!gitPath || (binding.Matched && !pathkey.EqualPaths(root, binding.RootPath)))
	c.probeDiscoveryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return binding, root, errors.Join(indexer.ErrCheckoutMutationBusy, err)
	}
	if root != "" && !gitPath {
		// An unreadable or invalid marker can hide a nested checkout.
		return binding, root, indexer.ErrCheckoutMutationStale
	}

	nested := binding.Matched && gitPath && !pathkey.EqualPaths(root, binding.RootPath)
	if !gitPath || (binding.Matched && !nested) {
		return binding, root, nil
	}
	// Nested Git evidence invalidates the enclosing binding even when an
	// unrelated declaration prevents discovery. Never revive that false exact
	// answer just because the new checkout cannot be registered yet.
	if nested && c.multiIndexer != nil && c.multiIndexer.RepoIdentityPending() {
		return binding, root, indexer.ErrCheckoutMutationBusy
	}
	_, found, err := c.lifecycle.ObserveCheckoutPath(ctx, path)
	if err != nil {
		return binding, root, err
	}
	if !found {
		if nested {
			return binding, root, indexer.ErrCheckoutMutationStale
		}
		return binding, root, nil
	}
	binding, err = c.lifecycle.ExplainView(ctx, path)
	if err == nil && !binding.Matched {
		err = indexer.ErrCheckoutMutationStale
	}
	return binding, root, err
}
