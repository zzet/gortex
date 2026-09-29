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
// A caller that hands the builder one large change set gets one large delta.
// Two cases are built by the sparse closure builder
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
	for _, change := range req.Changes {
		if dependencyManifestPath(change.Path) {
			b.logEditDeltaFallback(req, editDeltaFallbackManifest+": "+change.Path)
			return b.Build(ctx, req)
		}
	}
	generationID, report, err := b.buildEditDelta(ctx, req)
	var refused *editDeltaRefusedError
	if err != nil && errors.As(err, &refused) {
		b.logEditDeltaFallback(req, editDeltaFallbackRefused+": "+refused.reason)
		return b.Build(ctx, req)
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
	PayloadNodes      int
	PayloadEdges      int
	ReplacePaths      int
	DeletePaths       int
	EdgeSources       int
	Tombstones        int
	DroppedPaths      int
	DroppedSources    int
	DroppedNodes      int
	OrphanEdges       int
	IdentityClaims    int
	// RestatedNodes / RestatedEdges are the rows at replaced paths identical
	// to the view below's (DeltaPayload).
	RestatedNodes int
	RestatedEdges int
	// SharedRowEmitters are the unchanged files the delta re-derived because
	// a shared registry row's kept copy moved to them.
	SharedRowEmitters []string
	// Dependents are the unchanged files the delta re-derived because the
	// change can move their own rows (editDeltaDependents).
	Dependents []string
	// EnrichmentRestated counts edges the enrichment stage wrote outside the
	// delta's ownership that restated a row the view below serves, removed
	// before publication (edit_delta_enrich.go).
	EnrichmentRestated int
	// WholeLayerLoads / WholeLayerRows: layers below read wholesale
	// (graph.DeltaWriterStats).
	WholeLayerLoads int
	WholeLayerRows  int
	LayerRowsRead   int
	SlowReads       map[string]int

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
		delta, err := b.runEditDelta(ctx, req, plan, handle, &report)
		if err != nil {
			return err
		}
		recordLastEditDelta(delta)
		markPublicationPhase(ctx, PublicationExtracted)
		if err := ctx.Err(); err != nil {
			return err
		}
		b.runEnrichment(ctx, req, handle, &report)
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(report.Enrichment.Ran) > 0 {
			delta.EnrichmentRestated = editDeltaSettleEnrichment(handle, req.Base, delta.ownership)
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
			if err := req.PrePublish(withBuildReadSet(ctx, plan.indexed, nil, plan.deleted), generationID); err != nil {
				return err
			}
		}
		report.Work.mark("prepublish")
		reachBuildCommitPoint(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		markPublicationPhase(ctx, PublicationPayloadFlushed)
		if err := b.Store.PublishPayloadGeneration(ctx, generationID, time.Now().Unix()); err != nil {
			return fmt.Errorf("indexer: publish generation %d: %w", generationID, err)
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
) (*EditDeltaReport, error) {
	out := &EditDeltaReport{}
	dw := graph.NewDeltaWriter(req.Base, handle)
	idx := New(dw, b.Registry, b.Config, b.Logger)
	defer idx.Close()
	idx.headProvenance = req.headProvenance
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
	idx.forcedReparse = make(map[string]struct{}, len(plan.indexed))
	for _, rel := range plan.indexed {
		idx.forcedReparse[filepath.Clean(filepath.Join(req.RootPath, filepath.FromSlash(rel)))] = struct{}{}
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
	idx.forcedReparse = nil
	if err != nil {
		return nil, fmt.Errorf("indexer: per-file delta pass: %w", err)
	}
	if result != nil && len(result.FailedFiles) > 0 {
		return nil, &editDeltaRefusedError{reason: "files failed: " + strings.Join(result.FailedFiles, ", ")}
	}
	if refused := dw.Unsupported(); len(refused) > 0 {
		return nil, &editDeltaRefusedError{reason: "unsupported writes: " + strings.Join(refused, ", ")}
	}
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
	}
	dependents, err := b.editDeltaDependents(ctx, req, plan)
	if err != nil {
		return nil, err
	}
	if len(dependents) > 0 {
		if err := rederive("dependent", dependents); err != nil {
			return nil, err
		}
		out.Dependents = dependents
	}
	report.Work.mark("pass")

	fixed := make(map[string]struct{}, len(out.Paths))
	for _, rel := range out.Paths {
		fixed[builderGraphPath(req.RepoPrefix, rel)] = struct{}{}
	}
	payload := dw.Payload(fixed)

	if len(payload.Nodes) > 0 || len(payload.Edges) > 0 {
		handle.AddBatch(payload.Nodes, payload.Edges)
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
	report.Work.mark("delta_write")

	stats := dw.DeltaStats()
	out.CoveredPaths, out.ClaimedSources = stats.CoveredPaths, stats.ClaimedSources
	out.MaterializedNodes, out.MaterializedEdges = stats.MaterializedNodes, stats.MaterializedEdges
	out.SlowReads = stats.SlowReads
	out.WholeLayerLoads, out.WholeLayerRows = stats.WholeLayerLoads, stats.WholeLayerRows
	out.LayerRowsRead = stats.LayerRowsRead
	out.PayloadNodes, out.PayloadEdges = len(payload.Nodes), len(payload.Edges)
	out.ReplacePaths, out.DeletePaths = len(payload.ReplacePaths), len(payload.DeletePaths)
	out.EdgeSources, out.Tombstones = len(payload.EdgeSources), len(payload.Tombstones)
	out.DroppedPaths, out.DroppedSources, out.DroppedNodes = payload.DroppedPaths, payload.DroppedSources, payload.DroppedNodes
	out.OrphanEdges = payload.OrphanEdges
	out.RestatedNodes, out.RestatedEdges = payload.RestatedNodes, payload.RestatedEdges
	out.IdentityClaims = len(identityClaims)

	report.NodeCount, report.EdgeCount = len(payload.Nodes), len(payload.Edges)
	report.PassNodeCount, report.PassEdgeCount = report.NodeCount, report.EdgeCount
	report.ReplaceMasks = len(payload.ReplacePaths)
	report.DeleteMasks = len(masks) - len(payload.ReplacePaths)
	report.NodeTombstones = len(tombstones)
	report.EdgeSourceMarkers = len(payload.EdgeSources)
	if b.Logger != nil {
		fields := []zap.Field{
			zap.String("checkout", req.Identity.CheckoutID),
			zap.Strings("paths", out.Paths),
			zap.Int("covered", out.CoveredPaths),
			zap.Int("claimed", out.ClaimedSources),
			zap.Int("materialized_nodes", out.MaterializedNodes),
			zap.Int("materialized_edges", out.MaterializedEdges),
			zap.Int("payload_nodes", out.PayloadNodes),
			zap.Int("payload_edges", out.PayloadEdges),
			zap.Int("edge_sources", out.EdgeSources),
			zap.Int("tombstones", out.Tombstones),
			zap.Int("restated_nodes", out.RestatedNodes),
			zap.Int("restated_edges", out.RestatedEdges),
			zap.Any("slow_reads", out.SlowReads),
			zap.Int("whole_layer_loads", out.WholeLayerLoads),
			zap.Int("whole_layer_rows", out.WholeLayerRows),
			zap.Int("layer_rows_read", out.LayerRowsRead),
			zap.Strings("stack", dw.StackShape()),
		}
		b.Logger.Info("indexer: working-tree edit delta", fields...)
	}
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
