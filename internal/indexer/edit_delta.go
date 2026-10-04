package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/search"
)

// The per-file delta path: how every working-tree generation is built.
//
// A working-tree generation built this way carries exactly the rows that
// differ from the checkout's current view, and nothing is re-derived that the
// edit cannot change:
//
//  1. the changed files alone are parsed;
//  2. the primary checkout's per-save engine (the same code IndexFile runs:
//     evict and re-add the file's rows, restub or carry the references into
//     it, resolve its outgoing references and the incoming references its new
//     declarations can bind, re-derive the frontier's derived families) runs
//     against a graph.DeltaWriter whose reads are the checkout's composed view
//     and whose writes collect the difference;
//  3. the difference is published as one chained working-tree generation —
//     replace masks and rows for the changed files, edge-source markers for
//     the sources elsewhere whose edge set changed, tombstones for changed
//     pathless identities — through the same manifest, pre-publish fence and
//     publication phases as the sparse build, so the coordinator, freshness
//     and the answer path need no change.
//
// There is no closure walk, no declared-context withholding, no seeding and no
// pass corpus: the view below answers every read the engine makes, so nothing
// has to be parsed to be visible.
//
// Every change set goes through the delta whatever its size: the coordinator
// imports a large working-tree change as a chain of small deltas
// (checkout_import.go), and a caller that hands the builder one large change
// set gets one large delta. Two cases are built by the sparse closure builder
// (Build) instead, and the log says which:
//
//   - a change to a file that decides how the rest of the tree is read (a
//     module manifest, an ignore file, the repository configuration —
//     dependencyManifestPath). The per-save engine re-derives a go.mod
//     differently from a whole index (the dependency contract node's shape,
//     its consumes edge, a module node for a newly required module:
//     TestEditDeltaManifestChangesMatchAWholeIndex), while the sparse build
//     reproduces it;
//   - a delta the DeltaWriter cannot express (a write it refuses, a file that
//     fails to parse, a shared row whose emitter it cannot find within its
//     budget).

// Reasons a working-tree build falls back to the sparse closure builder.
const (
	editDeltaFallbackManifest = "dependency_manifest"
	editDeltaFallbackRefused  = "delta_refused"
)

// buildWorkingTreeLayer builds one working-tree generation through the delta
// path, falling back to the sparse closure builder for a manifest change and
// for a delta the DeltaWriter refused.
func (b *SparseGenerationBuilder) buildWorkingTreeLayer(ctx context.Context, req BuildRequest) (int64, BuildReport, error) {
	prepublish := req.PrePublish
	var reenter func(context.Context, bool) (context.Context, error)
	var leave func()
	req.PrePublish = func(ctx context.Context, generation int64) error {
		if req.prePublishBarrier != nil {
			req.prePublishBarrier()
		}
		for attempt := 0; ; attempt++ {
			if reenter != nil && attempt == 3 {
				fallbackCtx, err := reenter(ctx, true)
				if err != nil {
					return err
				}
				if prepublish != nil {
					if err := prepublish(fallbackCtx, generation); err != nil {
						return err
					}
				}
				reachBuildCommitPoint(fallbackCtx)
				return fallbackCtx.Err()
			}
			// Full sampling (including refresh demand and git admission) stays
			// outside the lane. A retry retains this complete private payload.
			if prepublish != nil {
				if err := prepublish(ctx, generation); err != nil {
					return err
				}
			}
			if reenter == nil {
				return nil
			}
			if _, err := reenter(ctx, false); err != nil {
				return err
			}
			proofCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			confirmed, err := req.prePublishRecheck(proofCtx)
			if proofCtx.Err() != nil {
				confirmed = false
			}
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if confirmed {
				return nil
			}
			leave()
		}
	}

	for _, change := range req.Changes {
		if dependencyManifestPath(change.Path) {
			if req.followup && b.Config.Coverage.IsEnabled("clones") {
				return 0, BuildReport{}, fmt.Errorf("indexer: clone follow-up cannot use the sparse manifest path: %s", change.Path)
			}
			var err error
			ctx, err = resumeImportBuildLane(ctx, true)
			if err != nil {
				return 0, BuildReport{}, err
			}
			b.logEditDeltaFallback(req, editDeltaFallbackManifest+": "+change.Path)
			return b.Build(ctx, req)
		}
	}
	if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil && lane.gate != nil {
		epochs, eligible, err := b.importPreparationEpochs(ctx, req)
		if err != nil {
			return 0, BuildReport{}, err
		}
		if eligible {
			// begin releases the lane before waiting for the preparation slot.
			release, err := lane.begin(ctx)
			if err != nil {
				return 0, BuildReport{}, err
			}
			defer release()
			leave = lane.leave
			reenter = func(ctx context.Context, yieldable bool) (context.Context, error) {
				var err error
				ctx, err = lane.reenter(ctx, yieldable)
				if err != nil {
					return ctx, err
				}
				return ctx, b.checkImportPreparationEpochs(epochs)
			}
		} else {
			ctx, err = lane.reenter(ctx, true)
			if err != nil {
				return 0, BuildReport{}, err
			}
		}
	}
	generationID, report, err := b.buildEditDelta(ctx, req)
	var refused *editDeltaRefusedError
	if err != nil && errors.As(err, &refused) {
		if req.followup && b.Config.Coverage.IsEnabled("clones") {
			return generationID, report, err
		}
		if reenter != nil {
			var admissionErr error
			ctx, admissionErr = reenter(ctx, true)
			if admissionErr != nil {
				return generationID, report, admissionErr
			}
		}
		// Sparse fallback owns a bulk window: it must never leave the lane.
		reenter, leave = nil, nil
		b.logEditDeltaFallback(req, editDeltaFallbackRefused+": "+refused.reason)
		return b.Build(ctx, req)
	}
	// A ready generation or a coalesced flight bypasses PrePublish. The
	// coordinator still reenters before it changes the route to that payload.
	if reenter != nil && ctx.Err() == nil {
		_, admissionErr := reenter(ctx, false)
		if err == nil {
			err = admissionErr
		}
	}
	if reenter != nil && errors.Is(err, ErrDirtySnapshotChanged) {
		err = fmt.Errorf("%w: %w", errImportPreparationChanged, err)
	}
	return generationID, report, err
}

func (b *SparseGenerationBuilder) logEditDeltaFallback(req BuildRequest, reason string) {
	if b.Logger == nil {
		return
	}
	b.Logger.Info("indexer: working-tree edit built by the sparse closure builder",
		zap.String("checkout", req.Identity.CheckoutID),
		zap.String("reason", reason),
		zap.Int("changes", len(req.Changes)))
}

// editDeltaRefusedError is a delta that could not be expressed. Nothing was
// published; the caller builds the same state with the sparse builder.
type editDeltaRefusedError struct{ reason string }

func (e *editDeltaRefusedError) Error() string {
	return "indexer: the edit cannot be published as a delta: " + e.reason
}

// EditDeltaReport is what the delta path did, beside the ordinary build
// report: how much of the view it had to copy to express the difference, and
// how much of the difference it published.
type EditDeltaReport struct {
	Paths             []string
	CoveredPaths      int
	ClaimedSources    int
	MaterializedNodes int
	MaterializedEdges int
	// EdgeClaims / EdgeClaimsPromoted are the edge-level claims a file
	// eviction made and the sources of them claimed whole at Payload;
	// ClaimsByWrite attributes the whole-source claims to their writes
	// (graph.DeltaWriterStats).
	EdgeClaims         int
	EdgeClaimsPromoted int
	ClaimsByWrite      map[string]graph.ClaimCount
	PayloadNodes       int
	PayloadEdges       int
	ReplacePaths       int
	DeletePaths        int
	EdgeSources        int
	Tombstones         int
	DroppedPaths       int
	DroppedSources     int
	DroppedNodes       int
	OrphanEdges        int
	IdentityClaims     int
	// RestatedNodes / RestatedEdges are the rows at replaced paths identical
	// to the view below's (DeltaPayload).
	RestatedNodes int
	RestatedEdges int
	// SharedRowEmitters are the unchanged files the delta re-derived because
	// a shared registry row's kept copy moved to them.
	SharedRowEmitters []string
	// DependentsWalked reports that planning the dependents ran the closure
	// walk, which extracts the changed files again (a file was deleted);
	// false when the plan was read from the pass's own rows
	// (editDeltaDependents).
	DependentsWalked bool
	// PayloadRows attributes the payload's rows by owner and edge kind, and
	// PayloadSteps times Payload's steps (graph.DeltaPayload).
	PayloadRows  graph.DeltaPayloadRows
	PayloadSteps map[string]float64
	// PayloadStepLayerRows / PhaseLayerRows are the rows read from the
	// layers below per Payload step and per delta phase; LayerRowsByRead
	// splits the delta's total by the read that composed them.
	PayloadStepLayerRows map[string]int
	PhaseLayerRows       map[string]int
	LayerRowsByRead      map[string]int
	BelowRowsServed      int
	// Dependents are the unchanged files the delta re-derived because the
	// change can move their own rows (editDeltaDependents).
	Dependents []string
	// EnrichmentRestated counts edges the enrichment stage wrote outside the
	// delta's ownership that restated a row the view below serves, removed
	// before publication (edit_delta_enrich.go).
	EnrichmentRestated int
	// EnrichmentNodeClaims counts nodes the enrichment stage wrote outside the
	// delta's ownership whose identity the view below serves, published under
	// an identity replacement claim (edit_delta_enrich.go).
	EnrichmentNodeClaims int
	// PageFaults / BlockReads are the process's major page faults and block
	// reads while the delta ran (getrusage; the whole process, so concurrent
	// work is included): the store pages the delta had to bring in.
	PageFaults int64
	BlockReads int64
	// ResolveFrontier / ResolveDuration / ResolveFaults describe the resolver
	// catch-up inside the pass: the files it resolved, its wall time and the
	// major page faults the process took meanwhile.
	// PhaseFaults are the major page faults per delta phase (Phases).
	PhaseFaults map[string]int64
	// PhaseCPU is the process CPU (every goroutine's, RUSAGE_SELF) and
	// PhaseReaderWait the store readers' gate and pool wait during each
	// phase. A phase whose wall is close to PhaseCPU was working; one whose
	// wall far exceeds both was waiting for the processor or on I/O.
	PhaseCPU        map[string]time.Duration
	PhaseReaderWait map[string]time.Duration
	// PhaseStoreWaits is each phase's store waits by kind (the writer pool,
	// SQLite's busy and WAL-retry sleeps, read-transaction time).
	PhaseStoreWaits map[string]store_sqlite.ReaderWaitSplit
	// PhaseSchedWait / PhaseSchedWaits are the runnable waits (for a
	// processor) that ended during each phase, process-wide: their estimated
	// total and their count (sched_latency_mark.go).
	PhaseSchedWait  map[string]time.Duration
	PhaseSchedWaits map[string]int64
	// SleepGaugeInstalled is the store's SQLite sleep gauge state at the
	// delta: a zero busy or WAL-retry sleep is a measured zero only when set.
	SleepGaugeInstalled bool
	// ChainLayersOverlaid is how many dirty-chain layers the delta composed
	// per read over the per-stack caches kept for the stack below them.
	ChainLayersOverlaid int
	// ChainKeeper is the per-layer keeper's counters after the delta:
	// layers, rows, hits, loads, declined.
	ChainKeeper [5]int
	// InertRestated is how many change-set files had an inert save and keep
	// the rows the view below holds.
	InertRestated int
	// AffectedByKeys is how many declarations of the changed files the
	// affected-by plan found changed in shape, AffectedByKeySample the first
	// of them, and AffectedByFiles how many referrer files it re-resolved.
	AffectedByKeys      int
	AffectedByKeySample []string
	AffectedByFiles     int
	// carryRegistry files the delta's final contract registry under the
	// published generation's stack (edit_delta_contract_cache.go); nil when
	// the delta's registry was not keyed.
	carryRegistry        func(generation int64)
	contractInputWitness *store_sqlite.PayloadInputWitness
	// StackCacheKey is the key the delta's per-stack caches were kept under
	// (edit_delta_contract_cache.go), empty when the stack below has none.
	StackCacheKey string

	// PhaseWALBytes / PhaseWriteTx are the WAL bytes appended and the write
	// transactions begun during each phase (process-wide, as PageFaults).
	// ContractRegistryCached reports that the delta's contract registry came
	// from the cache kept across deltas instead of a read of the view below.
	ContractRegistryCached bool
	PhaseWALBytes          map[string]int64
	PhaseWriteTx           map[string]int64
	ResolveFrontier        int
	ResolveDuration        time.Duration
	ResolveFaults          int64
	// WholeLayerLoads / WholeLayerRows: layers below read wholesale
	// (graph.DeltaWriterStats).
	WholeLayerLoads int
	// DeclarationDiff is the declaration-level diff of the changed files and
	// whether the save would qualify for a row-level form.
	DeclarationDiff editDeltaDeclarationDiff
	// StackPathNodeHits / StackPathNodeMisses are this delta's per-path
	// file-node reads served from, and loaded into, the stack's cache.
	StackPathNodeHits   int
	StackPathNodeMisses int
	WholeLayerRows      int
	LayerRowsRead       int
	SlowReads           map[string]int
	Phases              []GenerationPhase

	ownership editDeltaOwnership
}

// lastEditDeltaReport is the most recent delta report, for tests and the
// harness.
var (
	lastEditDeltaMu     sync.Mutex
	lastEditDeltaReport *EditDeltaReport
)

func recordLastEditDelta(r *EditDeltaReport) {
	lastEditDeltaMu.Lock()
	lastEditDeltaReport = r
	lastEditDeltaMu.Unlock()
}

// LastEditDeltaReport returns the most recent delta build's report, nil when
// none ran in this process.
func LastEditDeltaReport() *EditDeltaReport {
	lastEditDeltaMu.Lock()
	defer lastEditDeltaMu.Unlock()
	return lastEditDeltaReport
}

// buildEditDelta is the delta path's physical build: the same catalog
// reservation, flight, publication phases and cleanup as the sparse builder's
// buildReservedGenerationWithCallbacks, with the pass and the mask derivation
// replaced by the delta.
func (b *SparseGenerationBuilder) buildEditDelta(ctx context.Context, req BuildRequest) (int64, BuildReport, error) {
	started := time.Now()
	if err := b.validate(ctx, &req); err != nil {
		return 0, BuildReport{}, err
	}
	work := newGenerationWorkCounters(req)
	planningStarted := time.Now()
	plan, report, err := editDeltaPlan(ctx, req)
	report.PlanningDuration = time.Since(planningStarted)
	if err != nil {
		return 0, report, err
	}
	report.Work = work
	work.recordPlan(plan, report, report.PlanningDuration)
	markPublicationPhase(ctx, PublicationPlanned)

	generationID, handle, adopted, err := b.Store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: req.Identity.OwnerKind, GraphID: req.Identity.GraphID,
		LayerID: req.Identity.LayerID, CheckoutID: req.Identity.CheckoutID,
		GenerationKind: req.Identity.GenerationKind, BaseGenerationID: req.Identity.BaseGenerationID,
		LowerViewFingerprint: req.Identity.LowerViewFingerprint, TreeOID: req.Identity.TreeOID,
		ProvenanceCommitOID: req.Identity.ProvenanceCommitOID, ConfigHash: req.Identity.ConfigHash,
		ExtractorVersions: req.Identity.ExtractorVersions, ResolverVersion: req.Identity.ResolverVersion,
		DependencyRevision: req.Identity.DependencyRevision,
		CreatedAt:          req.Identity.CreatedAt,
	})
	if err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: begin payload generation: %w", err)
	}
	report.GenerationID = generationID
	flight, leader, ready, err := b.Store.JoinPayloadBuildFlight(ctx, generationID, adopted)
	if err != nil {
		report.Coalesced = adopted
		report.Duration = time.Since(started)
		return generationID, report, fmt.Errorf("indexer: join payload build flight %d: %w", generationID, err)
	}
	if ready {
		report.Coalesced = true
		report.Duration = time.Since(started)
		return generationID, report, nil
	}
	if !leader {
		report.Coalesced = true
		err = flight.Wait(ctx)
		report.Duration = time.Since(started)
		return generationID, report, err
	}
	runtimeactivity.Begin(sparseGenerationBuildActivity)
	defer runtimeactivity.End(sparseGenerationBuildActivity)
	report.Coalesced = false
	report.Work.startPhases()
	var buildErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			flight.Complete(fmt.Errorf("indexer: payload generation %d build panicked: %v", generationID, recovered))
			panic(recovered)
		}
		flight.Complete(buildErr)
	}()
	buildErr = func() (physicalErr error) {
		published := false
		defer func() {
			if !published {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), generationAbandonTimeout)
				defer cancel()
				b.abandon(cleanupCtx, generationID)
			}
		}()
		var cloneProjection *cloneFollowupProjection
		delta, err := b.runEditDelta(ctx, req, plan, handle, &report, &cloneProjection)
		if err != nil {
			return err
		}
		recordLastEditDelta(delta)
		markPublicationPhase(ctx, PublicationExtracted)
		if err := ctx.Err(); err != nil {
			return err
		}
		if lane, _ := ctx.Value(importBuildLaneKey{}).(*importBuildLane); lane != nil && lane.detached && !b.importHandleEnrichmentReady(ctx, req, handle) {
			var err error
			ctx, err = lane.reenter(ctx, true)
			if err != nil {
				return err
			}
		}
		enrichWAL, enrichTx, enrichIO, enrichStarted := handle.WALWriteMark(), store_sqlite.WriteTransactionsBegun(), editDeltaProcessIO(), time.Now()
		enrichCPU, enrichStore := processCPUTime(), b.storeWaitMark()
		b.runEnrichment(ctx, req, handle, &report)
		enrichStageMs := float64(time.Since(enrichStarted).Microseconds()) / 1000
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(report.Enrichment.Ran) > 0 {
			delta.EnrichmentRestated = editDeltaSettleEnrichment(handle, req.Base, delta.ownership)
			claimed, err := editDeltaClaimEnrichedNodes(handle, req.Base, delta.ownership)
			if err != nil {
				return fmt.Errorf("indexer: claim enrichment-restated nodes: %w", err)
			}
			delta.EnrichmentNodeClaims = claimed
		}
		if cloneProjection != nil {
			projectStarted := time.Now()
			detached, err := cloneProjection.apply(ctx, handle, req.Base)
			if err != nil {
				return fmt.Errorf("indexer: project clone follow-up: %w", err)
			}
			// Enrichment and detached identity overrides can add rows after the
			// delta's payload count was stamped. Reconcile only this successful
			// follow-up's per-repo count; pass/payload counters remain unchanged.
			counts, err := handle.ScanRepoMemoryEstimates(ctx)
			if err != nil {
				return fmt.Errorf("indexer: count clone follow-up repo rows: %w", err)
			}
			report.NodeCount = counts[req.RepoPrefix].NodeCount
			state, found, err := handle.GetRepoIndexState(req.RepoPrefix)
			if err != nil {
				return fmt.Errorf("indexer: read clone follow-up index state: %w", err)
			}
			if found {
				state.NodeCount = report.NodeCount
				if err := handle.SetRepoIndexState(state); err != nil {
					return fmt.Errorf("indexer: count clone follow-up repo rows: %w", err)
				}
			}
			if b.Logger != nil {
				b.Logger.Info("indexer: clone follow-up projection",
					zap.Int("signature_rows", len(cloneProjection.rows)),
					zap.Int("detached_nodes", detached),
					zap.Int("repo_nodes", report.NodeCount),
					zap.Float64("ms", float64(time.Since(projectStarted).Microseconds())/1000))
			}
			report.Work.mark("clone_followup_projection")
			report.cloneFollowupComplete = true
			cloneProjection = nil
		}
		if b.Logger != nil {
			b.Logger.Info("indexer: working-tree edit delta enrichment",
				zap.Strings("paths", delta.Paths),
				zap.Strings("ran", report.Enrichment.Ran),
				zap.Float64("ms", float64(time.Since(enrichStarted).Microseconds())/1000),
				// stage_ms is the enrichment stage itself (the go/types load
				// and apply, each with its own faults, CPU and store I/O on
				// the provider's lines); the rest of ms settles and claims
				// the stage's rows.
				zap.Float64("stage_ms", enrichStageMs),
				zap.Float64("cpu_ms", float64((processCPUTime()-enrichCPU).Microseconds())/1000),
				zap.Any("store_io", storeWaitMillis(b.storeWaitMark().Split(enrichStore))),
				zap.Int64("major_faults", editDeltaProcessIO().since(enrichIO).majorFaults),
				zap.Int64("wal_bytes", store_sqlite.WALWrittenBetween(enrichWAL, handle.WALWriteMark()).Bytes),
				zap.Int64("write_tx", store_sqlite.WriteTransactionsBegun()-enrichTx),
				zap.Int("restated", delta.EnrichmentRestated),
				zap.Int("node_claims", delta.EnrichmentNodeClaims))
		}
		report.Work.mark("enrich")
		markPublicationPhase(ctx, PublicationSemanticDone)
		if err := b.declareProducers(req, handle, &report); err != nil {
			return err
		}
		if req.inputManifest != nil {
			if err := handle.WriteInputManifest(ctx, req.inputManifest.meta, req.inputManifest.entries); err != nil {
				return fmt.Errorf("indexer: write input manifest for generation %d: %w", generationID, err)
			}
			report.ManifestEntriesWritten = len(req.inputManifest.entries)
		}
		report.Work.mark("separate_masks_producers")
		if req.PrePublish != nil {
			if err := b.measurePrepublish(&report, func() error {
				return req.PrePublish(withBuildReadSet(ctx, plan.indexed, nil, plan.deleted), generationID)
			}); err != nil {
				return err
			}
		}
		report.Work.mark("prepublish")
		reachBuildCommitPoint(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := stampBuiltGeneration(ctx, handle); err != nil {
			return err
		}
		markPublicationPhase(ctx, PublicationPayloadFlushed)
		var publishErr error
		if delta.contractInputWitness != nil {
			publishErr = b.Store.PublishPayloadGenerationWithInputWitness(ctx, generationID, time.Now().Unix(), delta.contractInputWitness)
		} else {
			publishErr = b.Store.PublishPayloadGeneration(ctx, generationID, time.Now().Unix())
		}
		if errors.Is(publishErr, store_sqlite.ErrPayloadInputChanged) {
			if b.Logger != nil {
				b.Logger.Info("indexer: contract input publication refused", zap.Int64("generation", generationID), zap.String("reason", "selected_input_changed"))
			}
			return fmt.Errorf("%w: %w: contract inputs changed before publication", ErrDirtySnapshotChanged, errContractInputsChanged)
		}
		if err := publishErr; err != nil {
			return fmt.Errorf("indexer: publish generation %d: %w", generationID, err)
		}
		if delta.carryRegistry != nil {
			delta.carryRegistry(generationID)
		}
		report.Work.mark("publish")
		markPublicationPhase(ctx, PublicationPublished)
		published = true
		return nil
	}()
	report.Work.finish(b.Store, generationID, &report)
	report.Duration = time.Since(started)
	return generationID, report, buildErr
}

// editDeltaPlan is the change set as the delta path sees it: the present
// paths (parsed) and the deleted ones (evicted), with no closure.
func editDeltaPlan(ctx context.Context, req BuildRequest) (buildPlan, BuildReport, error) {
	var report BuildReport
	deleted := make(map[string]struct{})
	present := make(map[string]struct{})
	for _, change := range req.Changes {
		if err := ctx.Err(); err != nil {
			return buildPlan{}, report, err
		}
		clean := path.Clean(change.Path)
		switch change.Kind {
		case LayerPathDeleted:
			report.DeletedFiles++
			deleted[clean] = struct{}{}
		case LayerPathAdded:
			report.AddedFiles++
			present[clean] = struct{}{}
		case LayerPathModified:
			report.ChangedFiles++
			present[clean] = struct{}{}
		}
	}
	for p := range present {
		delete(deleted, p)
		if _, err := req.Target.Stat(p); err != nil {
			return buildPlan{}, report, fmt.Errorf(
				"indexer: change set names %q as present but the target source does not hold it: %w", p, err)
		}
	}
	var plan buildPlan
	for p := range present {
		plan.indexed = append(plan.indexed, p)
	}
	for p := range deleted {
		plan.deleted = append(plan.deleted, p)
	}
	sort.Strings(plan.indexed)
	sort.Strings(plan.deleted)
	report.DeletedFiles = len(plan.deleted)
	for _, p := range plan.indexed {
		if meta, err := req.Target.Stat(p); err == nil && meta.Size > 0 {
			report.SourceBytes += meta.Size
		}
	}
	report.IndexedPaths = plan.indexed
	return plan, report, nil
}

// runEditDelta runs the per-save engine over a DeltaWriter on the view below
// and writes the resulting difference into the generation.
func (b *SparseGenerationBuilder) runEditDelta(
	ctx context.Context,
	req BuildRequest,
	plan buildPlan,
	handle *store_sqlite.Store,
	report *BuildReport,
	cloneProjection **cloneFollowupProjection,
) (*EditDeltaReport, error) {
	// A background stack pre-warm yields while a delta runs.
	defer editDeltaBegin()()
	out := &EditDeltaReport{}
	clock := newPhaseClock(&out.Phases)
	ioStarted := editDeltaProcessIO()
	ioLast := ioStarted
	out.PhaseFaults = make(map[string]int64)
	out.PhaseCPU = make(map[string]time.Duration)
	out.PhaseReaderWait = make(map[string]time.Duration)
	out.PhaseStoreWaits = make(map[string]store_sqlite.ReaderWaitSplit)
	out.PhaseSchedWait = make(map[string]time.Duration)
	out.PhaseSchedWaits = make(map[string]int64)
	schedLast := readSchedMark()
	storeMark := func() store_sqlite.ReaderWaitMark {
		if b.Store == nil {
			return store_sqlite.ReaderWaitMark{}
		}
		return b.Store.ReaderWaitMark()
	}
	cpuLast, storeLast := processCPUTime(), storeMark()
	out.PhaseWALBytes = make(map[string]int64)
	out.PhaseWriteTx = make(map[string]int64)
	out.PhaseLayerRows = make(map[string]int)
	walLast, txLast := handle.WALWriteMark(), store_sqlite.WriteTransactionsBegun()
	layerRowsLast := 0
	var layerRowsOf func() int
	lap := func(name string) {
		clock.lap(name)
		if layerRowsOf != nil {
			if rows := layerRowsOf(); rows > layerRowsLast {
				out.PhaseLayerRows[name] += rows - layerRowsLast
				layerRowsLast = rows
			}
		}
		now := editDeltaProcessIO()
		out.PhaseFaults[name] += now.since(ioLast).majorFaults
		ioLast = now
		cpuNow, storeNow := processCPUTime(), storeMark()
		out.PhaseCPU[name] += cpuNow - cpuLast
		split := storeNow.Split(storeLast)
		out.PhaseReaderWait[name] += split.Gate + split.ReadPool
		acc := out.PhaseStoreWaits[name]
		acc.Gate += split.Gate
		acc.ReadPool += split.ReadPool
		acc.WriterPool += split.WriterPool
		acc.BusySleep += split.BusySleep
		acc.WALRetrySleep += split.WALRetrySleep
		acc.BusySleeps += split.BusySleeps
		acc.WALRetries += split.WALRetries
		acc.ReadTxn += split.ReadTxn
		acc.ReadTxns += split.ReadTxns
		acc.CacheHits += split.CacheHits
		acc.CacheMisses += split.CacheMisses
		acc.CacheSpills += split.CacheSpills
		acc.MappedPages += split.MappedPages
		acc.WriterCacheHits += split.WriterCacheHits
		acc.WriterCacheMisses += split.WriterCacheMisses
		acc.WriterCacheSpills += split.WriterCacheSpills
		acc.VFS = addVFSIOSplit(acc.VFS, split.VFS)
		out.PhaseStoreWaits[name] = acc
		cpuLast, storeLast = cpuNow, storeNow
		schedNow := readSchedMark()
		waits, waited := schedNow.since(schedLast)
		out.PhaseSchedWait[name] += waited
		out.PhaseSchedWaits[name] += waits
		schedLast = schedNow
		out.SleepGaugeInstalled = storeNow.Sleep.Installed
		walNow, txNow := handle.WALWriteMark(), store_sqlite.WriteTransactionsBegun()
		if wal := store_sqlite.WALWrittenBetween(walLast, walNow); wal.Bytes > 0 {
			out.PhaseWALBytes[name] += wal.Bytes
		}
		if tx := txNow - txLast; tx > 0 {
			out.PhaseWriteTx[name] += tx
		}
		walLast, txLast = walNow, txNow
	}
	// Every dependency-frontier decision, including inert/metadata reuse and
	// changed-record fallback, can consume accepted ancestor inputs. Capture
	// once before reading those inputs and retain the original through every
	// engine pass and the guarded final publication.
	var contractInputWitness *store_sqlite.PayloadInputWitness
	if generations, complete := editDeltaContractInputGenerations(req.Base, b.Store); complete && b.contractCoreRuntime.Load() == nil {
		var err error
		contractInputWitness, err = b.Store.CapturePayloadInputWitness(ctx, generations)
		if err != nil {
			return out, fmt.Errorf("indexer: capture contract inputs: %w", err)
		}
		// A materialized reader can predate capture. An empty checked read
		// validates every layer's original construction revision without
		// selecting rows; publication then guards movement after capture.
		if _, err := graph.ConstantValuesByNodeIDsContext(ctx, req.Base, nil); err != nil {
			if errors.Is(err, graph.ErrConstantProjectionStale) || errors.Is(err, graph.ErrContractProjectionStale) {
				return out, fmt.Errorf("%w: %w: %w", ErrDirtySnapshotChanged, errContractInputsChanged, err)
			}
			return out, fmt.Errorf("indexer: validate contract input reader: %w", err)
		}
	}
	dw := graph.NewDeltaWriter(req.Base, handle)
	layerRowsOf = func() int { return dw.DeltaStats().LayerRowsRead }
	// The per-stack caches (edit_delta_contract_cache.go). Over a dirty chain
	// they are kept for the stack below it and the delta composes the chain's
	// layers per read; keyBase is the base every key below is taken from, and
	// cacheBase the view the caches are loaded from.
	keyBase, cacheBase := installEditDeltaStackCache(dw, req.Base, b.Store)
	out.ChainLayersOverlaid = dw.ChainLayers()
	// The payload's comparisons read the view below's rows at the changed
	// paths once per stack (edit_delta_below_rows.go).
	if key, ok := editDeltaBaseCacheKey(keyBase, b.Store); ok {
		source := editDeltaBelowRowsSource{key: key, base: cacheBase}
		if dw.ChainLayers() > 0 {
			source.chain = dw
		}
		dw.SetBelowFileRows(source)
	}
	var baseCache *graph.BaseProjectionCache
	var baseFileHits, baseFileMisses, baseImportHits, baseImportMisses int
	var pathNodeHits, pathNodeMisses int
	if key, ok := editDeltaBaseCacheKey(keyBase, b.Store); ok {
		out.StackCacheKey = key
		baseCache = editDeltaBaseCache(key)
		baseFileHits, baseFileMisses, baseImportHits, baseImportMisses = baseCache.Stats()
		pathNodeHits, pathNodeMisses = baseCache.StackFileNodeStats()
	}
	chainTouched := dw.ChainTouchedPaths()
	idx := New(dw, b.Registry, b.Config, b.Logger)
	idx.contractGenerationID = handle.ViewGeneration()
	idx.contractProjectionContext = ctx
	idx.contractProjectionNeedsWitness = true
	idx.contractInputWitness = contractInputWitness
	if err := b.installSelectedContractCoreInputs(ctx, idx, handle, req); err != nil {
		idx.Close()
		return out, err
	}
	idx.cloneRecompute = cloneRecomputePaths(req.RepoPrefix, req.RecomputeDerivedPaths)
	defer idx.Close()
	idx.headProvenance = req.headProvenance
	if store := b.Store; store != nil {
		idx.storeWaits = store.ReaderWaitMark
	}
	// The project name shapes every symbol search document (a whole index
	// detects it from the root it walks); the per-save engine never walks.
	if absRoot, err := filepath.Abs(req.RootPath); err == nil {
		idx.projectName = search.DetectProjectName(absRoot)
	}
	idx.SetRepoPrefix(req.RepoPrefix)
	// The resolver catch-up logs its own legs (frontier collection, pass
	// indexes, outgoing and incoming resolution) when given a logger.
	if b.Logger != nil && idx.resolver != nil {
		idx.resolver.SetLogger(b.Logger)
	}
	idx.SetWorkspaceID(req.WorkspaceID)
	idx.SetProjectID(req.ProjectID)
	if b.Embedder != nil {
		idx.SetEmbedder(b.Embedder)
	}
	if b.Admissions != nil {
		idx.shadowAdmission = b.Admissions.shadowAdmission
		idx.indexMemoryAdmission = b.Admissions.indexMemoryAdmission
		idx.parseAdmission.Store(b.Admissions.parseAdmission.Load())
		idx.nativeParseAdmission.Store(b.Admissions.nativeParseAdmission.Load())
	}
	lap("open")
	// The repository's contract registry as the view below holds it, kept
	// across deltas over the same immutable stack (edit_delta_contract_cache.go).
	// The registry is kept for the whole stack below the delta, the chain
	// included: it is not composed per read (edit_delta_contract_cache.go).
	if key, ok := editDeltaRegistryKey(keyBase, b.Store, req.RepoPrefix, req.WorkspaceID, req.ProjectID); ok && idx.contractCoreInputs == nil {
		idx.contractRegistrySeed = func() {
			out.ContractRegistryCached = seedEditDeltaContractRegistry(idx, key)
		}
	}
	if req.headProvenance != nil && idx.contractCoreInputs == nil {
		idx.priorContractInputs = idx.verifiedHeadContractInputs(req.RootPath, req.headProvenance.sha)
	}
	// Prior rows without fingerprints are given their HEAD content's
	// (edit_delta_prior_fingerprints.go).
	if key, ok := editDeltaContractCacheKey(keyBase, b.Store, req.RepoPrefix, "", ""); ok && req.headProvenance != nil {
		if absRoot, err := filepath.Abs(req.RootPath); err == nil {
			idx.priorFingerprints = headPriorFingerprintsExcept(idx, absRoot, req.headProvenance.sha, key, func(rel string) bool {
				_, touched := chainTouched[idx.prefixPath(rel)]
				return touched
			})
		}
	}
	// The changed files' prior adjacency, kept the same way
	// (edit_delta_prior_view.go).
	if key, ok := editDeltaBaseCacheKey(keyBase, b.Store); ok {
		setPriorEdgesSource(dw, editDeltaPriorEdges(dw, key))
		defer setPriorEdgesSource(dw, nil)
	}
	// The stack's dependency-module contracts, kept the same way
	// (edit_delta_dep_contracts.go).
	if key, ok := editDeltaBaseCacheKey(keyBase, b.Store); ok {
		installEditDeltaDeps(idx, key, append(append([]string(nil), plan.indexed...), plan.deleted...), chainTouched)
	}
	// The stack's callee parameter index (the dataflow pass), kept the same
	// way (edit_delta_param_index.go); a path the chain speaks for is read
	// like the change set's.
	if key, ok := editDeltaBaseCacheKey(keyBase, b.Store); ok {
		changed := make([]string, 0, len(plan.indexed)+len(plan.deleted))
		for _, rel := range append(append([]string(nil), plan.indexed...), plan.deleted...) {
			changed = append(changed, idx.prefixPath(rel))
		}
		idx.dataflowParams = newEditDeltaParamIndexOver(dw, key, changed)
	}
	// The stack's provides rows (the resolver's DI index), kept the same way
	// (edit_delta_provides.go).
	if key, ok := editDeltaBaseCacheKey(keyBase, b.Store); ok {
		installEditDeltaProvides(idx, cacheBase, key, append(append([]string(nil), plan.indexed...), plan.deleted...))
	}
	// The stack's Go file inventory behind package ownership, kept the same
	// way (edit_delta_go_ownership.go); read before the delta writes.
	if key, ok := editDeltaContractCacheKey(keyBase, b.Store, req.RepoPrefix, "", ""); ok {
		if primeEditDeltaGoOwnership(idx, key, append(append([]string(nil), plan.indexed...), plan.deleted...)) != nil {
			lap("go_ownership_inventory")
		}
	}

	absPaths := make([]string, 0, len(plan.indexed)+len(plan.deleted))
	for _, rel := range plan.indexed {
		absPaths = append(absPaths, filepath.Join(req.RootPath, filepath.FromSlash(rel)))
	}
	for _, rel := range plan.deleted {
		absPaths = append(absPaths, filepath.Join(req.RootPath, filepath.FromSlash(rel)))
	}
	out.Paths = append(append([]string(nil), plan.indexed...), plan.deleted...)
	// Every changed file is re-derived from source even when its stored
	// fingerprints call the save inert: the generation claims each changed
	// path and must carry its complete rows and side tables (the files row's
	// content hash among them), which an inert save leaves unwritten.
	// The resolver catch-up is the engine's largest phase with no log of its
	// own; the delta times it and counts the pages it brings in.
	idx.incrementalResolveFilesHook = func(frontier []string) {
		started, io := time.Now(), editDeltaProcessIO()
		idx.resolver.ResolveFilesAndIncoming(frontier)
		used := editDeltaProcessIO().since(io)
		out.ResolveFrontier += len(frontier)
		out.ResolveDuration += time.Since(started)
		out.ResolveFaults += used.majorFaults
	}
	// Each engine pass is split into the stages the engine announces
	// (observeIncrementalCatchup): the reconcile before the resolver, then
	// resolve, dataflow, affected_by, ref_facts, semantic and derived, each a
	// phase with its own fault count. passStage names the running one.
	passPrefix, passStage := "pass", "reconcile"
	idx.affectedByDeltaHook = func(_ string, keys []string) {
		out.AffectedByKeys += len(keys)
		for _, key := range keys {
			if len(out.AffectedByKeySample) < 20 {
				// The plan's keys join kind and name with a NUL byte; the
				// sample goes to logs and reports, so it is rendered as text.
				out.AffectedByKeySample = append(out.AffectedByKeySample, strings.ReplaceAll(key, "\x00", ":"))
			}
		}
	}
	idx.incrementalCatchupHook = func(kind string, files []string) {
		if kind == "affected_by" {
			out.AffectedByFiles += len(files)
		}
		switch kind {
		case "resolve", "dataflow", "affected_plan", "affected_by", "ref_facts", "semantic", "derived":
		default:
			return
		}
		lap(passPrefix + "_" + passStage)
		passStage = kind
	}
	endPass := func(next string) {
		lap(passPrefix + "_" + passStage)
		passPrefix, passStage = next, "reconcile"
	}
	// The change set is re-derived from source whatever its fingerprints
	// say, and its prior resolutions stay reusable (the shape-keyed reuse and
	// the prior-binding carry): nothing but the files themselves changed.
	idx.inertReparsed = make(map[string]struct{})
	idx.reparseKeepingResolutions = make(map[string]struct{}, len(plan.indexed))
	for _, rel := range plan.indexed {
		idx.reparseKeepingResolutions[filepath.Clean(filepath.Join(req.RootPath, filepath.FromSlash(rel)))] = struct{}{}
	}
	// An empty change set (the working tree is back at the state below) is
	// an empty delta. The engine is not run for it: given no path, its
	// scoped discovery would take the whole root as the scope.
	var result *IndexResult
	var err error
	if len(absPaths) > 0 {
		result, err = idx.incrementalWatcherPaths(req.RootPath, absPaths, incrementalPathMode{
			detectDeletions:    true,
			forceExplicitFiles: true,
			exactPointSemantic: true,
		})
	}
	idx.reparseKeepingResolutions = nil
	if err != nil {
		return nil, fmt.Errorf("indexer: per-file delta pass: %w", err)
	}
	if result != nil && len(result.FailedFiles) > 0 {
		return nil, &editDeltaRefusedError{reason: "files failed: " + strings.Join(result.FailedFiles, ", ")}
	}
	if refused := dw.Unsupported(); len(refused) > 0 {
		return nil, &editDeltaRefusedError{reason: "unsupported writes: " + strings.Join(refused, ", ")}
	}
	endPass("shared_rows")
	// rederive runs the engine again over unchanged files the delta has to
	// carry. They are unchanged, so their stored fingerprints would classify
	// them inert; they are re-derived from source regardless, through the
	// seam a deletion's surviving importers use.
	rederive := func(what string, rels []string) error {
		abs := make([]string, 0, len(rels))
		idx.forcedReparse = make(map[string]struct{}, len(rels))
		for _, rel := range rels {
			p := filepath.Clean(filepath.Join(req.RootPath, filepath.FromSlash(rel)))
			abs = append(abs, p)
			idx.forcedReparse[p] = struct{}{}
		}
		result, err := idx.incrementalWatcherPaths(req.RootPath, abs, incrementalPathMode{
			forceExplicitFiles: true,
			exactPointSemantic: true,
		})
		idx.forcedReparse = nil
		if err != nil {
			return fmt.Errorf("indexer: per-file delta %s pass: %w", what, err)
		}
		if result != nil && len(result.FailedFiles) > 0 {
			return &editDeltaRefusedError{reason: "files failed: " + strings.Join(result.FailedFiles, ", ")}
		}
		if refused := dw.Unsupported(); len(refused) > 0 {
			return &editDeltaRefusedError{reason: "unsupported writes: " + strings.Join(refused, ", ")}
		}
		return nil
	}
	emitters, err := b.editDeltaSharedEmitters(ctx, req, idx, dw, plan)
	if err != nil {
		return nil, err
	}
	if len(emitters) > 0 {
		if err := rederive("shared-row", emitters); err != nil {
			return nil, err
		}
		out.SharedRowEmitters = emitters
		endPass("dependents")
	} else {
		lap("shared_rows_plan")
		passPrefix = "dependents"
	}
	dependents, walked, err := b.editDeltaDependents(ctx, req, plan, dw)
	if err != nil {
		return nil, err
	}
	out.DependentsWalked = walked
	lap("dependents_plan")
	if len(dependents) > 0 {
		if err := rederive("dependent", dependents); err != nil {
			return nil, err
		}
		out.Dependents = dependents
		endPass("")
	}
	idx.incrementalCatchupHook = nil
	report.Work.mark("pass")
	report.ChangedBodyFiles = idx.cloneChangedBodyFiles()

	// A change-set file whose save is inert (its content fingerprints are
	// the stored ones) keeps the rows the view below holds, as the primary
	// per-save path keeps them: what the engine re-derived for it is replaced.
	if len(idx.inertReparsed) > 0 {
		inert := make([]string, 0, len(idx.inertReparsed))
		for p := range idx.inertReparsed {
			inert = append(inert, p)
		}
		sort.Strings(inert)
		_, _, skipped := dw.RestateBelowRows(inert)
		out.InertRestated = len(inert) - len(skipped)
	}
	idx.inertReparsed = nil

	fixed := make(map[string]struct{}, len(out.Paths))
	for _, rel := range out.Paths {
		fixed[builderGraphPath(req.RepoPrefix, rel)] = struct{}{}
	}
	if req.followup && b.Config.Coverage.IsEnabled("clones") {
		projection, err := prepareCloneFollowup(ctx, req.Base, dw, handle, req.RepoPrefix, idx.cloneThreshold())
		if err != nil {
			return nil, fmt.Errorf("indexer: recompute composed clone corpus: %w", err)
		}
		*cloneProjection = projection
		lap("clone_followup")
	}
	payload := dw.Payload(fixed)
	lap("payload")

	if len(payload.Nodes) > 0 || len(payload.Edges) > 0 {
		handle.AddBatch(payload.Nodes, payload.Edges)
	}
	lap("write_rows")
	// Measurement only, opt-in: what a declaration-granular form would have
	// written (edit_delta_declaration_diff.go). It runs inside the delta, so
	// it is off unless asked for.
	if editDeltaMeasureDeclarations {
		fixedPaths := make([]string, 0, len(fixed))
		for p := range fixed {
			fixedPaths = append(fixedPaths, p)
		}
		out.DeclarationDiff = measureDeclarationDiff(dw, fixedPaths)
		lap("declaration_diff")
	}
	extracted := make(map[string]struct{}, len(fixed)+len(out.SharedRowEmitters))
	for p := range fixed {
		extracted[p] = struct{}{}
	}
	for _, rel := range append(append([]string(nil), out.SharedRowEmitters...), out.Dependents...) {
		extracted[builderGraphPath(req.RepoPrefix, rel)] = struct{}{}
	}
	if err := editDeltaResolverRowFTS(idx, handle, payload.Nodes, extracted); err != nil {
		return nil, fmt.Errorf("indexer: write resolver-minted symbol documents: %w", err)
	}
	lap("write_fts")
	// The generation's own freshness provenance: the sample's HEAD and dirty
	// bit (req.headProvenance), as every generation a pass builds records
	// it. The Merkle baseline is the view below's; the counts are what the
	// generation carries.
	workspaceFP := ""
	if r, ok := req.Base.(graph.RepoIndexStateReader); ok {
		if prev, found, _ := r.GetRepoIndexState(req.RepoPrefix); found {
			workspaceFP = prev.WorkspaceFP
		}
	}
	if absRoot, err := filepath.Abs(req.RootPath); err == nil {
		idx.persistRepoIndexState(handle, absRoot, workspaceFP, len(payload.Nodes), len(payload.Edges))
	}
	lap("write_state")
	// The store materializes a builtin sentinel lazily, per generation, for
	// every edge into one, and the lazily materialized row carries no
	// repository stamps. The view's row (stamped by the whole index, or by the
	// delta's own attribution) replaces it under an identity claim. The claim
	// is an identity replacement, never a legacy tombstone: a tombstone would
	// also take over the builtin's outgoing edges recorded in every other file
	// (the value flows out of `len`, `append`, …), hiding them all.
	tombstones := payload.Tombstones
	identityClaims := payload.IdentityClaims
	if stubs := graph.BuiltinStubNodes(payload.Edges); len(stubs) > 0 {
		seen := make(map[string]struct{}, len(tombstones)+len(identityClaims))
		for _, id := range tombstones {
			seen[id] = struct{}{}
		}
		for _, id := range identityClaims {
			seen[id] = struct{}{}
		}
		ids := make([]string, 0, len(stubs))
		for _, stub := range stubs {
			ids = append(ids, stub.ID)
		}
		stamped := dw.View().GetNodesByIDs(ids)
		rows := make([]*graph.Node, 0, len(stubs))
		for _, stub := range stubs {
			row := stamped[stub.ID]
			if row == nil {
				row = stub
			}
			rows = append(rows, row)
			if _, dup := seen[stub.ID]; !dup {
				identityClaims = append(identityClaims, stub.ID)
				seen[stub.ID] = struct{}{}
			}
		}
		// One write transaction for every sentinel row, not one per row.
		handle.AddBatch(rows, nil)
		sort.Strings(identityClaims)
	}
	lap("write_sentinels")
	masks := make([]store_sqlite.FileMask, 0, len(payload.ReplacePaths)+len(payload.DeletePaths)+len(plan.deleted))
	claimed := make(map[string]struct{}, len(payload.ReplacePaths)+len(payload.DeletePaths))
	for _, p := range payload.ReplacePaths {
		claimed[p] = struct{}{}
		masks = append(masks, store_sqlite.FileMask{RepoPrefix: req.RepoPrefix, FilePath: p, Mode: store_sqlite.OwnershipReplace})
	}
	for _, p := range payload.DeletePaths {
		claimed[p] = struct{}{}
		masks = append(masks, store_sqlite.FileMask{RepoPrefix: req.RepoPrefix, FilePath: p, Mode: store_sqlite.OwnershipDelete})
	}
	for _, rel := range plan.deleted {
		graphPath := builderGraphPath(req.RepoPrefix, rel)
		if _, done := claimed[graphPath]; done {
			continue
		}
		claimed[graphPath] = struct{}{}
		masks = append(masks, store_sqlite.FileMask{RepoPrefix: req.RepoPrefix, FilePath: graphPath, Mode: store_sqlite.OwnershipDelete})
	}
	sort.Slice(masks, func(i, j int) bool { return masks[i].FilePath < masks[j].FilePath })
	out.ownership = editDeltaOwnership{
		paths:   make(map[string]struct{}, len(masks)),
		sources: make(map[string]struct{}, len(payload.EdgeSources)+len(tombstones)),
	}
	for _, m := range masks {
		out.ownership.paths[m.FilePath] = struct{}{}
	}
	for _, id := range payload.EdgeSources {
		out.ownership.sources[id] = struct{}{}
	}
	if err := handle.SetFileMasks(masks); err != nil {
		return nil, fmt.Errorf("indexer: write generation file masks: %w", err)
	}
	if err := handle.SetNodeTombstones(tombstones); err != nil {
		return nil, fmt.Errorf("indexer: write generation node tombstones: %w", err)
	}
	for _, id := range tombstones {
		out.ownership.sources[id] = struct{}{}
	}
	if len(identityClaims) > 0 {
		if err := handle.SetNodeIdentityReplacements(identityClaims); err != nil {
			return nil, fmt.Errorf("indexer: write generation node identity claims: %w", err)
		}
	}
	markers := make([]store_sqlite.EdgeSourceMask, 0, len(payload.EdgeSources))
	for _, id := range payload.EdgeSources {
		markers = append(markers, store_sqlite.EdgeSourceMask{SourceID: id, Mode: store_sqlite.OwnershipReplace})
	}
	if err := handle.SetEdgeSourceMasks(markers); err != nil {
		return nil, fmt.Errorf("indexer: write generation edge-source masks: %w", err)
	}
	lap("write_masks")
	report.Work.mark("delta_write")

	stats := dw.DeltaStats()
	out.CoveredPaths, out.ClaimedSources = stats.CoveredPaths, stats.ClaimedSources
	out.MaterializedNodes, out.MaterializedEdges = stats.MaterializedNodes, stats.MaterializedEdges
	out.EdgeClaims, out.EdgeClaimsPromoted, out.ClaimsByWrite = stats.EdgeClaims, stats.EdgeClaimsPromoted, stats.ClaimsByWrite
	out.SlowReads = stats.SlowReads
	out.WholeLayerLoads, out.WholeLayerRows = stats.WholeLayerLoads, stats.WholeLayerRows
	out.LayerRowsRead = stats.LayerRowsRead
	out.PayloadNodes, out.PayloadEdges = len(payload.Nodes), len(payload.Edges)
	out.ReplacePaths, out.DeletePaths = len(payload.ReplacePaths), len(payload.DeletePaths)
	out.EdgeSources, out.Tombstones = len(payload.EdgeSources), len(payload.Tombstones)
	out.DroppedPaths, out.DroppedSources, out.DroppedNodes = payload.DroppedPaths, payload.DroppedSources, payload.DroppedNodes
	out.OrphanEdges = payload.OrphanEdges
	out.RestatedNodes, out.RestatedEdges = payload.RestatedNodes, payload.RestatedEdges
	out.PayloadRows, out.PayloadSteps = payload.Rows, payload.Steps
	out.PayloadStepLayerRows = payload.StepLayerRows
	out.LayerRowsByRead = stats.LayerRowsByRead
	out.BelowRowsServed = stats.BelowRowsServed
	out.IdentityClaims = len(identityClaims)

	report.NodeCount, report.EdgeCount = len(payload.Nodes), len(payload.Edges)
	report.PassNodeCount, report.PassEdgeCount = report.NodeCount, report.EdgeCount
	report.ReplaceMasks = len(payload.ReplacePaths)
	report.DeleteMasks = len(masks) - len(payload.ReplacePaths)
	report.NodeTombstones = len(tombstones)
	report.EdgeSourceMarkers = len(payload.EdgeSources)
	report.PassSteps = append(report.PassSteps, out.Phases...)
	if b.Logger != nil {
		fields := []zap.Field{
			zap.String("checkout", req.Identity.CheckoutID),
			zap.Strings("paths", out.Paths),
			zap.Int("covered", out.CoveredPaths),
			zap.Int("claimed", out.ClaimedSources),
			zap.Int("materialized_nodes", out.MaterializedNodes),
			zap.Int("materialized_edges", out.MaterializedEdges),
			zap.Int("edge_claims", out.EdgeClaims),
			zap.Int("edge_claims_promoted", out.EdgeClaimsPromoted),
			zap.Any("claims_by_write", out.ClaimsByWrite),
			zap.Int("payload_nodes", out.PayloadNodes),
			zap.Int("payload_edges", out.PayloadEdges),
			zap.Int("edge_sources", out.EdgeSources),
			zap.Int("tombstones", out.Tombstones),
			zap.Int("restated_nodes", out.RestatedNodes),
			zap.Any("declaration_diff", out.DeclarationDiff),
			zap.Int("restated_edges", out.RestatedEdges),
			zap.Any("slow_reads", out.SlowReads),
			zap.Int("whole_layer_loads", out.WholeLayerLoads),
			zap.Int("whole_layer_rows", out.WholeLayerRows),
			zap.Int("layer_rows_read", out.LayerRowsRead),
			zap.Bool("dependents_walked", out.DependentsWalked),
			zap.Any("payload_rows", out.PayloadRows),
			zap.Any("payload_steps", out.PayloadSteps),
			zap.Any("payload_step_layer_rows", out.PayloadStepLayerRows),
			zap.Any("phase_layer_rows", out.PhaseLayerRows),
			zap.Any("layer_rows_by_read", out.LayerRowsByRead),
			zap.Int("below_rows_served", out.BelowRowsServed),
			zap.Strings("dependents", out.Dependents),
			zap.Int("resolve_frontier", out.ResolveFrontier),
			zap.Float64("resolve_ms", float64(out.ResolveDuration.Microseconds())/1000),
			zap.Int64("resolve_major_faults", out.ResolveFaults),
			zap.Float64("contract_registry_ms", float64(idx.contractRegistryLoad.Microseconds())/1000),
			zap.Bool("contract_registry_cached", out.ContractRegistryCached),
			zap.Bool("base_projection_cache", baseCache != nil),
			zap.Any("phase_major_faults", out.PhaseFaults),
			zap.Any("phase_cpu_ms", editDeltaPhaseMillis(out.PhaseCPU)),
			zap.Any("phase_reader_wait_ms", editDeltaPhaseMillis(out.PhaseReaderWait)),
			zap.Any("phase_store_waits", editDeltaPhaseStoreWaits(out.PhaseStoreWaits)),
			zap.Any("phase_sched_wait_ms", editDeltaPhaseMillis(out.PhaseSchedWait)),
			zap.Any("phase_sched_waits", out.PhaseSchedWaits),
			zap.Bool("sqlite_sleep_gauge_installed", out.SleepGaugeInstalled),
			zap.Any("phase_wal_bytes", out.PhaseWALBytes),
			zap.Any("phase_write_tx", out.PhaseWriteTx),
			zap.Strings("stack", dw.StackShape()),
			// chain_depth is the number of layers the delta composes over the
			// store (the commit layer and the working-tree chain above it): a
			// file's rows sit beside every generation's copy of them, so the
			// delta's writes and evictions grow with it until a fold.
			zap.Int("chain_depth", len(dw.StackShape())-1),
		}
		if idx.contractRegistryLoadPhases != nil {
			fields = append(fields, zap.Any("contract_registry_phases", idx.contractRegistryLoadPhases))
		}
		fields = append(fields, zap.String("stack_cache_key", editDeltaKeyDigest(out.StackCacheKey)),
			zap.Int("chain_layers_overlaid", out.ChainLayersOverlaid))
		if keeper := dw.ChainLayerRowsKeeper(); keeper != nil {
			layers, rows, hits, loads, declined := keeper.Counters()
			out.ChainKeeper = [5]int{layers, rows, hits, loads, declined}
			fields = append(fields, zap.Ints("chain_keeper_layers_rows_hits_loads_declined", []int{layers, rows, hits, loads, declined}))
		}
		if baseCache != nil {
			fh, fm, ih, im := baseCache.Stats()
			fields = append(fields,
				zap.Int("base_file_index_hits", fh-baseFileHits), zap.Int("base_file_index_misses", fm-baseFileMisses),
				zap.Int("base_import_hits", ih-baseImportHits), zap.Int("base_import_misses", im-baseImportMisses))
			// The stack-level entries (the layers below the delta composed
			// over the store), cumulative over the stack's deltas.
			sih, sim := baseCache.StackStats()
			sfh, sfm := baseCache.StackFileStats()
			snh, snm := baseCache.StackNodeStats()
			sah, sam := baseCache.StackNameStats()
			srh, srm := baseCache.StackRefFactStats()
			fields = append(fields, zap.Ints("stack_cache_hits_import_file_node_name_fact", []int{sih, sfh, snh, sah, srh}),
				zap.Ints("stack_cache_misses_import_file_node_name_fact", []int{sim, sfm, snm, sam, srm}))
			ph, pm := baseCache.StackFileNodeStats()
			out.StackPathNodeHits, out.StackPathNodeMisses = ph-pathNodeHits, pm-pathNodeMisses
			fields = append(fields, zap.Int("stack_path_node_hits", out.StackPathNodeHits),
				zap.Int("stack_path_node_misses", out.StackPathNodeMisses))
			// Cumulative over the stack's deltas (delta_writer_stack_recorded.go).
			rh, rm := baseCache.StackRecordedEdgeStats()
			fields = append(fields, zap.Int("stack_recorded_edge_hits", rh), zap.Int("stack_recorded_edge_misses", rm))
			// Cumulative over the stack's deltas: the bottom store's
			// adjacency (identities) and scoped rows (scopes) kept per stack.
			bah, bam := baseCache.StackBaseAdjacencyStats()
			bsh, bsm := baseCache.StackBaseScopedStats()
			fields = append(fields, zap.Ints("stack_base_adjacency_scoped_hits", []int{bah, bsh}),
				zap.Ints("stack_base_adjacency_scoped_misses", []int{bam, bsm}))
		}
		io := editDeltaProcessIO().since(ioStarted)
		out.PageFaults, out.BlockReads = io.majorFaults, io.blockReads
		fields = append(fields, zap.Int64("delta_major_faults", io.majorFaults), zap.Int64("delta_block_reads", io.blockReads))
		fields = append(fields, phaseFields("delta_", out.Phases)...)
		b.Logger.Info("indexer: working-tree edit delta", fields...)
	}
	out.carryRegistry = editDeltaRegistryCarry(idx, req.Base, b.Store, req.RepoPrefix, req.WorkspaceID, req.ProjectID)
	out.contractInputWitness = idx.contractInputWitness
	return out, nil
}

// editDeltaResolverRowFTS writes the symbol search document of every row the
// delta carries that no file it re-derived extracted — the dependency, module
// and external-call rows the resolver mints. The per-save engine writes
// documents only for the rows it extracts from a file, while a whole index
// writes one for every node it searches, minted rows included; without this a
// row the delta introduces (the dependency stub a deleted package's importers
// now bind to, say) is served with no document. extracted holds the graph
// paths the engine re-derived from source.
func editDeltaResolverRowFTS(idx *Indexer, handle *store_sqlite.Store, nodes []*graph.Node, extracted map[string]struct{}) error {
	var items []graph.SymbolFTSItem
	for _, node := range nodes {
		if node == nil || !idx.shouldIndexForSearch(node) {
			continue
		}
		if _, fromSource := extracted[node.FilePath]; fromSource {
			continue
		}
		items = append(items, graph.SymbolFTSItem{NodeID: node.ID, Tokens: ftsTokensFor(node, idx.projectName)})
	}
	if len(items) == 0 {
		return nil
	}
	return handle.BatchUpsertSymbolFTS(items)
}

// Unwrap exposes the composed view a layer base carries, so a delta can take
// the stack below it apart for batched reads (graph.Unwrapper). The reference
// facts the base adds are served by the delta through graph.RefFactsReader on
// the base itself, which Unwrap does not bypass.
func (b commitLayerBase) Unwrap() graph.Reader { return b.Reader }

// editDeltaPhaseMillis renders per-phase durations in milliseconds.
func editDeltaPhaseMillis(phases map[string]time.Duration) map[string]float64 {
	out := make(map[string]float64, len(phases))
	for name, d := range phases {
		out[name] = float64(d.Microseconds()) / 1000
	}
	return out
}

// editDeltaPhaseStoreWaits renders each phase's store waits by kind.
func editDeltaPhaseStoreWaits(phases map[string]store_sqlite.ReaderWaitSplit) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(phases))
	for name, w := range phases {
		out[name] = storeWaitMillis(w)
	}
	return out
}
