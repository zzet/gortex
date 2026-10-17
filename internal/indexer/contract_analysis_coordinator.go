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
	"github.com/zzet/gortex/internal/viewmetrics"
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
	// Capture runs on the scheduler's job slot: the census of an actor's
	// wanted inputs from the source its request prepared.
	Capture ContractFollowupSourceCapture
	// Inputs, when set, captures a primary actor's inputs again after a
	// cohort Published: the cohort's current input vector composed for repo.
	Inputs func(ctx context.Context, repos []string, repo string) (*graphview.SelectedContractInputs, error)
	// LinkedInputs reacquires current selected sources after a linked segment
	// consumed its handoff. Nil preserves non-resumable linked execution.
	LinkedInputs ContractLinkedInputCapture
	// ReconcileBaseline admits an independently owned baseline job. It must not
	// retain the caller's RepoView or report success for unavailable capture.
	ReconcileBaseline  func(context.Context, *graphview.RepoView, string, string) (bool, error)
	Yield              func(context.Context) error
	ScratchParent      string
	RequireRepoConfigs bool
	// RetireReservation, when set, transfers a failed reservation to the
	// existing durable deferred retirement owner after fencing it. A refusal
	// keeps the ordinary synchronous cleanup fallback. Standalone coordinators
	// without that owner retain full retirement.
	RetireReservation func(context.Context, *store_sqlite.Store, int64, func(int64) bool) error
	// QueueCapacity bounds the actors with pending or running work, 1..32;
	// zero means 8.
	QueueCapacity int
	// JobSlots is the number of global job slots; zero means one.
	JobSlots int
	// Now replaces time.Now for interest and backoff.
	Now func() time.Time
}

type contractAnalysisJob struct {
	id          string
	publication *ContractPublishedCohort
	delta       *contractDeltaSession
	snapshot    ContractFollowupSnapshot
	targets     []ContractFollowupTarget
	// trigger is what queued the job and queuedAt when; capture is how long
	// capturing its snapshot took.
	trigger           string
	queuedAt          time.Time
	capture           time.Duration
	otherActorWaiting func() bool
}

// contractJobTriggerInterest queues a job for a request's interest in its key.
const contractJobTriggerInterest = "interest"

// ContractAnalysisCoordinator schedules contract analysis per actor, outside
// the requests that ask for it (contract_analysis_scheduler.go). Core
// publication notifications only mark interested actors and wake waiters;
// ordinary tools never capture, read or wait on contract payloads through
// this component.
type ContractAnalysisCoordinator struct {
	deltaCaches            contractDeltaLRU
	deltaRegistryEvictions uint64
	options                ContractAnalysisCoordinatorOptions
	ctx                    context.Context
	cancel                 context.CancelFunc
	// workers joins every dispatcher and job goroutine.
	workers   sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	changed   chan struct{}
	published uint64
	// acceptedAt holds when each repository last announced an accepted core
	// mutation.
	acceptedAt      map[string]time.Time
	actors          map[ContractActor]*contractActor
	rings           map[string]*contractRing
	slots           *contractJobSlots
	capacityEpoch   uint64
	capacityChanged chan struct{}
	// meanJob is the running mean of job body times: a refused request's
	// retry hint.
	meanJob time.Duration
	// run replaces execute in scheduler tests.
	run func(*contractAnalysisJob) string
	// repairPending, when set, reports whether a stuck-input repair is due
	// for a cohort: an unsettled job's actor then waits for its accept.
	repairPending func(repos []string) bool
	// unsettled, when set, asks the repair to scan a cohort a job could not
	// settle its reads in.
	unsettled func(repos []string)
}

func NewContractAnalysisCoordinator(options ContractAnalysisCoordinatorOptions) (*ContractAnalysisCoordinator, error) {
	if options.Store == nil || options.Leases == nil || options.Registry == nil || options.Capture == nil || options.Yield == nil {
		return nil, fmt.Errorf("contract analysis coordinator: incomplete options")
	}
	if options.QueueCapacity == 0 {
		options.QueueCapacity = contractSchedulerCapacity
	}
	if options.QueueCapacity < 1 || options.QueueCapacity > contractSchedulerMaxCapacity {
		return nil, fmt.Errorf("contract analysis coordinator: invalid queue capacity")
	}
	if options.Logger == nil {
		options.Logger = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &ContractAnalysisCoordinator{options: options, ctx: ctx, cancel: cancel, changed: make(chan struct{}),
		acceptedAt: make(map[string]time.Time), actors: make(map[ContractActor]*contractActor), rings: make(map[string]*contractRing),
		slots: &contractJobSlots{free: contractJobSlotCount(options.JobSlots)}, capacityChanged: make(chan struct{})}
	return c, nil
}

func contractAnalysisCohortID(snapshot ContractFollowupSnapshot, cfg config.IndexConfig) (string, error) {
	// Exact physical witnesses preserve source actor, order, Found and Accepted.
	// No source clock or latest attachment substitutes for immutable authority.
	// File source fingerprints are left out: a job proves its own reads by
	// receipt stamps, so a body-level save between two captures of one key
	// joins the running job instead of queueing another.
	files := make([]ContractFollowupFile, len(snapshot.Files))
	for i, file := range snapshot.Files {
		file.SourceFingerprint = ""
		files[i] = file
	}
	encoded, err := json.Marshal(struct {
		Inputs             []graph.ContractInputWitness
		Files              []ContractFollowupFile
		Config             config.IndexConfig
		RepoConfigs        map[string]config.IndexConfig
		TrackedRepoModules map[string]string
	}{snapshot.Inputs, files, cfg, snapshot.RepoConfigs, snapshot.TrackedRepoModules})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Published announces an accepted core mutation of repo: it steps the input
// epoch, marks the interested actors whose cohort holds repo and wakes every
// waiter. It runs inside the writer's repository lane and takes only c.mu.
func (c *ContractAnalysisCoordinator) Published(_ context.Context, repo, _ string) {
	c.mu.Lock()
	c.published++
	c.acceptedAt[repo] = time.Now()
	c.markPublishedLocked(repo, c.now())
	c.notifyLocked()
	c.mu.Unlock()
}

// lastAccepted is when a repository of the cohort last announced an accepted
// core mutation, zero when none did.
func (c *ContractAnalysisCoordinator) lastAccepted(inputs []graph.ContractInputWitness) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var last time.Time
	for _, witness := range inputs {
		if at := c.acceptedAt[witness.State.RepoPrefix]; at.After(last) {
			last = at
		}
	}
	return last
}

// publishedEpoch is the level a running job samples before a round's first
// scan; waitPublished returns once a Published event passed it, so an accept
// and its announcement landing between that scan and the wait still wake the
// job's retry.
func (c *ContractAnalysisCoordinator) publishedEpoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.published
}

func (c *ContractAnalysisCoordinator) waitPublished(ctx context.Context, since uint64, until time.Time) bool {
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	for {
		c.mu.Lock()
		epoch, changed := c.published, c.changed
		c.mu.Unlock()
		if epoch > since {
			return true
		}
		select {
		case <-changed:
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		case <-c.ctx.Done():
			return false
		}
	}
}

func (c *ContractAnalysisCoordinator) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// execute is a job's body: it reserves a payload per missing target, runs
// the cohort job and reports how it ended, "" when every target was attached
// already.
func (c *ContractAnalysisCoordinator) execute(job *contractAnalysisJob) string {
	defer job.snapshot.Release()
	if err := c.ctx.Err(); err != nil {
		return viewmetrics.ContractJobPreempted
	}
	targets := make([]ContractFollowupTarget, 0, len(job.targets))
	reserved := make([]ContractFollowupTarget, 0, len(job.targets))
	published := make(map[int64]bool)
	defer func() {
		// Failed reservations never become core routes. Successful attachments own
		// durable references; retirement refuses those even after a late error.
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, target := range reserved {
			if published[target.Payload.ViewGeneration()] {
				continue
			}
			generation := target.Payload.ViewGeneration()
			var err error
			if c.options.RetireReservation != nil {
				err = c.options.RetireReservation(cleanup, target.Catalog, generation, c.options.Leases.InUse)
				if err == nil || errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced) {
					continue
				}
				c.options.Logger.Warn("contract reservation retirement handoff refused", zap.Int64("generation", generation), zap.Error(err))
			}
			err = target.Catalog.RetirePayloadGeneration(cleanup, generation, c.options.Leases.InUse)
			if err != nil && !errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced) {
				c.options.Logger.Warn("contract analysis reservation retirement deferred", zap.Int64("generation", generation), zap.Error(err))
			}
		}
	}()
	for _, target := range job.targets {
		attachment, err := target.Catalog.GetContractAttachmentContext(c.ctx, target.Key)
		if err != nil {
			c.options.Logger.Warn("contract attachment lookup failed", zap.Error(err))
			return viewmetrics.ContractJobFailed
		}
		if attachment != nil {
			continue
		}
		attempt := make([]byte, 16)
		if _, err := rand.Read(attempt); err != nil {
			c.options.Logger.Warn("contract reservation identity failed", zap.Error(err))
			return viewmetrics.ContractJobFailed
		}
		encoded, err := json.Marshal(target.Key)
		if err != nil {
			return viewmetrics.ContractJobFailed
		}
		keyDigest := sha256.Sum256(encoded)
		graphDigest := sha256.Sum256([]byte(target.Key.RepoPrefix + "\x00" + target.Key.CheckoutID))
		_, payload, err := target.Catalog.BeginPayloadGeneration(c.ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "contract-analysis:" + hex.EncodeToString(graphDigest[:]), CheckoutID: target.Key.CheckoutID, GenerationKind: "contract_analysis", LayerID: hex.EncodeToString(keyDigest[:]) + ":" + job.id + ":" + hex.EncodeToString(attempt), LowerViewFingerprint: job.id, CreatedAt: time.Now().Unix()})
		if err != nil {
			c.options.Logger.Warn("contract payload reservation failed", zap.Error(err))
			return viewmetrics.ContractJobFailed
		}
		target.Payload = payload
		targets = append(targets, target)
		reserved = append(reserved, target)
	}
	if len(targets) == 0 {
		return ""
	}
	started := time.Now()
	// The time from the last accepted core mutation of the cohort, -1 when
	// none was announced since the coordinator started.
	sinceAccept := -1.0
	if accepted := c.lastAccepted(job.snapshot.Inputs); !accepted.IsZero() {
		sinceAccept = contractMillis(started.Sub(accepted))
	}
	dispatch := started.Sub(job.queuedAt)
	startMode := viewmetrics.ContractModeCold
	if job.delta != nil {
		startMode = viewmetrics.ContractModeFill
		if p := job.delta.cache; p != nil {
			startMode = viewmetrics.ContractModeDelta
			if p.identity != job.delta.identity || p.deltas+1 >= contractDeltaRefresh {
				startMode = viewmetrics.ContractModeCold
			} else if !p.complete && !p.hasBaseline {
				startMode = viewmetrics.ContractModeFill
			}
		}
		for _, target := range targets {
			if contractDeltaMembershipUnknown(target.Work) {
				startMode = viewmetrics.ContractModeCold
			}
		}
	}
	c.options.Logger.Info("contract analysis job started", zap.String("mode", startMode), zap.String("job", job.id),
		zap.String("repo", targets[0].Key.RepoPrefix), zap.String("checkout", targets[0].Key.CheckoutID), zap.Int("targets", len(targets)),
		zap.String("trigger", job.trigger), zap.Float64("since_trigger_ms", contractMillis(dispatch)),
		zap.Float64("since_accept_ms", sinceAccept), zap.Float64("capture_ms", contractMillis(job.capture)))
	report, err := RunContractFollowupBatch(c.ctx, ContractFollowupBatchRequest{Snapshot: job.snapshot, Targets: targets, RebuildReason: ContractFollowupUnknownInterval, Registry: c.options.Registry, Config: c.options.Config, RepoConfigs: job.snapshot.RepoConfigs, Logger: c.options.Logger, Leases: c.options.Leases, ScratchParent: c.options.ScratchParent, Yield: c.options.Yield,
		PublishedEpoch: c.publishedEpoch, WaitPublished: c.waitPublished, OtherActorWaiting: job.otherActorWaiting, delta: job.delta})
	if err != nil && !errors.As(err, new(*ContractFillContinued)) {
		c.options.Logger.Warn("contract cohort analysis failed", zap.Error(err))
	}
	var targetErr error
	for _, result := range report.Targets {
		if result.Report.Published {
			published[result.Report.PayloadGeneration] = true
		}
		if result.Err != nil {
			targetErr = result.Err
			if !errors.As(result.Err, new(*ContractFillContinued)) {
				c.options.Logger.Warn("contract target publication failed", zap.String("repo", result.Key.RepoPrefix), zap.Error(result.Err))
			}
		}
	}
	if err == nil {
		err = targetErr
	}
	// Only the plural transaction's complete target vector proves this cohort.
	// Previously attached targets skipped above have their own historical proof;
	// never invent it from this job's current source witnesses or stamps.
	if err == nil && len(targets) == len(job.targets) && contractPublishedCohortComplete(report.Publication) {
		job.publication = cloneContractPublishedCohort(report.Publication)
	}
	elapsed := time.Since(started)
	outcome := contractJobOutcome(err)
	if c.ctx.Err() != nil && err != nil {
		outcome = viewmetrics.ContractJobPreempted
	}
	mode := report.Cohort.Mode
	if mode == "" {
		mode = viewmetrics.ContractModeCold
	}
	countContractJob(mode, outcome, elapsed)
	logContractJob(c.options.Logger, mode, outcome, elapsed, report,
		zap.Float64("dispatch_ms", contractMillis(dispatch)), zap.Float64("capture_ms", contractMillis(job.capture)))
	return outcome
}

// contractJobOutcome classifies a finished job body for the jobs counter.
func contractJobOutcome(err error) string {
	return contractJobOutcomeOf(err)
}

// Close cancels running work, releases every queued source handoff, then
// waits until every dispatcher and job, including its durable reservation
// retirement handoff (or standalone physical retirement), and
// leases, has ended.
func (c *ContractAnalysisCoordinator) Close() error { return c.CloseContext(context.Background()) }

// CloseContext preserves canceled pending debt and exposes an undrained worker
// to its owner on timeout. The owner must not close the backing store early.
func (c *ContractAnalysisCoordinator) CloseContext(ctx context.Context) error {
	var drained []*ContractCaptureSource
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
		for _, a := range c.actors {
			drained = append(drained, a.want.source)
			a.want.source = nil
			a.notifyLocked()
		}
		c.notifyLocked()
	}
	c.mu.Unlock()
	for _, source := range drained {
		source.Release()
	}
	done := make(chan struct{})
	go func() {
		c.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
