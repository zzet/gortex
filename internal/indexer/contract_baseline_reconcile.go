package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// NewContractBaselineReconciler admits bounded contract-only reconstruction of
// accepted primary sources. Legacy sealed positive payloads require the existing
// core rebuild path; this constructor never edits their immutable rows.
func NewContractBaselineReconciler(options ContractFollowupCaptureOptions) func(context.Context, *graphview.RepoView, string, string) (bool, error) {
	return NewContractBaselineReconcilers(options).Reconcile
}

// ContractBaselineReconcilers are the two admissions of one baseline job
// owner: they share its per-repository jobs and its slots.
type ContractBaselineReconcilers struct {
	// Reconcile is NewContractBaselineReconciler's admission of a namespace
	// whose baseline is missing or of an earlier version. A request it
	// reports admitted is followed by a Published for its repository: it
	// starts a job, coalesces onto one of the repository, or, with both slots
	// taken, waits for a slot ahead of the policy rebuilds.
	Reconcile func(context.Context, *graphview.RepoView, string, string) (bool, error)
	// PolicyChanged admits the rebuild of an accepted primary namespace a
	// capture found holding receipts of an earlier extraction policy
	// (ContractFollowupCaptureOptions.OutdatedPolicy). The job rebuilds only
	// if its own census still finds such a receipt, and announces Published
	// only when it did or a Reconcile request coalesced onto it. A request is
	// dropped while the namespace header is pending (its own baseline owns
	// it), coalesced while a job of the repository runs or waits, and queued
	// while the slots are full. Within the repository's retry window
	// (contractBaselinePolicyRun) it is held and admitted again once the
	// window has passed, so a consumer waiting on the refused capture is not
	// left without a later wake.
	PolicyChanged func(repo string)
}

// contractBaselinePolicyRetry is the shortest gap between two policy rebuilds
// of one repository, measured from the end of the last one, and
// contractBaselinePolicyRetryMax the longest. A rebuild cannot settle a
// receipt whose file is dirty on disk (that file's save owns it), nor one a
// save keeps rewriting under the configuration the running indexer was built
// with when only the configuration manager's copy changed; either way the
// next capture refuses again. The gap doubles for each request that follows
// the last rebuild within twice its window. Tests shorten the base.
var contractBaselinePolicyRetry = time.Minute

const contractBaselinePolicyRetryMax = 30 * time.Minute

// contractBaselinePolicyRun is a repository's last policy rebuild: when it
// ended (when it started, while it runs), and how many such rebuilds
// followed each other closely.
type contractBaselinePolicyRun struct {
	at     time.Time
	streak int
}

func (r contractBaselinePolicyRun) window() time.Duration {
	window := contractBaselinePolicyRetry
	for i := 0; i < r.streak && window < contractBaselinePolicyRetryMax; i++ {
		window *= 2
	}
	return min(window, contractBaselinePolicyRetryMax)
}

// NewContractBaselineReconcilers returns the admissions of one baseline job
// owner.
func NewContractBaselineReconcilers(options ContractFollowupCaptureOptions) ContractBaselineReconcilers {
	var jobsMu sync.Mutex
	jobs := make(map[string]bool)
	slots := make(chan struct{}, 2)
	// All guarded by jobsMu: queued are policy rebuilds waiting for a slot, in
	// arrival order, and turnedAway the Reconcile requests that found both
	// slots taken, likewise; runs are each repository's last policy rebuild;
	// held are the requests a window holds until it passes; policyJobs marks
	// running policy jobs, reported a policy request coalesced onto one, and
	// waiting a Reconcile request coalesced onto any job. A repository is in
	// each queue at most once.
	var queued, turnedAway []string
	queuedSet, turnedAwaySet := make(map[string]bool), make(map[string]bool)
	runs := make(map[string]contractBaselinePolicyRun)
	held := make(map[string]*time.Timer)
	policyJobs, reported, waiting := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	if options.Context != nil {
		context.AfterFunc(options.Context, func() {
			jobsMu.Lock()
			defer jobsMu.Unlock()
			for repo, timer := range held {
				timer.Stop()
				delete(held, repo)
			}
		})
	}
	runtimeReady := func() error {
		if options.Context == nil || options.BaselineAdmissionMu == nil || options.BaselineJobs == nil || options.Store == nil || options.Materializer == nil || options.Materializer.Leases == nil || options.MultiIndexer == nil || options.Yield == nil {
			return fmt.Errorf("contract baseline: incomplete runtime owner")
		}
		return nil
	}
	// admitPolicy reports whether the request now owns or awaits a slot:
	// started, queued, or coalesced with a job that will drain the queue
	// when it ends. A request dropped or held by its window owns none.
	var admitPolicy func(repo string) bool
	// admitTurnedAway admits a Reconcile request recorded in turnedAway, as
	// reconcile would have with a slot free: its consumer waits on the job's
	// Published for the repository.
	var admitTurnedAway func(repo string) bool
	// start runs an admitted job; the caller holds jobsMu and a slot.
	start := func(repo string, policy bool) {
		jobs[repo] = true
		if policy {
			policyJobs[repo] = true
			if timer := held[repo]; timer != nil {
				timer.Stop()
				delete(held, repo)
			}
		}
		countContractBaseline(viewmetrics.ContractBaselineAdmitted)
		lease := options.Materializer.Leases.Acquire(0)
		options.BaselineJobs.Add(1)
		go func() {
			defer options.BaselineJobs.Done()
			// current marks a policy job that found every receipt current;
			// announce, whether the job announces Published for repo.
			announce, current := true, false
			// Admission capacity must be available before waking a consumer
			// that may need to repair another namespace in the same cohort.
			defer func() {
				if !announce {
					return
				}
				if hooks := options.MultiIndexer.contractCoreRuntime.Load(); hooks != nil && hooks.Published != nil {
					hooks.Published(options.Context, repo, "")
				}
			}()
			defer lease.Release()
			defer func() {
				<-slots
				jobsMu.Lock()
				delete(jobs, repo)
				delete(policyJobs, repo)
				if waiting[repo] {
					// A Reconcile request coalesced onto this job waits for
					// its announcement, whatever the job found.
					announce = true
					delete(waiting, repo)
				}
				// A capture that reported while a job which then found
				// nothing ran saw a drift the job's census did not: ask
				// again, after the window, whether or not the job announces.
				again := current && reported[repo]
				delete(reported, repo)
				if policy {
					run := runs[repo]
					run.at = time.Now()
					runs[repo] = run
				}
				jobsMu.Unlock()
				if again {
					admitPolicy(repo)
				}
				// Hand the freed slot down the queues until a request owns it:
				// first to a Reconcile request turned away while the slots
				// were full, whose consumer is parked on its namespace, then
				// to a policy rebuild. One dropped on admission (its owner's
				// context ended, its header went pending, or its window has
				// not passed) must not strand the rest.
				for {
					jobsMu.Lock()
					var next string
					admit := admitPolicy
					switch {
					case len(turnedAway) > 0:
						next, turnedAway, admit = turnedAway[0], turnedAway[1:], admitTurnedAway
						delete(turnedAwaySet, next)
					case len(queued) > 0:
						next, queued = queued[0], queued[1:]
						delete(queuedSet, next)
					default:
						jobsMu.Unlock()
						return
					}
					jobsMu.Unlock()
					if admit(next) {
						return
					}
				}
			}()
			// A panicking job still releases its slot, lease and waiters above.
			defer func() {
				if recovered := recover(); recovered != nil {
					countContractBaseline(viewmetrics.ContractBaselinePanic)
					if options.Logger != nil {
						options.Logger.Error("contract baseline reconstruction panicked", zap.String("repo", repo), zap.Any("panic", recovered))
					}
				}
			}()
			rebuilt, err := rebuildPrimaryContractBaseline(options.Context, options, repo, policy)
			switch {
			case err != nil:
				countContractBaseline(viewmetrics.ContractBaselineFailed)
			case policy && !rebuilt:
				// Nothing was rewritten: waking the consumers would only repeat
				// the capture that asked.
				announce, current = false, true
				countContractBaseline(viewmetrics.ContractBaselineCurrent)
				if options.Logger != nil {
					options.Logger.Debug("contract baseline policy rebuild found every receipt current", zap.String("repo", repo))
				}
			default:
				countContractBaseline(viewmetrics.ContractBaselinePublished)
			}
			if err != nil && options.Context.Err() == nil && options.Logger != nil {
				options.Logger.Warn("contract baseline reconstruction refused", zap.String("repo", repo), zap.Error(err))
			}
		}()
	}
	// admitReconcileLocked admits a Reconcile job of repo; the caller holds
	// BaselineAdmissionMu and jobsMu. Each outcome ends in a Published for
	// repo, which wakes the consumers parked on its baseline: the job it
	// starts announces one, as does a job it coalesces onto. With both slots
	// taken the request is recorded in turnedAway, and a slot holder's job
	// admits it when it ends: it was recorded while that job held its slot,
	// and the job looks for it only after releasing the slot. A request
	// already recorded stays where it is.
	admitReconcileLocked := func(repo string) {
		if jobs[repo] {
			waiting[repo] = true
			countContractBaseline(viewmetrics.ContractBaselineCoalesced)
			return
		}
		if turnedAwaySet[repo] {
			countContractBaseline(viewmetrics.ContractBaselineCoalesced)
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			turnedAway = append(turnedAway, repo)
			turnedAwaySet[repo] = true
			countContractBaseline(viewmetrics.ContractBaselineSaturated)
			return
		}
		start(repo, false)
	}
	reconcile := func(ctx context.Context, view *graphview.RepoView, repo, checkout string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := runtimeReady(); err != nil {
			return false, err
		}
		if view != nil || checkout != "" {
			return false, nil
		}
		// No raw-source pin gates admission: a never-mutated or interrupted
		// owner has no accepted witness, and refusing on that kept a cold
		// store's consumers waiting on a producer that never started. The job
		// runs its census and every write under the repository lanes and
		// fences on the pending input state instead.
		baseline, err := options.Store.AtGeneration(0).ContractBoundaryReceiptBaselineContext(ctx, repo, "")
		if err != nil {
			return false, err
		}
		if baseline != nil && baseline.Version == contractBoundaryReceiptVersion {
			countContractBaseline(viewmetrics.ContractBaselineCurrent)
			return false, nil
		}
		options.BaselineAdmissionMu.Lock()
		defer options.BaselineAdmissionMu.Unlock()
		if err := options.Context.Err(); err != nil {
			return false, err
		}
		jobsMu.Lock()
		defer jobsMu.Unlock()
		admitReconcileLocked(repo)
		return true, nil
	}
	admitTurnedAway = func(repo string) bool {
		if runtimeReady() != nil || options.Context.Err() != nil {
			return false
		}
		options.BaselineAdmissionMu.Lock()
		defer options.BaselineAdmissionMu.Unlock()
		if options.Context.Err() != nil {
			return false
		}
		jobsMu.Lock()
		defer jobsMu.Unlock()
		admitReconcileLocked(repo)
		return true
	}
	admitPolicy = func(repo string) bool {
		ctx := options.Context
		if runtimeReady() != nil || ctx.Err() != nil || repo == "" {
			return false
		}
		current, found, err := options.Store.AtGeneration(0).ContractInputStateContext(ctx, repo, "")
		if err != nil || !found || !current.Accepted {
			// A pending header is its own baseline's, admitted on the
			// consumer's deferral or by the header repair.
			return false
		}
		options.BaselineAdmissionMu.Lock()
		defer options.BaselineAdmissionMu.Unlock()
		if ctx.Err() != nil {
			return false
		}
		jobsMu.Lock()
		defer jobsMu.Unlock()
		if jobs[repo] || queuedSet[repo] {
			if policyJobs[repo] {
				reported[repo] = true
			}
			countContractBaseline(viewmetrics.ContractBaselineCoalesced)
			return true
		}
		now := time.Now()
		run := contractBaselinePolicyRun{at: now}
		if last, ok := runs[repo]; ok {
			since := now.Sub(last.at)
			if since < last.window() {
				countContractBaseline(viewmetrics.ContractBaselineCurrent)
				if held[repo] == nil {
					var timer *time.Timer
					timer = time.AfterFunc(last.window()-since, func() {
						jobsMu.Lock()
						// A job that started meanwhile, or a newer hold, owns
						// the repository: this firing is spent.
						current := held[repo] == timer
						if current {
							delete(held, repo)
						}
						jobsMu.Unlock()
						if current {
							admitPolicy(repo)
						}
					})
					held[repo] = timer
				}
				return false
			}
			if since < 2*last.window() {
				run.streak = last.streak + 1
			}
		}
		select {
		case slots <- struct{}{}:
		default:
			countContractBaseline(viewmetrics.ContractBaselineSaturated)
			queued = append(queued, repo)
			queuedSet[repo] = true
			return true
		}
		runs[repo] = run
		if options.Logger != nil {
			fields := []zap.Field{zap.String("repo", repo), zap.Int("repeats", run.streak), zap.Duration("next_after", run.window())}
			if run.streak >= 3 {
				options.Logger.Warn("contract receipts keep returning to an earlier extraction policy; the running indexer may not have taken a configuration change", fields...)
			} else {
				options.Logger.Info("contract receipts carry an earlier extraction policy; rebuilding the namespace baseline", fields...)
			}
		}
		start(repo, true)
		return true
	}
	return ContractBaselineReconcilers{Reconcile: reconcile, PolicyChanged: func(repo string) { admitPolicy(repo) }}
}

func reconcilePrimaryContractBaseline(ctx context.Context, options ContractFollowupCaptureOptions, repo string) error {
	_, err := rebuildPrimaryContractBaseline(ctx, options, repo, false)
	return err
}

// rebuildPrimaryContractBaseline reconstructs repo's primary namespace
// baseline and reports whether it rebuilt one. An accepted header over a
// current baseline is left alone unless policy is set and an accepted
// receipt of the census carries an extraction policy other than its file's
// current one (contractBaselinePolicyDrifted): then the namespace is rebuilt
// as a pending one would be. Every file of the census is read again; a
// receipt still current is reused rather than extracted again.
func rebuildPrimaryContractBaseline(ctx context.Context, options ContractFollowupCaptureOptions, repo string, policy bool) (bool, error) {
	end := options.MultiIndexer.beginContractBaseline(repo)
	defer end()
	store := options.Store.AtGeneration(0)
	var pending graph.ContractInputState
	var snapshot ContractFollowupSnapshot
	defer func() {
		if snapshot.Release != nil {
			snapshot.Release()
		}
	}()
	// The census and the pending marker share one lane interval, so no core
	// write of this repository lands between the inventory and the marker.
	err := options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
		return options.MultiIndexer.withContractBaselineOutput(ctx, options.Store, repo, OutputEntryContractBaseline, func() error {
			var err error
			snapshot, err = captureAcceptedContractFiles(ctx, options, nil, []string{repo})
			if err != nil {
				return err
			}
			current, found, err := store.ContractInputStateContext(ctx, repo, "")
			if err != nil {
				return err
			}
			baseline, err := store.ContractBoundaryReceiptBaselineContext(ctx, repo, "")
			if err != nil {
				return err
			}
			if found && current.Accepted && baseline != nil && baseline.Version == contractBoundaryReceiptVersion {
				if !policy {
					return nil
				}
				drifted, err := contractBaselinePolicyDrifted(ctx, options, store, repo, snapshot)
				if err != nil || !drifted {
					return err
				}
			}
			var previous *graph.ContractInputState
			if found {
				previous = &current
			}
			changes := []contractCoreInputChange{{FilePath: repo + "/", Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"namespace_baseline_reconciliation"}}}}}
			pending, _, err = nextContractCoreInputState(previous, repo, "", changes)
			if err != nil {
				return err
			}
			work, err := contractCoreWorkForChanges(0, pending, changes)
			if err != nil {
				return err
			}
			return store.BeginContractInputMutationContext(ctx, previous, pending, work)
		})
	})
	if err != nil {
		return false, err
	}
	if pending.InputFingerprint == "" {
		return false, nil
	}
	digest := sha256.New()
	yield := options.Yield
	for start := 0; start < len(snapshot.Files); start += 64 {
		end := min(start+64, len(snapshot.Files))
		files := snapshot.Files[start:end]
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
		prior, err := store.ContractBoundaryReceiptsForPathsContext(ctx, repo, "", paths)
		if err != nil {
			return false, err
		}
		rows := make([]graph.ContractBoundaryReceipt, 0, len(files))
		reconstructed := make(map[string][]byte, len(files))
		for _, file := range files {
			if err := yield(ctx); err != nil {
				return false, err
			}
			if row := prior[file.Path]; row != nil && row.Accepted && contractReceiptIsFailureEnvelope(row) {
				// An accepted failure envelope stays: the file is excluded, and
				// its envelope is folded as it stands below.
				continue
			}
			accepted, err := snapshot.ReadAccepted(ctx, file)
			if errors.Is(err, graph.ErrContractProjectionStale) {
				// The file moved after the census: its save owns its receipt.
				// The chunk folds whatever receipt stands for it at write time.
				continue
			}
			if err != nil {
				return false, err
			}
			cfg, ok := snapshot.RepoConfigs[repo]
			if !ok {
				return false, fmt.Errorf("contract baseline: accepted configuration unavailable")
			}
			opts, ok := snapshot.RepoExtractionOptions[repo]
			if !ok {
				return false, fmt.Errorf("contract baseline: accepted parser options unavailable")
			}
			idx := &Indexer{config: cfg, registry: options.Registry, repoPrefix: repo, workspaceID: file.WorkspaceID, projectID: file.ProjectID, logger: options.Logger}
			idx.extractionOptions.Store(&opts)
			if reuseContractBaselineOverflowReceipt(idx, file, accepted, prior[file.Path]) {
				idx.Close()
				continue // Fold the standing bounded Unknown row, not empty facts.
			}
			receipt, reused := reuseContractBaselineCurrentReceipt(idx, file, accepted, prior[file.Path])
			var collectErr error
			if !reused {
				receipt, collectErr = collectContractBaselineAcceptedReceipt(ctx, idx, file, accepted)
			}
			idx.Close()
			if collectErr != nil {
				return false, collectErr
			}
			row, err := contractCoreStoredReceipt(repo, "", contractCoreInputChange{FilePath: file.Path, Current: &receipt, SourceFingerprint: accepted.SourceFingerprint})
			if err != nil {
				return false, err
			}
			if row.Scope.Unknown {
				if _, overflow := contractReceiptOverflowPolicy(&row); !overflow {
					return false, fmt.Errorf("contract baseline: incomplete receipt %q", file.Path)
				}
				rows = append(rows, row)
				reconstructed[file.Path] = row.Payload
				continue // Fold the exact bounded source/policy marker.
			}
			rows = append(rows, row)
			receipt.Source = "" // Source bytes certify eligibility, not contract identity.
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return false, err
			}
			reconstructed[file.Path] = encoded
		}
		err = options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
			return options.MultiIndexer.withContractBaselineOutput(ctx, options.Store, repo, OutputEntryContractBaseline, func() error {
				// The fence is the pending input state alone: a key-moving save
				// rewrote it, and only that supersedes the baseline.
				current, found, err := store.ContractInputStateContext(ctx, repo, "")
				if err != nil {
					return err
				}
				if !found || !reflect.DeepEqual(current, pending) {
					return graph.ErrContractProjectionStale
				}
				actual, err := store.ContractBoundaryReceiptsForPathsContext(ctx, repo, "", paths)
				if err != nil {
					return err
				}
				// Per-row CAS: a row a concurrent save rewrote since the chunk
				// read it is skipped and folded as it stands; the save's own
				// acceptance settles it. Only unchanged rows are certified.
				var write []graph.ContractBoundaryReceipt
				for _, row := range rows {
					if reflect.DeepEqual(actual[row.FilePath], prior[row.FilePath]) {
						write = append(write, row)
					}
				}
				for _, path := range paths {
					encoded, rebuilt := reconstructed[path]
					if rebuilt && reflect.DeepEqual(actual[path], prior[path]) {
						_, _ = digest.Write(encoded)
					} else if standing := actual[path]; standing != nil {
						_, _ = digest.Write([]byte("standing:" + standing.Fingerprint + ":" + standing.SourceFingerprint))
					} else {
						_, _ = digest.Write([]byte("absent:" + path))
					}
					_, _ = digest.Write([]byte{0})
				}
				if len(write) == 0 {
					return nil
				}
				if err := store.SetContractBoundaryReceiptsContext(ctx, write); err != nil {
					return err
				}
				return store.AcceptContractBoundaryReceiptsContext(ctx, write)
			})
		})
		if err != nil {
			return false, err
		}
	}
	err = options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
		return options.MultiIndexer.withContractBaselineOutput(ctx, options.Store, repo, OutputEntryContractBaseline, func() error {
			current, found, err := store.ContractInputStateContext(ctx, repo, "")
			if err != nil {
				return err
			}
			if !found || !reflect.DeepEqual(current, pending) {
				return graph.ErrContractProjectionStale
			}
			next := pending
			next.InputFingerprint = contractInputHash([]byte(pending.InputFingerprint + ":" + hex.EncodeToString(digest.Sum(nil))))
			changes := []contractCoreInputChange{{FilePath: repo + "/", Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"namespace_baseline_complete"}}}}}
			work, err := contractCoreWorkForChanges(0, next, changes)
			if err != nil {
				return err
			}
			if err := store.BeginContractInputMutationContext(ctx, &pending, next, work); err != nil {
				return err
			}
			return store.AcceptContractInputMutationWithBaselineContext(ctx, next, graph.ContractBoundaryReceiptBaseline{RepoPrefix: repo, Version: contractBoundaryReceiptVersion, Fingerprint: hex.EncodeToString(digest.Sum(nil))})
		})
	})
	return err == nil, err
}

// contractBaselinePolicyDrifted reports whether an accepted primary receipt
// of the census carries an extraction policy that is neither its file's
// current ordinary policy nor, for a generated parser, its current projected
// one: the receipts a strict capture refuses until they are rebuilt. A
// failure envelope, an unaccepted row and a missing row are left to their
// own repairs.
func contractBaselinePolicyDrifted(ctx context.Context, options ContractFollowupCaptureOptions, store *store_sqlite.Store, repo string, snapshot ContractFollowupSnapshot) (bool, error) {
	cfg, ok := snapshot.RepoConfigs[repo]
	if !ok {
		return false, fmt.Errorf("contract baseline: accepted configuration unavailable")
	}
	opts := snapshot.RepoExtractionOptions[repo]
	idx := &Indexer{config: cfg, registry: options.Registry, repoPrefix: repo, logger: options.Logger}
	idx.extractionOptions.Store(&opts)
	defer idx.Close()
	for start := 0; start < len(snapshot.Files); start += 64 {
		files := snapshot.Files[start:min(start+64, len(snapshot.Files))]
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
		rows, err := store.ContractBoundaryReceiptsForPathsContext(ctx, repo, "", paths)
		if err != nil {
			return false, err
		}
		for _, file := range files {
			row := rows[file.Path]
			if row == nil || !row.Accepted || row.Deleted || contractReceiptIsFailureEnvelope(row) {
				continue
			}
			policy := ""
			if prior, known, err := contractCorePriorReceipt(row, true); err == nil && known && prior != nil {
				policy = prior.Policy
			} else if marker, overflow := contractReceiptOverflowPolicy(row); overflow {
				policy = marker
			}
			if policy == "" || policy == file.Policy {
				continue
			}
			if projected, err := idx.contractFollowupPolicyMode(file.Language, policy); err == nil && projected {
				continue
			}
			return true, nil
		}
	}
	return false, ctx.Err()
}

// beginContractBaseline records a live reconstruction before it asks for a
// repository lane. Overlapping reconstructions each retain their own claim.
func (mi *MultiIndexer) beginContractBaseline(repo string) func() {
	mi.contractBaselineMu.Lock()
	if mi.contractBaselines == nil {
		mi.contractBaselines = make(map[string]int)
	}
	mi.contractBaselines[repo]++
	mi.contractBaselineMu.Unlock()
	return func() {
		mi.contractBaselineMu.Lock()
		defer mi.contractBaselineMu.Unlock()
		mi.contractBaselines[repo]--
		if mi.contractBaselines[repo] == 0 {
			delete(mi.contractBaselines, repo)
		}
	}
}

func (mi *MultiIndexer) contractBaselineActive(repo string) bool {
	mi.contractBaselineMu.Lock()
	defer mi.contractBaselineMu.Unlock()
	return mi.contractBaselines[repo] > 0
}

// Durable core rows include resolver and enrichment output and may be ordered
// differently from the accepted parser result. Reconstruct parser-owned facts
// from the already verified extractor bytes, without indexing or resolving.
func collectContractBaselineAcceptedReceipt(ctx context.Context, idx *Indexer, file ContractFollowupFile, accepted ContractAcceptedSource) (contractBoundaryReceipt, error) {
	if err := ctx.Err(); err != nil {
		return contractBoundaryReceipt{}, err
	}
	ext, ok := idx.registry.GetByLanguage(file.Language)
	if !ok || ext == nil {
		return contractBoundaryReceipt{}, fmt.Errorf("contract baseline: accepted extractor unavailable for %s", file.Language)
	}
	rel := strings.TrimPrefix(file.Path, file.RepoPrefix+"/")
	// A content skip (binary or minified) is a successful read with a nil
	// error; foreground indexing records a receipt for its skip node, so
	// reconstruction folds the same result. Failed skips return an error.
	result, _, err := idx.extractFileCtxWithRawLease(ctx, nil, nil, nil, nil, file.Path, rel, file.Language, ext, accepted.Bytes)
	if result != nil {
		defer result.ReleaseTree()
	}
	if err != nil {
		return contractBoundaryReceipt{}, err
	}
	if result == nil {
		return contractBoundaryReceipt{}, fmt.Errorf("contract baseline: accepted extraction incomplete for %s", file.Path)
	}
	return idx.collectContractBoundaryReceipt(ctx, file.Path, file.Language, accepted.Bytes, result)
}

// withContractBaselineOutput names the contract-only output after its repository
// lane is held. It deliberately avoids core-input journal/source mutation entry:
// the reconstruction's own accepted-source and sidecar CAS fences remain in fn.
// Callers pass the actual runtime Store used for these writes, not a temporary
// AtGeneration handle, and already own the repository mutation lane.
func (mi *MultiIndexer) withContractBaselineOutput(ctx context.Context, output *store_sqlite.Store, repo string, entry OutputMutationEntry, fn func() error) error {
	if fn == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mi == nil || output == nil || output.SymbolSearchViewGeneration() != 0 || repo == "" {
		return fmt.Errorf("%w: contract baseline requires its generation-zero store and repository root", ErrOutputMutationTargetInvalid)
	}
	root := mi.repoRootPath(repo)
	if root == "" {
		return fmt.Errorf("%w: contract baseline requires its repository root", ErrOutputMutationTargetInvalid)
	}
	if entry != OutputEntryContractBaseline {
		return fmt.Errorf("%w: contract baseline entry %q", ErrOutputMutationEntryUnregistered, entry)
	}
	target := legacyOutputTargetFor(outputStoreIdentity(output), repo, root, "")
	target.OwnerKey += "|producer:contract-baseline"
	receipt, err := mi.outputGenerationAuthority().Begin(ctx, entry, target)
	if err != nil {
		return err
	}
	defer receipt.Abandon() // Also settles a panic; idempotent after Complete.
	return runUnderOutputReceipt(receipt, fn)
}
