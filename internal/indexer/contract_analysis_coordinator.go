package indexer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

// ContractFollowupCapture transfers an independent selected-source handoff.
// Targets include all missing attachments in this exact proof-bound cohort;
// their catalog receivers and immutable work are never reconstructed here.
type ContractFollowupCapture func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error)

type ContractAnalysisCoordinatorOptions struct {
	Store    *store_sqlite.Store
	Leases   *graphview.LeaseManager
	Registry *parser.Registry
	Config   config.IndexConfig
	Logger   *zap.Logger
	Capture  ContractFollowupCapture
	// ReconcileBaseline admits an independently owned baseline job. It must not
	// retain the caller's RepoView or report success for unavailable capture.
	ReconcileBaseline  func(context.Context, *graphview.RepoView, string, string) (bool, error)
	Yield              func(context.Context) error
	ScratchParent      string
	RequireRepoConfigs bool
	QueueCapacity      int
}

type contractBaselineWaiter struct {
	key  graph.ContractAttachmentKey
	done <-chan struct{}
}
type contractBaselineObservation struct {
	changed <-chan struct{}
	stop    func() bool
}

type contractAnalysisJob struct {
	id       string
	snapshot ContractFollowupSnapshot
	targets  []ContractFollowupTarget
}

// ContractAnalysisCoordinator owns a bounded single background analysis lane.
// Core publication notifications only wake waiters; ordinary tools never
// capture, read or wait on contract payloads through this component.
type ContractAnalysisCoordinator struct {
	options          ContractAnalysisCoordinatorOptions
	ctx              context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	mu               sync.Mutex
	closed           bool
	queue            []*contractAnalysisJob
	jobs             map[string]*contractAnalysisJob
	keys             map[graph.ContractAttachmentKey]int
	changed          chan struct{}
	ready            chan struct{}
	baselines        map[contractBaselineWaiter]*contractBaselineObservation
	captureAdmission chan struct{}
}

func NewContractAnalysisCoordinator(options ContractAnalysisCoordinatorOptions) (*ContractAnalysisCoordinator, error) {
	if options.Store == nil || options.Leases == nil || options.Registry == nil || options.Capture == nil || options.Yield == nil {
		return nil, fmt.Errorf("contract analysis coordinator: incomplete options")
	}
	if options.QueueCapacity == 0 {
		options.QueueCapacity = 2
	}
	if options.QueueCapacity < 1 || options.QueueCapacity > 32 {
		return nil, fmt.Errorf("contract analysis coordinator: invalid queue capacity")
	}
	if options.Logger == nil {
		options.Logger = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &ContractAnalysisCoordinator{options: options, ctx: ctx, cancel: cancel, done: make(chan struct{}), jobs: make(map[string]*contractAnalysisJob), keys: make(map[graph.ContractAttachmentKey]int), changed: make(chan struct{}), ready: make(chan struct{}, 1), baselines: make(map[contractBaselineWaiter]*contractBaselineObservation), captureAdmission: make(chan struct{}, 1)}
	go c.run()
	return c, nil
}

func (c *ContractAnalysisCoordinator) Request(ctx context.Context, view *graphview.RepoView, inputs *graphview.SelectedContractInputs, repo, checkout string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return false, context.Canceled
	}
	if inputs == nil {
		if c.options.ReconcileBaseline == nil {
			return false, nil
		}
		key := graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout}
		waiter, observation := c.observeProgress(ctx, key)
		admitted, err := c.options.ReconcileBaseline(ctx, view, repo, checkout)
		if !admitted || err != nil {
			c.mu.Lock()
			if c.baselines[waiter] == observation {
				delete(c.baselines, waiter)
			}
			c.mu.Unlock()
			observation.stop()
		}

		return admitted, err
	}
	key := graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout, InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint}
	if !inputs.State.Accepted {
		c.observeProgress(ctx, key)
		return true, nil // Core acceptance/supersession owns eligibility, not this job.
	}
	if key.InputVersion != "" && key.InputFingerprint != "" {
		header, err := c.options.Store.GetContractAttachmentContext(ctx, key)
		if err != nil {
			return false, err
		}
		if header != nil {
			return true, nil
		}
	}
	select {
	case c.captureAdmission <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.ctx.Done():
		return false, context.Canceled
	}
	captureCtx, cancelCapture := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(c.ctx, cancelCapture)
	snapshot, targets, err := c.options.Capture(captureCtx, view, inputs, repo, checkout)
	stopCancel()
	cancelCapture()
	defer func() { <-c.captureAdmission }()
	release := snapshot.Release
	var once sync.Once
	snapshot.Release = func() {
		once.Do(func() {
			if release != nil {
				release()
			}
		})
	}
	if err != nil {
		snapshot.Release()
		return false, err
	}
	if len(targets) == 0 {
		snapshot.Release()
		if key.InputVersion == "" || key.InputFingerprint == "" {
			return false, nil
		}
		header, err := c.options.Store.GetContractAttachmentContext(ctx, key)
		return header != nil, err
	}
	if snapshot.Core == nil || release == nil {
		snapshot.Release()
		return false, fmt.Errorf("contract analysis capture: missing owned handoff")
	}
	if c.options.RequireRepoConfigs {
		for _, file := range snapshot.Files {
			if _, found := snapshot.RepoConfigs[file.RepoPrefix]; !found {
				snapshot.Release()
				return false, fmt.Errorf("contract capture: missing accepted configuration for %q", file.RepoPrefix)
			}
		}
	}
	id, err := contractAnalysisCohortID(snapshot, c.options.Config)
	if err != nil {
		snapshot.Release()
		return false, err
	}
	job := &contractAnalysisJob{id: id, snapshot: snapshot, targets: targets}
	c.mu.Lock()
	if c.closed || ctx.Err() != nil {
		c.mu.Unlock()
		snapshot.Release()
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, context.Canceled
	}
	if existing := c.jobs[id]; existing != nil {
		// Cohort capture must enumerate every missing target before its first job.
		// Never silently attach an unvalidated target to an already running build.
		existingKeys := make(map[graph.ContractAttachmentKey]bool, len(existing.targets))
		for _, target := range existing.targets {
			existingKeys[target.Key] = true
		}
		for _, target := range targets {
			if !existingKeys[target.Key] {
				c.mu.Unlock()
				snapshot.Release()
				return false, nil
			}
		}
		c.mu.Unlock()
		snapshot.Release()
		return true, nil
	}
	if len(c.queue) >= c.options.QueueCapacity {
		c.mu.Unlock()
		snapshot.Release()
		return false, nil
	}
	seen := make(map[graph.ContractAttachmentKey]bool, len(targets))
	for _, target := range targets {
		if target.Catalog == nil || target.Payload != nil || target.Key.InputVersion == "" || target.Key.InputFingerprint == "" || seen[target.Key] {
			c.mu.Unlock()
			snapshot.Release()
			return false, fmt.Errorf("contract analysis capture: invalid target")
		}
		seen[target.Key] = true
	}
	c.jobs[id] = job
	for key := range seen {
		c.keys[key]++
	}
	c.queue = append(c.queue, job)
	c.mu.Unlock()
	select {
	case c.ready <- struct{}{}:
	default:
	}
	return true, nil
}

// observeProgress records a request-local acceptance observation before an
// admission callback can publish. A later episode never inherits a closed one.
func (c *ContractAnalysisCoordinator) observeProgress(ctx context.Context, key graph.ContractAttachmentKey) (contractBaselineWaiter, *contractBaselineObservation) {
	waiter := contractBaselineWaiter{key: key, done: ctx.Done()}
	c.mu.Lock()
	observation := &contractBaselineObservation{changed: c.changed}
	if old := c.baselines[waiter]; old != nil && old.stop != nil {
		old.stop()
	}
	c.baselines[waiter] = observation
	observation.stop = context.AfterFunc(ctx, func() {
		c.mu.Lock()
		if c.baselines[waiter] == observation {
			delete(c.baselines, waiter)
		}
		c.mu.Unlock()
	})
	c.mu.Unlock()
	return waiter, observation
}

func contractAnalysisCohortID(snapshot ContractFollowupSnapshot, cfg config.IndexConfig) (string, error) {
	// Exact physical witnesses preserve source actor, order, Found and Accepted.
	// No source clock or latest attachment substitutes for immutable authority.
	encoded, err := json.Marshal(struct {
		Inputs             []graph.ContractInputWitness
		Files              []ContractFollowupFile
		Config             config.IndexConfig
		RepoConfigs        map[string]config.IndexConfig
		TrackedRepoModules map[string]string
	}{snapshot.Inputs, snapshot.Files, cfg, snapshot.RepoConfigs, snapshot.TrackedRepoModules})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// WaitChange registers before its durable lookup, covering publication racing
// registration. Failures and supersession wake the server to recapture under
// its original RPC deadline; they are never represented as fresh analysis.
func (c *ContractAnalysisCoordinator) WaitChange(ctx context.Context, key graph.ContractAttachmentKey) error {
	c.mu.Lock()
	var changed <-chan struct{} = c.changed
	waiter := contractBaselineWaiter{key: key, done: ctx.Done()}
	progressOnly := false
	if baseline := c.baselines[waiter]; baseline != nil {
		progressOnly = true
		changed = baseline.changed
		delete(c.baselines, waiter)
		if baseline.stop != nil {
			baseline.stop()
		}
	}
	active := c.keys[key] > 0
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return context.Canceled
	}
	if progressOnly || key.InputVersion == "" || key.InputFingerprint == "" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return context.Canceled
		case <-changed:
			return nil
		}
	}
	attachment, err := c.options.Store.GetContractAttachmentContext(ctx, key)
	if err != nil {
		return err
	}
	if attachment != nil || !active {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return context.Canceled
	case <-changed:
		return nil
	}
}

func (c *ContractAnalysisCoordinator) Published(_ context.Context, _, _ string) {
	c.mu.Lock()
	c.notifyLocked()
	c.mu.Unlock()
}
func (c *ContractAnalysisCoordinator) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *ContractAnalysisCoordinator) run() {
	defer close(c.done)
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-c.ctx.Done():
				return
			case <-c.ready:
			}
			continue
		}
		job := c.queue[0]
		c.queue = c.queue[1:]
		c.mu.Unlock()
		c.execute(job)
		c.mu.Lock()
		delete(c.jobs, job.id)
		for _, target := range job.targets {
			c.keys[target.Key]--
			if c.keys[target.Key] == 0 {
				delete(c.keys, target.Key)
			}
		}
		c.notifyLocked()
		c.mu.Unlock()
	}
}

func (c *ContractAnalysisCoordinator) execute(job *contractAnalysisJob) {
	defer job.snapshot.Release()
	if err := c.ctx.Err(); err != nil {
		return
	}
	targets := make([]ContractFollowupTarget, 0, len(job.targets))
	reserved := make([]ContractFollowupTarget, 0, len(job.targets))
	published := make(map[graph.ContractAttachmentKey]bool)
	defer func() {
		// Failed reservations never become core routes. Successful attachments own
		// durable references; retirement refuses those even after a late error.
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, target := range reserved {
			if published[target.Key] {
				continue
			}
			err := target.Catalog.RetirePayloadGeneration(cleanup, target.Payload.ViewGeneration(), c.options.Leases.InUse)
			if err != nil && !errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced) {
				c.options.Logger.Warn("contract analysis reservation retirement deferred", zap.Error(err))
			}
		}
	}()
	for _, target := range job.targets {
		attachment, err := target.Catalog.GetContractAttachmentContext(c.ctx, target.Key)
		if err != nil {
			c.options.Logger.Warn("contract attachment lookup failed", zap.Error(err))
			return
		}
		if attachment != nil {
			continue
		}
		attempt := make([]byte, 16)
		if _, err := rand.Read(attempt); err != nil {
			c.options.Logger.Warn("contract reservation identity failed", zap.Error(err))
			return
		}
		encoded, err := json.Marshal(target.Key)
		if err != nil {
			return
		}
		keyDigest := sha256.Sum256(encoded)
		graphDigest := sha256.Sum256([]byte(target.Key.RepoPrefix + "\x00" + target.Key.CheckoutID))
		_, payload, err := target.Catalog.BeginPayloadGeneration(c.ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "contract-analysis:" + hex.EncodeToString(graphDigest[:]), CheckoutID: target.Key.CheckoutID, GenerationKind: "contract_analysis", LayerID: hex.EncodeToString(keyDigest[:]) + ":" + job.id + ":" + hex.EncodeToString(attempt), LowerViewFingerprint: job.id, CreatedAt: time.Now().Unix()})
		if err != nil {
			c.options.Logger.Warn("contract payload reservation failed", zap.Error(err))
			return
		}
		target.Payload = payload
		targets = append(targets, target)
		reserved = append(reserved, target)
	}
	if len(targets) == 0 {
		return
	}
	report, err := RunContractFollowupBatch(c.ctx, ContractFollowupBatchRequest{Snapshot: job.snapshot, Targets: targets, RebuildReason: ContractFollowupUnknownInterval, Registry: c.options.Registry, Config: c.options.Config, RepoConfigs: job.snapshot.RepoConfigs, Logger: c.options.Logger, Leases: c.options.Leases, ScratchParent: c.options.ScratchParent, Yield: c.options.Yield})
	if err != nil {
		c.options.Logger.Warn("contract cohort analysis failed", zap.Error(err))
	}
	for _, result := range report.Targets {
		if result.Report.Published {
			published[result.Key] = true
		}
		if result.Err != nil {
			c.options.Logger.Warn("contract target publication failed", zap.String("repo", result.Key.RepoPrefix), zap.Error(result.Err))
		}
	}
}

// Close cancels running work, drains every queued source handoff, then waits
// until payload retirement and the worker's independent leases are released.
func (c *ContractAnalysisCoordinator) Close() error { return c.CloseContext(context.Background()) }

// CloseContext preserves canceled pending debt and exposes an undrained worker
// to its owner on timeout. The owner must not close the backing store early.
func (c *ContractAnalysisCoordinator) CloseContext(ctx context.Context) error {
	var drained []*contractAnalysisJob
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		drained = c.queue
		c.queue = nil
		c.notifyLocked()
	}
	c.mu.Unlock()
	for _, job := range drained {
		job.snapshot.Release()
	}
	select {
	case <-c.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case c.captureAdmission <- struct{}{}:
		<-c.captureAdmission
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ContractAnalysisYield gives foreground demand priority between bounded worker
// units without letting one pause consume a strict RPC's entire budget. After
// 25ms of sustained demand the background lane gets one progress opportunity;
// it holds no core publication lane and yields again at its next checkpoint.
func ContractAnalysisYield(shouldYield func() bool) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if shouldYield == nil || !shouldYield() {
			return nil
		}
		capTimer := time.NewTimer(25 * time.Millisecond)
		defer capTimer.Stop()
		poll := time.NewTicker(5 * time.Millisecond)
		defer poll.Stop()
		for shouldYield() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-capTimer.C:
				return ctx.Err()
			case <-poll.C:
			}
		}
		return ctx.Err()
	}
}
