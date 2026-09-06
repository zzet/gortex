package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

const (
	checkoutObservationTimeout     = 250 * time.Millisecond
	checkoutObservationWorkTimeout = 5 * time.Second
	checkoutObservationCapacity    = 32
)

type checkoutObservationProof struct {
	inventory    *gitstate.FamilyInventory
	familyID     string
	selected     gitstate.WorktreeRecord
	primary      store_sqlite.DedicatedGraph
	rootInfo     os.FileInfo
	gitInfo      os.FileInfo
	commonInfo   os.FileInfo
	gitMarker    checkoutObservationMarker
	commonMarker checkoutObservationMarker
}

type checkoutObservationMarker struct {
	info   os.FileInfo
	bytes  []byte
	absent bool
}

type checkoutObservationJob struct {
	ctx           context.Context
	cancel        context.CancelFunc
	proofReady    chan struct{}
	authorized    chan struct{}
	authorizeOnce sync.Once
	done          chan struct{}
	proof         *checkoutObservationProof
	proofErr      error
	checkout      store_sqlite.Checkout
	found         bool
	err           error
}

// ObserveCheckoutPath gives a first request in a newly added worktree its
// automatic identity. Only an already known Git family can be observed: this
// is not explicit tracking, and never creates a dedicated graph or config
// entry. Requests wait at most 250ms, but coalesced metadata work continues for
// up to five seconds across retries. Slow Git startup therefore cannot restart
// the same unfinished work indefinitely. Graph builds use existing activation.
//
// found=false, err=nil means the path has no known family to serve it from.
// A busy/error outcome carries no permission to fall through to another graph.
// Explicit selectors must provide an authorizer for the known primary prefix;
// it runs outside locks and before any catalog observation or build activation.
func (l *CheckoutLifecycle) ObserveCheckoutPath(ctx context.Context, path string, authorize ...func(string) error) (checkout store_sqlite.Checkout, found bool, err error) {
	if l == nil || l.catalog == nil || l.rec == nil || path == "" || !filepath.IsAbs(path) {
		return checkout, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return checkout, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutObservationTimeout)
	defer cancel()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
		}
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || retryableCheckoutRefreshError(err)) {
			err = fmt.Errorf("%w: selected checkout discovery is pending: %w", ErrCheckoutMutationBusy, err)
		}
	}()
	job, err := l.checkoutObservation(pathkey.CanonicalExistingRoot(path))
	if err != nil {
		return checkout, false, err
	}
	select {
	case <-ctx.Done():
		return checkout, false, ctx.Err()
	case <-job.proofReady:
	}
	if job.proofErr != nil || job.proof == nil {
		return checkout, false, job.proofErr
	}
	// Authorizers belong to callers and never run in detached workers. Even a
	// completed shared job cannot expose its result to an unauthorized caller.
	for _, allow := range authorize {
		if allow != nil {
			if err := allow(job.proof.primary.RepoPrefix); err != nil {
				return checkout, false, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return checkout, false, err
	}
	job.authorizeOnce.Do(func() { close(job.authorized) })
	select {
	case <-ctx.Done():
		return checkout, false, ctx.Err()
	case <-job.done:
	}
	if job.err != nil || !job.found {
		return checkout, false, job.err
	}
	// Cached completion is not permanent authority: deletion, replacement and
	// primary changes invalidate the proof before any identity is returned.
	if err := l.validateCheckoutObservation(ctx, job.proof); err != nil {
		job.cancel()
		return checkout, false, err
	}
	checkout, found, err = l.catalog.GetCheckout(ctx, job.checkout.CheckoutID)
	if err != nil || !found {
		return store_sqlite.Checkout{}, false, err
	}
	if checkout.Incarnation != job.checkout.Incarnation || !sameMutationRoot(checkout.RootPath, job.proof.selected.Path) {
		return store_sqlite.Checkout{}, false, ErrCheckoutMutationStale
	}
	return checkout, true, nil
}

func (l *CheckoutLifecycle) checkoutObservation(path string) (*checkoutObservationJob, error) {
	l.observationMu.Lock()
	defer l.observationMu.Unlock()
	if l.observationClosed {
		return nil, ErrCheckoutRefreshStopped
	}
	if job := l.observationJobs[path]; job != nil {
		if job.ctx.Err() == nil {
			return job, nil
		}
		return nil, fmt.Errorf("%w: expired checkout discovery is draining", ErrCheckoutMutationBusy)
	}
	if len(l.observationJobs) >= checkoutObservationCapacity {
		return nil, fmt.Errorf("%w: checkout discovery capacity reached", ErrCheckoutMutationBusy)
	}
	if l.observationJobs == nil {
		l.observationJobs = make(map[string]*checkoutObservationJob)
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkoutObservationWorkTimeout)
	job := &checkoutObservationJob{ctx: ctx, cancel: cancel, proofReady: make(chan struct{}), authorized: make(chan struct{}), done: make(chan struct{})}
	l.observationJobs[path] = job
	l.observationWG.Add(1)
	go l.runCheckoutObservation(path, job)
	return job, nil
}

func (l *CheckoutLifecycle) runCheckoutObservation(path string, job *checkoutObservationJob) {
	defer l.observationWG.Done()
	defer job.cancel()
	defer l.forgetCheckoutObservation(path, job)
	job.proof, job.proofErr = l.prepareCheckoutObservation(job.ctx, path)
	close(job.proofReady)
	if job.proofErr != nil || job.proof == nil {
		job.err = job.proofErr
		close(job.done)
		return // No negative cache or automatic adoption of unknown families.
	}
	select {
	case <-job.ctx.Done():
		job.err = job.ctx.Err()
	case <-job.authorized:
		job.checkout, job.found, job.err = l.applyCheckoutObservation(job.ctx, job.proof)
	}
	if job.err != nil && job.ctx.Err() != nil {
		job.err = errors.Join(job.ctx.Err(), job.err)
	}
	close(job.done)
	if job.err == nil && job.found {
		// A caller that timed out just before publication can retrieve this
		// result. One bounded worker owns expiry; no unbounded timer registry.
		<-job.ctx.Done()
	}
}

func (l *CheckoutLifecycle) forgetCheckoutObservation(path string, job *checkoutObservationJob) {
	l.observationMu.Lock()
	if l.observationJobs[path] == job {
		delete(l.observationJobs, path)
	}
	l.observationMu.Unlock()
}

func (l *CheckoutLifecycle) closeCheckoutObservations() {
	l.observationMu.Lock()
	l.observationClosed = true
	for _, job := range l.observationJobs {
		job.cancel()
	}
	l.observationMu.Unlock()
	l.observationWG.Wait()
}

func (l *CheckoutLifecycle) prepareCheckoutObservation(ctx context.Context, path string) (*checkoutObservationProof, error) {
	inventory := gitstate.Inventory
	if l.observationInventory != nil {
		inventory = l.observationInventory
	}
	inv, err := inventory(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.Join(ctx.Err(), err)
		}
		return nil, nil // Not a Git checkout; ordinary scope resolution may continue.
	}
	familyID := FamilyIDFor(inv.CommonDir)
	family, known, err := l.catalog.GetRepositoryFamily(ctx, familyID)
	if err != nil || !known {
		return nil, err
	}
	if !sameMutationRoot(family.CommonDirIdentity, inv.CommonDir) {
		return nil, ErrCheckoutMutationStale
	}
	var selected *gitstate.WorktreeRecord
	for i := range inv.Records {
		record := &inv.Records[i]
		gitDir := inv.CommonDir
		if !record.IsMain {
			gitDir = filepath.Join(inv.CommonDir, "worktrees", record.AdminName)
		}
		if !record.Bare && record.AdminName != "" && sameMutationRoot(gitDir, inv.GitDir) && checkoutObservationContains(path, record.Path) {
			selected = record
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("%w: Git did not identify the selected working copy", ErrCheckoutMutationStale)
	}
	primary, err := l.observationPrimary(ctx, familyID)
	if err != nil {
		return nil, err
	}
	proof := &checkoutObservationProof{inventory: inv, familyID: familyID, selected: *selected, primary: primary}
	proof.rootInfo, err = checkoutRootFileInfo(selected.Path)
	if err != nil {
		return nil, err
	}
	proof.gitInfo, err = checkoutRootFileInfo(inv.GitDir)
	if err != nil {
		return nil, err
	}
	proof.commonInfo, err = checkoutRootFileInfo(inv.CommonDir)
	if err != nil {
		return nil, err
	}
	proof.gitMarker, err = readCheckoutObservationMarker(filepath.Join(selected.Path, ".git"))
	if err != nil || proof.gitMarker.absent {
		return nil, ErrCheckoutMutationStale
	}
	proof.commonMarker, err = readCheckoutObservationMarker(filepath.Join(inv.GitDir, "commondir"))
	if err != nil {
		return nil, err
	}
	// Inventory precedes these snapshots. Confirm they still describe the
	// inventory's Git family before accepting them as a cached proof; otherwise
	// a replaced .git marker could pair new physical identity with old scope.
	// This resolves directories only, without another worktree-list inventory.
	gitDir, commonDir, err := gitstate.ResolveFamilyDirs(ctx, path)
	if err != nil {
		return nil, err
	}
	if !sameMutationRoot(gitDir, inv.GitDir) || !sameMutationRoot(commonDir, inv.CommonDir) {
		return nil, ErrCheckoutMutationStale
	}
	if err := l.validateCheckoutObservation(ctx, proof); err != nil {
		return nil, err
	}
	return proof, nil
}

func readCheckoutObservationMarker(path string) (checkoutObservationMarker, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return checkoutObservationMarker{absent: true}, nil
	}
	if err != nil {
		return checkoutObservationMarker{}, err
	}
	defer file.Close()
	info, err := file.Stat() // Eager physical identity on Windows as well.
	if err != nil {
		return checkoutObservationMarker{}, err
	}
	marker := checkoutObservationMarker{info: info}
	if !info.IsDir() {
		const maxMarkerBytes = 64 * 1024
		marker.bytes, err = io.ReadAll(io.LimitReader(file, maxMarkerBytes+1))
		if len(marker.bytes) > maxMarkerBytes {
			return checkoutObservationMarker{}, ErrCheckoutMutationStale
		}
	}
	return marker, err
}

func checkoutObservationMarkerMatches(path string, expected checkoutObservationMarker) bool {
	actual, err := readCheckoutObservationMarker(path)
	if err != nil || actual.absent != expected.absent {
		return false
	}
	return actual.absent || (os.SameFile(actual.info, expected.info) && bytes.Equal(actual.bytes, expected.bytes))
}

func (l *CheckoutLifecycle) validateCheckoutObservation(ctx context.Context, proof *checkoutObservationProof) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := checkoutRootFileInfo(proof.selected.Path)
	if err != nil || !os.SameFile(root, proof.rootInfo) {
		return ErrCheckoutMutationStale
	}
	gitDir, err := checkoutRootFileInfo(proof.inventory.GitDir)
	if err != nil || !os.SameFile(gitDir, proof.gitInfo) {
		return ErrCheckoutMutationStale
	}
	commonDir, err := checkoutRootFileInfo(proof.inventory.CommonDir)
	if err != nil || !os.SameFile(commonDir, proof.commonInfo) {
		return ErrCheckoutMutationStale
	}
	if !checkoutObservationMarkerMatches(filepath.Join(proof.selected.Path, ".git"), proof.gitMarker) ||
		!checkoutObservationMarkerMatches(filepath.Join(proof.inventory.GitDir, "commondir"), proof.commonMarker) {
		return ErrCheckoutMutationStale
	}
	primary, err := l.observationPrimary(ctx, proof.familyID)
	if err != nil {
		return err
	}
	if primary.GraphID != proof.primary.GraphID || primary.RepoPrefix != proof.primary.RepoPrefix {
		return ErrCheckoutMutationStale
	}
	return nil
}

func (l *CheckoutLifecycle) applyCheckoutObservation(ctx context.Context, proof *checkoutObservationProof) (store_sqlite.Checkout, bool, error) {
	if err := l.validateCheckoutObservation(ctx, proof); err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	entry, err := l.rec.ObserveCheckout(ctx, proof.familyID, proof.selected.Path, proof.inventory)
	if err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	if !entry.Durable || entry.CheckoutID == "" {
		return store_sqlite.Checkout{}, false, fmt.Errorf("%w: %s", ErrCheckoutNotTracked, entry.Detail)
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, entry.CheckoutID)
	if err != nil || !found {
		return checkout, found, err
	}
	if checkout.Incarnation != entry.Incarnation || !sameMutationRoot(checkout.RootPath, proof.selected.Path) {
		return store_sqlite.Checkout{}, false, ErrCheckoutMutationStale
	}
	if err := l.validateCheckoutObservation(ctx, proof); err != nil {
		return store_sqlite.Checkout{}, false, err
	}
	if checkout.State == store_sqlite.CheckoutStateReady && checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic {
		l.ActivateCheckout(checkout.CheckoutID, "first request observed checkout")
	}
	return checkout, true, nil
}

func (l *CheckoutLifecycle) observationPrimary(ctx context.Context, familyID string) (store_sqlite.DedicatedGraph, error) {
	graphs, err := l.catalog.ListDedicatedGraphs(ctx, familyID)
	if err != nil {
		return store_sqlite.DedicatedGraph{}, err
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase && graph.GraphID != "" {
			return graph, nil
		}
	}
	return store_sqlite.DedicatedGraph{}, fmt.Errorf("%w: known family has no primary graph", ErrCheckoutNotTracked)
}

// Verify both lexical containment and physical root identity. Case folding is
// an identity policy, not proof that two differently spelled directories on a
// case-sensitive volume are the same checkout.
func checkoutObservationContains(path, root string) bool {
	path, root = pathkey.CanonicalExistingRoot(path), pathkey.CanonicalExistingRoot(root)
	if !pathkey.HasPathPrefix(path, root) {
		return false
	}
	for !pathkey.EqualPaths(path, root) {
		parent := filepath.Dir(path)
		if pathkey.EqualPaths(parent, path) {
			return false
		}
		path = parent
	}
	selected, selectedErr := os.Stat(root)
	matched, matchedErr := os.Stat(path)
	return selectedErr == nil && matchedErr == nil && selected.IsDir() && os.SameFile(selected, matched)
}
