package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
)

// A scoped warm reconcile can cover thousands of files after a branch switch,
// but it must not turn into one SQLite transaction/query pipeline per file.
// Keep both retained extraction memory and statement frontiers bounded while
// amortising every graph read/write over a small file group.
const (
	incrementalBatchFiles = 32
	incrementalBatchNodes = 20_000
	incrementalBatchEdges = 80_000
	incrementalBatchBytes = 64 << 20
	deletedBatchFiles     = 128
)

type incrementalBatchStage struct {
	absPath       string
	mtimeKey      string
	readVersion   fileReadVersion
	relPath       string
	graphPath     string
	src           []byte
	result        *parser.ExtractionResult
	prepared      *preparedExtraction
	priorNodes    []*graph.Node
	storedGraph   fileDeltaFingerprints
	storedDerived derivedFingerprints
	probe         fileDeltaProbe
	metadataOnly  bool
	edgeRefreshes []graph.EdgeReindex
	abSnap        *affectedBySnapshot
	reuse         map[reuseKey]*reuseVal
	priorPending  []*graph.Edge
	bytes         int64
}

func (stage *incrementalBatchStage) releasePrepared() {
	if stage == nil {
		return
	}
	stage.prepared.release()
	stage.prepared = nil
	stage.src = nil
	stage.result = nil
}

type fileReadReceipt struct {
	absPath     string
	mtimeKey    string
	readVersion fileReadVersion
}

type incrementalFallback struct {
	filePath      string
	graphPath     string
	priorNodes    []*graph.Node
	storedGraph   fileDeltaFingerprints
	storedDerived derivedFingerprints
	probe         fileDeltaProbe
	probeOK       bool
}

type incrementalPriorView struct {
	nodesByFile map[string][]*graph.Node
	nodesByID   map[string]*graph.Node
	inByNode    map[string][]*graph.Edge
	outByNode   map[string][]*graph.Edge
}

// reindexIncrementalFilesBatched owns the bounded multi-file engine. Normal
// reconciliation retries first-pass failures once. A direct IndexFile request
// can instead stop after a first-pass version race so its historical receipt
// contract remains observable while watcher storms retain retry behaviour.
func (idx *Indexer) reindexIncrementalFilesBatched(
	staleFiles, deletedFiles []string,
	markerBatch *reparsePendingEnrichmentBatch,
	surfaceFirstVersionChange bool,
) (DerivedInvalidationPlan, []string, []string, []string) {
	idx.loadFileIndexFailures()
	defer idx.flushFileIndexFailures()
	var invalidation DerivedInvalidationPlan
	deleteTiming := startReconcilePhase(idx.logger, idx.repoPrefix, "graph_delete",
		zap.Int("deleted_files", len(deletedFiles)))
	defer deleteTiming.abort()
	idx.evictDeletedFilesBatched(deletedFiles, &invalidation)
	deleteTiming.complete(nil)

	passPlan, reparsed, failed, versionChanged := idx.reindexIncrementalStalePass(
		staleFiles, markerBatch, surfaceFirstVersionChange,
	)
	invalidation.Merge(passPlan)
	for _, filePath := range failed {
		idx.logger.Debug("incremental reindex: failed to index file",
			zap.String("file", filePath), zap.Error(idx.fileIndexFailureError(filePath)))
	}
	if len(failed) == 0 {
		if len(reparsed) > 0 {
			idx.reparsedThisRun.Store(true)
		}
		return invalidation, reparsed, nil, nil
	}
	if surfaceFirstVersionChange {
		// Direct IndexFile is a single accepted-read operation. Do not let a
		// retry hide any first-pass failure; its caller distinguishes an actual
		// version race from parse/read failures after the complete mutation tail.
		if len(reparsed) > 0 || len(invalidation.Files) > 0 {
			idx.reparsedThisRun.Store(true)
		}
		return invalidation, reparsed, failed, versionChanged
	}

	var retryPaths, permissionFailed []string
	for _, path := range failed {
		if errors.Is(idx.fileIndexFailureError(path), os.ErrPermission) {
			permissionFailed = append(permissionFailed, path)
		} else {
			retryPaths = append(retryPaths, path)
		}
	}
	retryPlan, retryReparsed, retryFailed, retryVersionChanged := idx.reindexIncrementalStalePass(
		retryPaths, markerBatch, false,
	)
	invalidation.Merge(retryPlan)
	reparsed = appendUniqueSorted(reparsed, retryReparsed...)
	versionChanged = appendUniqueSorted(versionChanged, retryVersionChanged...)
	for _, filePath := range retryFailed {
		err := idx.fileIndexFailureError(filePath)
		if errors.Is(err, os.ErrPermission) {
			continue // One aggregate permission warning is emitted for the repo.
		}
		idx.logger.Warn("incremental reindex: file failed after retry",
			zap.String("file", filePath), zap.Error(err))
	}
	if len(reparsed) > 0 {
		idx.reparsedThisRun.Store(true)
	}
	return invalidation, reparsed, appendUniqueSorted(permissionFailed, retryFailed...), versionChanged
}

func (idx *Indexer) reindexIncrementalStalePass(
	files []string,
	markerBatch *reparsePendingEnrichmentBatch,
	acceptPreparedSnapshot bool,
) (DerivedInvalidationPlan, []string, []string, []string) {
	var plan DerivedInvalidationPlan
	var reparsed, failed, versionChanged []string
	for start := 0; start < len(files); {
		end := min(start+incrementalBatchFiles, len(files))
		consumed, chunkPlan, chunkReparsed, chunkFailed, chunkVersionChanged := idx.reindexIncrementalChunk(
			files[start:end], markerBatch, acceptPreparedSnapshot,
		)
		if consumed <= 0 {
			consumed = 1
		}
		start += consumed
		plan.Merge(chunkPlan)
		reparsed = append(reparsed, chunkReparsed...)
		failed = append(failed, chunkFailed...)
		versionChanged = append(versionChanged, chunkVersionChanged...)
	}
	return plan,
		appendUniqueSorted(nil, reparsed...),
		appendUniqueSorted(nil, failed...),
		appendUniqueSorted(nil, versionChanged...)
}

func (idx *Indexer) reindexIncrementalChunk(
	files []string,
	markerBatch *reparsePendingEnrichmentBatch,
	acceptPreparedSnapshot bool,
) (int, DerivedInvalidationPlan, []string, []string, []string) {
	var plan DerivedInvalidationPlan
	if len(files) == 0 {
		return 0, plan, nil, nil, nil
	}

	graphPaths := make([]string, len(files))
	priorTiming := startReconcilePhase(idx.logger, idx.repoPrefix, "chunk_prior_graph",
		zap.Int("candidate_files", len(files)))
	defer priorTiming.abort()
	for i, filePath := range files {
		graphPaths[i] = idx.prefixPath(idx.relKey(filePath))
	}
	priorByFile := idx.graph.GetFileNodesByPaths(graphPaths)
	priorTiming.complete(nil)
	parseTiming := startReconcilePhase(idx.logger, idx.repoPrefix, "chunk_parse",
		zap.Int("candidate_files", len(files)), zap.Bool("includes_admission_wait", true))
	defer parseTiming.abort()

	stages := make([]*incrementalBatchStage, 0, len(files))
	// Each staged result carries the tree-sitter tree its extraction
	// produced — C memory the Go GC cannot reclaim. Nothing downstream
	// of staging reads the tree (the commit works off Nodes/Edges), so
	// release the batch's trees on every exit, committed or not.
	defer func() {
		for _, stage := range stages {
			stage.releasePrepared()
		}
	}()
	fallbacks := make([]incrementalFallback, 0)
	receipts := make([]fileReadReceipt, 0, len(files))
	nodeCount, edgeCount := 0, 0
	var retainedBytes int64
	var readFailed []string
	consumed := 0
	for i, filePath := range files {
		graphPath := graphPaths[i]
		priorNodes := priorByFile[graphPath]
		storedGraph := storedExtractionGraphFingerprints(priorNodes)
		storedDerived := storedDerivedFingerprints(priorNodes)
		// Once this chunk retains a prepared result, never block while
		// asking the shared budget for another. Admission pressure flushes
		// the partial chunk and retries this file in the next chunk; this
		// prevents repositories from each holding a partial batch while all
		// wait for capacity owned by the others.
		probe, probeOK, admissionBusy := idx.prepareFileDeltaWithAdmission(filePath, len(stages) > 0)
		if admissionBusy {
			break
		}
		consumed++

		if probeOK && storedGraph.semantic != "" &&
			probe.fingerprints.semantic == storedGraph.semantic &&
			probe.fingerprints.metadata == storedGraph.metadata {
			idx.discardPreparedExtraction(filePath)
			receipts = append(receipts, fileReadReceipt{
				absPath: filePath, mtimeKey: idx.relKey(filePath), readVersion: probe.readVersion,
			})
			plan.InertFiles++
			continue
		}

		if !probeOK {
			if probe.readErr != nil {
				idx.noteFileIndexFailure(filePath, probe.readErr)
				readFailed = append(readFailed, filePath)
				continue
			}
			fallbacks = append(fallbacks, incrementalFallback{
				filePath: filePath, graphPath: graphPath, priorNodes: priorNodes,
				storedGraph: storedGraph, storedDerived: storedDerived,
			})
			continue
		}
		var prepared *preparedExtraction
		var ok bool
		if acceptPreparedSnapshot {
			prepared, ok = idx.takePreparedSnapshot(filePath)
		} else {
			prepared, ok = idx.takePreparedRefresh(filePath)
		}
		if !ok || prepared == nil || prepared.result == nil {
			prepared.release()
			fallbacks = append(fallbacks, incrementalFallback{
				filePath: filePath, graphPath: graphPath, priorNodes: priorNodes,
				storedGraph: storedGraph, storedDerived: storedDerived,
				probe: probe, probeOK: true,
			})
			continue
		}

		idx.applyRepoPrefix(prepared.result.Nodes, prepared.result.Edges)
		stage := &incrementalBatchStage{
			absPath: filePath, mtimeKey: idx.relKey(filePath),
			readVersion: prepared.readVersion,
			relPath:     prepared.relPath, graphPath: graphPath,
			src: prepared.src, result: prepared.result, prepared: prepared, priorNodes: priorNodes,
			storedGraph: storedGraph, storedDerived: storedDerived, probe: probe,
			metadataOnly: storedGraph.semantic != "" &&
				probe.fingerprints.semantic == storedGraph.semantic,
		}
		stage.bytes = estimateParseGraphBytes(stage.result.Nodes, stage.result.Edges) + int64(len(stage.src))
		stages = append(stages, stage)
		nodeCount += len(stage.result.Nodes)
		edgeCount += len(stage.result.Edges)
		retainedBytes += stage.bytes
		if len(stages) >= incrementalBatchFiles || nodeCount >= incrementalBatchNodes ||
			edgeCount >= incrementalBatchEdges || retainedBytes >= incrementalBatchBytes {
			break
		}
	}

	parseTiming.complete(nil, zap.Int("consumed_files", consumed), zap.Int("staged_files", len(stages)),
		zap.Int("inert_files", plan.InertFiles), zap.Int("read_failed_files", len(readFailed)),
		zap.Int("fallback_files", len(fallbacks)), zap.Int("nodes", nodeCount), zap.Int("edges", edgeCount))
	if len(stages) > 0 {
		plan.Merge(idx.commitIncrementalStages(stages, markerBatch))
		for _, stage := range stages {
			receipts = append(receipts, fileReadReceipt{
				absPath: stage.absPath, mtimeKey: stage.mtimeKey, readVersion: stage.readVersion,
			})
		}
		// Commit no longer reads the staged source, result, or tree. Return
		// their shared admission capacity before fallback work or receipt
		// validation can wait behind another repository.
		for _, stage := range stages {
			stage.releasePrepared()
		}
	}
	receiptTiming := startReconcilePhase(idx.logger, idx.repoPrefix, "chunk_receipts",
		zap.Int("receipts", len(receipts)))
	defer receiptTiming.abort()
	freshPaths, stalePaths := idx.recordFileReadVersionsBatched(receipts)
	receiptTiming.complete(nil, zap.Int("fresh_files", len(freshPaths)), zap.Int("stale_files", len(stalePaths)))
	freshSet := make(map[string]struct{}, len(freshPaths))
	for _, filePath := range freshPaths {
		freshSet[filePath] = struct{}{}
	}

	var reparsed []string
	failed := append(readFailed, stalePaths...)
	var versionChanged []string
	for _, path := range failed {
		if errors.Is(idx.fileIndexFailureError(path), errFileVersionChanged) {
			versionChanged = append(versionChanged, path)
		}
	}
	var fallbackTiming *reconcilePhaseTiming
	if len(fallbacks) > 0 {
		// Legacy fallback combines extraction and graph mutation; do not label
		// this interval as parse-only or silently charge it to graph apply.
		fallbackTiming = startReconcilePhase(idx.logger, idx.repoPrefix, "chunk_fallback_parse_apply",
			zap.Int("files", len(fallbacks)))
		defer fallbackTiming.abort()
	}
	failedBeforeFallback := len(failed)
	for _, fallback := range fallbacks {
		if err := idx.reindexIncrementalFallback(fallback, markerBatch, &plan); err != nil {
			idx.noteFileIndexFailure(fallback.filePath, err)
			idx.discardPreparedExtraction(fallback.filePath)
			failed = append(failed, fallback.filePath)
			if errors.Is(err, errFileVersionChanged) {
				versionChanged = append(versionChanged, fallback.filePath)
			}
			continue
		}
		idx.noteFileIndexFailure(fallback.filePath, nil)
		reparsed = append(reparsed, fallback.filePath)
	}
	if fallbackTiming != nil {
		// Prior read/receipt failures belong to earlier phases, not fallback.
		fallbackTiming.complete(nil, zap.Int("failed_files", len(failed)-failedBeforeFallback))
	}
	for _, stage := range stages {
		if _, fresh := freshSet[stage.absPath]; fresh && !stage.metadataOnly {
			reparsed = append(reparsed, stage.absPath)
		}
	}
	return consumed, plan, reparsed, failed, versionChanged
}

func (idx *Indexer) reindexIncrementalFallback(
	fallback incrementalFallback,
	markerBatch *reparsePendingEnrichmentBatch,
	plan *DerivedInvalidationPlan,
) error {
	plan.ContractBridgeNodeIDs = appendUniqueSorted(
		plan.ContractBridgeNodeIDs,
		contractBridgeNodeIDsForNodes(idx.graph, fallback.priorNodes)...,
	)
	resolve := markerBatch == nil || !markerBatch.deferResolverCatchup
	if err := idx.indexFile(fallback.filePath, resolve, markerBatch); err != nil {
		return err
	}
	freshNodes := idx.graph.GetFileNodesByPaths([]string{fallback.graphPath})[fallback.graphPath]
	freshDerived := storedDerivedFingerprints(freshNodes)
	if fallback.probeOK && fallback.probe.derived.complete() {
		freshDerived = fallback.probe.derived
	}
	semanticChanged := !fallback.probeOK || fallback.storedGraph.semantic == "" ||
		fallback.probe.fingerprints.semantic != fallback.storedGraph.semantic
	plan.Merge(derivedPlanForDelta(
		fallback.storedDerived, freshDerived, semanticChanged,
		fallback.graphPath, fallback.priorNodes, freshNodes,
	))
	return nil
}

func loadIncrementalPriorView(g graph.Store, stages []*incrementalBatchStage) incrementalPriorView {
	view := incrementalPriorView{
		nodesByFile: make(map[string][]*graph.Node, len(stages)),
		nodesByID:   make(map[string]*graph.Node),
	}
	var ids []string
	for _, stage := range stages {
		view.nodesByFile[stage.graphPath] = stage.priorNodes
		for _, node := range stage.priorNodes {
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := view.nodesByID[node.ID]; duplicate {
				continue
			}
			view.nodesByID[node.ID] = node
			ids = append(ids, node.ID)
		}
	}
	if len(ids) > 0 {
		view.inByNode = g.GetInEdgesByNodeIDs(ids)
		view.outByNode = g.GetOutEdgesByNodeIDs(ids)
	}

	missingTargets := make(map[string]struct{})
	for _, edges := range view.outByNode {
		for _, edge := range edges {
			if !reusableResolvedEdge(edge) {
				continue
			}
			if _, known := view.nodesByID[edge.To]; !known {
				missingTargets[edge.To] = struct{}{}
			}
		}
	}
	if len(missingTargets) > 0 {
		ids = ids[:0]
		for id := range missingTargets {
			ids = append(ids, id)
		}
		for id, node := range g.GetNodesByIDs(ids) {
			if node != nil {
				view.nodesByID[id] = node
			}
		}
	}
	return view
}

// contractBridgeNodeIDsFromPriorView captures the exact derived bridge nodes
// that will lose an incident contract edge when the selected files are evicted.
// The adjacency is already loaded in one bounded batch by loadIncrementalPriorView.
func contractBridgeNodeIDsFromPriorView(stages []*incrementalBatchStage, view incrementalPriorView) []string {
	bridgeIDs := make(map[string]struct{})
	for _, stage := range stages {
		for _, node := range stage.priorNodes {
			if node == nil || node.ID == "" {
				continue
			}
			for _, edge := range view.inByNode[node.ID] {
				if edge != nil && edge.Kind == graph.EdgeBridges && edge.From != "" {
					bridgeIDs[edge.From] = struct{}{}
				}
			}
		}
	}
	return sortedStringKeys(bridgeIDs)
}

// contractBridgeNodeIDsForNodes is the single-file fallback form. It performs
// one exact incoming-adjacency lookup and never scans the bridge population.
func contractBridgeNodeIDsForNodes(g graph.Store, nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	stage := &incrementalBatchStage{priorNodes: nodes}
	return contractBridgeNodeIDsFromPriorView(
		[]*incrementalBatchStage{stage},
		incrementalPriorView{inByNode: g.GetInEdgesByNodeIDs(ids)},
	)
}

func (idx *Indexer) commitIncrementalStages(
	stages []*incrementalBatchStage,
	markerBatch *reparsePendingEnrichmentBatch,
) DerivedInvalidationPlan {
	timing := startReconcilePhase(idx.logger, idx.repoPrefix, "chunk_graph_apply",
		zap.Int("staged_files", len(stages)))
	defer timing.abort()
	var plan DerivedInvalidationPlan
	view := loadIncrementalPriorView(idx.graph, stages)

	for _, stage := range stages {
		stage.reuse, stage.priorPending = captureIncrementalStateFromView(
			stage.priorNodes, view.outByNode, view.nodesByID,
		)
		// A using-stamp change re-prices every visibility-narrowed verdict
		// in the file: reuse would restore an extension target the new
		// usings refuse, and the prior-unresolved skip would park a call
		// the new usings can bind. Drop the snapshot and force the full
		// structural path — resolutions may legitimately change.
		if csharpVisibilityStampForNodes(stage.priorNodes) != csharpVisibilityStampForNodes(stage.result.Nodes) {
			stage.reuse, stage.priorPending = nil, nil
			stage.metadataOnly = false
		}
	}

	// A metadata-only edge refresh cannot safely race a sibling structural
	// replacement whose target it references. Promote that source to the same
	// structural batch; repeat to close transitive dependencies.
	for changed := true; changed; {
		changed = false
		evicted := structuralPriorIDs(stages)
		for _, stage := range stages {
			if !stage.metadataOnly || metadataStageIndependent(stage, view.outByNode, evicted) {
				continue
			}
			stage.metadataOnly = false
			changed = true
		}
	}

	freshIDs := make(map[string]struct{})
	structuralPrior := structuralPriorIDs(stages)
	for _, stage := range stages {
		for _, node := range stage.result.Nodes {
			if node != nil && node.ID != "" {
				freshIDs[node.ID] = struct{}{}
			}
		}
	}
	existing := make(map[string]struct{}, len(view.nodesByID)+len(freshIDs))
	for id := range view.nodesByID {
		if _, evicted := structuralPrior[id]; !evicted {
			existing[id] = struct{}{}
		}
	}
	for id := range freshIDs {
		existing[id] = struct{}{}
	}

	var structural, metadata []*incrementalBatchStage
	for _, stage := range stages {
		if stage.metadataOnly {
			if !prepareMetadataRefreshFromView(stage, view.outByNode, existing) {
				stage.metadataOnly = false
			}
		}
		if stage.metadataOnly {
			metadata = append(metadata, stage)
		} else {
			structural = append(structural, stage)
		}
	}

	// A validation failure above may have promoted another replacement. Rebuild
	// target existence and apply captured resolutions only after classification
	// has reached its final form.
	structuralPrior = structuralPriorIDs(stages)
	existing = make(map[string]struct{}, len(view.nodesByID)+len(freshIDs))
	for id := range view.nodesByID {
		if _, evicted := structuralPrior[id]; !evicted {
			existing[id] = struct{}{}
		}
	}
	for id := range freshIDs {
		existing[id] = struct{}{}
	}
	for _, stage := range structural {
		applyResolvedOutEdgesFromView(stage.result.Edges, stage.reuse, existing)
		if !idx.deferGlobalPasses.Load() {
			stage.abSnap = snapshotAffectedByFromView(stage.priorNodes, view)
		}
	}
	validatedMetadata := metadata[:0]
	for _, stage := range metadata {
		if !prepareMetadataRefreshFromView(stage, view.outByNode, existing) {
			// Extremely defensive: the first validation succeeded, so this can
			// only happen if an in-memory adapter mutated the snapshot concurrently.
			stage.metadataOnly = false
			structural = append(structural, stage)
			applyResolvedOutEdgesFromView(stage.result.Edges, stage.reuse, existing)
			if !idx.deferGlobalPasses.Load() {
				stage.abSnap = snapshotAffectedByFromView(stage.priorNodes, view)
			}
			continue
		}
		validatedMetadata = append(validatedMetadata, stage)
	}
	metadata = validatedMetadata
	plan.ContractBridgeNodeIDs = appendUniqueSorted(
		plan.ContractBridgeNodeIDs,
		contractBridgeNodeIDsFromPriorView(structural, view)...,
	)

	idx.replaceIncrementalContentBatch(stages)

	if len(structural) > 0 {
		idx.commitStructuralIncrementalBatch(structural, view, markerBatch)
	}
	if len(metadata) > 0 {
		idx.commitMetadataIncrementalBatch(metadata)
	}
	idx.persistIncrementalSidecars(stages)
	idx.updateIncrementalSearch(stages)
	idx.upsertIncrementalFTS(stages)

	for _, stage := range stages {
		if stage.metadataOnly {
			plan.MetadataOnlyFiles++
			plan.Files = appendUniqueSorted(plan.Files, stage.graphPath)
			continue
		}
		freshDerived := storedDerivedFingerprints(stage.result.Nodes)
		if stage.probe.derived.complete() {
			freshDerived = stage.probe.derived
		}
		semanticChanged := stage.storedGraph.semantic == "" ||
			stage.probe.fingerprints.semantic != stage.storedGraph.semantic
		plan.Merge(derivedPlanForDelta(
			stage.storedDerived, freshDerived, semanticChanged,
			stage.graphPath, stage.priorNodes, stage.result.Nodes,
		))
	}
	timing.complete(nil, zap.Int("structural_files", len(structural)), zap.Int("metadata_files", len(metadata)))
	return plan
}

func structuralPriorIDs(stages []*incrementalBatchStage) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, stage := range stages {
		if stage.metadataOnly {
			continue
		}
		for _, node := range stage.priorNodes {
			if node != nil {
				ids[node.ID] = struct{}{}
			}
		}
	}
	return ids
}

func metadataStageIndependent(
	stage *incrementalBatchStage,
	outByNode map[string][]*graph.Edge,
	evicted map[string]struct{},
) bool {
	for _, node := range stage.priorNodes {
		if node == nil {
			continue
		}
		for _, edge := range outByNode[node.ID] {
			if edge != nil {
				if _, dependency := evicted[edge.To]; dependency {
					return false
				}
			}
		}
	}
	return true
}

func captureIncrementalStateFromView(
	nodes []*graph.Node,
	outByNode map[string][]*graph.Edge,
	nodesByID map[string]*graph.Node,
) (map[reuseKey]*reuseVal, []*graph.Edge) {
	reuse := make(map[reuseKey]*reuseVal)
	var priorPending []*graph.Edge
	for _, node := range nodes {
		if node == nil {
			continue
		}
		for _, edge := range outByNode[node.ID] {
			if edge == nil || edge.To == "" {
				continue
			}
			if graph.IsUnresolvedTarget(edge.To) {
				priorPending = append(priorPending, &graph.Edge{
					From: edge.From, Kind: edge.Kind, To: edge.To, Meta: edge.Meta,
				})
				continue
			}
			if !reusableResolvedEdge(edge) {
				continue
			}
			target := nodesByID[edge.To]
			if target == nil || target.Name == "" {
				continue
			}
			key := reuseKey{from: edge.From, kind: edge.Kind, recv: edgeReceiverType(edge), name: target.Name}
			if current, seen := reuse[key]; seen {
				if current != nil && current.to != edge.To {
					reuse[key] = nil
				}
				continue
			}
			reuse[key] = &reuseVal{
				to: edge.To, confidence: edge.Confidence,
				confLabel: edge.ConfidenceLabel, origin: edge.Origin, tier: edge.Tier,
			}
		}
	}
	return reuse, priorPending
}

func applyResolvedOutEdgesFromView(
	edges []*graph.Edge,
	reuse map[reuseKey]*reuseVal,
	existing map[string]struct{},
) int {
	if len(reuse) == 0 {
		return 0
	}
	reused := 0
	for _, edge := range edges {
		if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
			continue
		}
		name := reuseIdentifier(graph.UnresolvedName(edge.To))
		if name == "" {
			continue
		}
		value := reuse[reuseKey{
			from: edge.From, kind: edge.Kind, recv: edgeReceiverType(edge), name: name,
		}]
		if value == nil {
			continue
		}
		if _, ok := existing[value.to]; !ok {
			continue
		}
		edge.To = value.to
		edge.Confidence = value.confidence
		edge.ConfidenceLabel = value.confLabel
		edge.Origin = value.origin
		edge.Tier = value.tier
		reused++
	}
	return reused
}

func prepareMetadataRefreshFromView(
	stage *incrementalBatchStage,
	outByNode map[string][]*graph.Edge,
	existing map[string]struct{},
) bool {
	priorByID := make(map[string]*graph.Node, len(stage.priorNodes))
	for _, node := range stage.priorNodes {
		if node != nil {
			priorByID[node.ID] = node
		}
	}
	if len(priorByID) != len(stage.result.Nodes) {
		return false
	}
	for i, fresh := range stage.result.Nodes {
		if fresh == nil {
			return false
		}
		old := priorByID[fresh.ID]
		if old == nil {
			return false
		}
		copyNode := *fresh
		copyNode.Meta = mergeRefreshMeta(old.Meta, fresh.Meta)
		if copyNode.WorkspaceID == "" {
			copyNode.WorkspaceID = old.WorkspaceID
		}
		if copyNode.ProjectID == "" {
			copyNode.ProjectID = old.ProjectID
		}
		stage.result.Nodes[i] = &copyNode
	}

	applyResolvedOutEdgesFromView(stage.result.Edges, stage.reuse, existing)
	freshByKey := make(map[edgeRefreshKey][]*graph.Edge)
	for _, edge := range stage.result.Edges {
		if edge == nil {
			continue
		}
		if priorByID[edge.From] == nil {
			return false
		}
		key := edgeRefreshKey{from: edge.From, kind: edge.Kind, alias: edge.Alias}
		freshByKey[key] = append(freshByKey[key], edge)
	}
	oldByKey := make(map[edgeRefreshKey][]*graph.Edge)
	for id := range priorByID {
		for _, edge := range outByNode[id] {
			if edge == nil {
				continue
			}
			key := edgeRefreshKey{from: edge.From, kind: edge.Kind, alias: edge.Alias}
			if _, needed := freshByKey[key]; needed {
				oldByKey[key] = append(oldByKey[key], edge)
			}
		}
	}
	updates := make([]graph.EdgeReindex, 0, len(stage.result.Edges))
	for key, fresh := range freshByKey {
		old := oldByKey[key]
		if len(old) != len(fresh) {
			return false
		}
		sort.Slice(old, func(i, j int) bool {
			if old[i].Line != old[j].Line {
				return old[i].Line < old[j].Line
			}
			return old[i].To < old[j].To
		})
		sort.Slice(fresh, func(i, j int) bool {
			if fresh[i].Line != fresh[j].Line {
				return fresh[i].Line < fresh[j].Line
			}
			return fresh[i].To < fresh[j].To
		})
		for i := range fresh {
			before := old[i]
			after := *before
			after.FilePath = fresh[i].FilePath
			after.Line = fresh[i].Line
			after.Alias = fresh[i].Alias
			after.Meta = mergeRefreshMeta(before.Meta, fresh[i].Meta)
			updates = append(updates, graph.EdgeReindex{
				Edge: &after, OldTo: before.To,
				OldFilePath: before.FilePath, OldLine: before.Line,
				RefreshIdentity: true,
			})
		}
	}
	stage.edgeRefreshes = updates
	return true
}

func (idx *Indexer) commitStructuralIncrementalBatch(
	stages []*incrementalBatchStage,
	view incrementalPriorView,
	markerBatch *reparsePendingEnrichmentBatch,
) {
	deferResolverCatchup := markerBatch != nil && markerBatch.deferResolverCatchup
	paths := make([]string, 0, len(stages))
	oldNodeIDs := make([]string, 0)
	oldFuncIDs := make([]string, 0)
	var nodes []*graph.Node
	var edges []*graph.Edge
	var priorPending []*graph.Edge
	for _, stage := range stages {
		paths = append(paths, stage.graphPath)
		nodes = append(nodes, stage.result.Nodes...)
		edges = append(edges, stage.result.Edges...)
		priorPending = append(priorPending, stage.priorPending...)
		for _, node := range stage.priorNodes {
			if node == nil {
				continue
			}
			oldNodeIDs = append(oldNodeIDs, node.ID)
			idx.removeFromSearch(node)
			if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
				oldFuncIDs = append(oldFuncIDs, node.ID)
			}
		}
	}

	restubIncomingRefsFromView(idx.graph, stages, view)
	idx.deleteSymbolFTS(oldNodeIDs)
	evictFilesBatched(idx.graph, paths)
	idx.graph.AddBatch(nodes, edges)

	if !deferResolverCatchup {
		idx.observeIncrementalCatchup("resolve", paths)
		idx.resolver.SetIncrementalSkip(priorPending)
		idx.resolver.ResolveFilesAndIncoming(paths)
		idx.resolver.SetIncrementalSkip(nil)
		idx.observeIncrementalCatchup("dataflow", paths)
		idx.materializeDataflowParamsForStages(stages)
	}

	// A global-using edit changes every dependent file's visibility
	// without touching the files themselves — nothing above re-resolves
	// them. Re-price their extension binds now; the fresh stamps are
	// already committed by the AddBatch above.
	var globalChanged []string
	for _, stage := range stages {
		if csharpVisibilityStampForNodes(stage.priorNodes).globals !=
			csharpVisibilityStampForNodes(stage.result.Nodes).globals {
			globalChanged = append(globalChanged, stage.graphPath)
		}
	}
	if len(globalChanged) > 0 {
		own := make(map[string]struct{}, len(paths))
		for _, p := range paths {
			own[p] = struct{}{}
		}
		idx.reresolveCSharpGlobalUsingDependents(globalChanged, own)
	}

	if !idx.deferGlobalPasses.Load() {
		freshFuncs := cloneFuncNodes(nodes)
		if len(oldFuncIDs) > 0 || len(freshFuncs) > 0 {
			if idx.cloneIndex != nil && idx.cloneIndex.Ready() {
				idx.cloneIndex.EvictFuncs(idx.graph, oldFuncIDs)
				idx.cloneIndex.UpdateFuncs(idx.graph, idx.repoPrefix, freshFuncs, idx.cloneThreshold())
			} else if idx.cloneIndex != nil {
				idx.cloneIndex.MarkPending()
			}
		}
	}

	if !deferResolverCatchup {
		idx.observeIncrementalCatchup("ref_facts", paths)
		idx.persistRefFactsForFiles(paths)
		if !idx.deferGlobalPasses.Load() {
			idx.reresolveAffectedByStages(stages)
		}
	} else if !idx.deferGlobalPasses.Load() {
		markerBatch.mergeDeferredAffected(idx.planAffectedByStages(stages))
	}
	idx.enrichAndMarkIncrementalStages(stages, markerBatch)
}

func (idx *Indexer) commitMetadataIncrementalBatch(stages []*incrementalBatchStage) {
	var nodes []*graph.Node
	var updates []graph.EdgeReindex
	for _, stage := range stages {
		nodes = append(nodes, stage.result.Nodes...)
		updates = append(updates, stage.edgeRefreshes...)
	}
	idx.graph.AddBatch(nodes, nil)
	idx.graph.ReindexEdges(updates)
}

func (idx *Indexer) updateIncrementalSearch(stages []*incrementalBatchStage) {
	for _, stage := range stages {
		for _, node := range stage.result.Nodes {
			if !idx.shouldIndexForSearch(node) {
				continue
			}
			if stage.metadataOnly {
				idx.search.Remove(node.ID)
			}
			idx.search.Add(node.ID, searchIndexFields(node, idx.projectName)...)
		}
	}
}

func restubIncomingRefsFromView(
	g graph.Store,
	stages []*incrementalBatchStage,
	view incrementalPriorView,
) {
	evicted := structuralPriorIDs(stages)
	var reindexes []graph.EdgeReindex
	for _, stage := range stages {
		for _, node := range stage.priorNodes {
			if node == nil || node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
				continue
			}
			stub := graph.UnresolvedMarker + node.Name
			for _, edge := range view.inByNode[node.ID] {
				if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.To) {
					continue
				}
				if _, sourceEvicted := evicted[edge.From]; sourceEvicted {
					continue
				}
				oldTo := edge.To
				graph.StashRestubProvenance(edge)
				edge.To = stub
				reindexes = append(reindexes, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
			}
		}
	}
	if len(reindexes) > 0 {
		g.ReindexEdges(reindexes)
	}
}

func evictFilesBatched(g graph.Store, paths []string) (int, int) {
	paths = appendUniqueSorted(nil, paths...)
	// Only the caller's own spellings are evicted. Rows a pre-fix binary
	// wrote under a re-spelled path are healed once, by schema migration
	// v13 (purgeLegacyCoverageSpellings): sweeping a twin spelling here
	// would route it through endpoint-touch eviction, which cannot tell a
	// shared coverage target (a license, a team, a module) from a
	// per-file artifact and so would take other files' valid edges with
	// it — or miss the stale ones, depending on which file the shared
	// node happened to be anchored to.
	if len(paths) == 0 {
		return 0, 0
	}
	if batch, ok := g.(graph.FileBatchEvicter); ok {
		return batch.EvictFiles(paths)
	}
	nodes, edges := 0, 0
	for _, path := range paths {
		n, e := g.EvictFile(path)
		nodes += n
		edges += e
	}
	return nodes, edges
}

func (idx *Indexer) deleteSymbolFTS(nodeIDs []string) {
	if deleter, ok := idx.graph.(graph.SymbolFTSBatchDeleter); ok {
		if err := deleter.BatchDeleteSymbolFTS(nodeIDs); err != nil {
			idx.logger.Debug("indexer: backend FTS batch delete failed", zap.Error(err))
		}
	}
}

func (idx *Indexer) upsertIncrementalFTS(stages []*incrementalBatchStage) {
	batcher, ok := idx.graph.(graph.SymbolFTSBatchUpserter)
	if !ok {
		return
	}
	var items []graph.SymbolFTSItem
	for _, stage := range stages {
		for _, node := range stage.result.Nodes {
			if !idx.shouldIndexForSearch(node) {
				continue
			}
			items = append(items, graph.SymbolFTSItem{
				NodeID: node.ID, Tokens: ftsTokensFor(node, idx.projectName),
			})
		}
	}
	if err := batcher.BatchUpsertSymbolFTS(items); err != nil {
		idx.logger.Debug("indexer: backend FTS batch upsert failed",
			zap.Int("symbols", len(items)), zap.Error(err))
	}
}

func (idx *Indexer) replaceIncrementalContentBatch(stages []*incrementalBatchStage) {
	searcher := idx.contentSearcher()
	if searcher == nil || len(stages) == 0 {
		return
	}
	replacements := make([]graph.ContentFTSFileReplacement, 0, len(stages))
	for _, stage := range stages {
		replacements = append(replacements, graph.ContentFTSFileReplacement{
			FilePath: stage.graphPath,
			Items:    collectContentItems(stage.result.Nodes),
		})
	}
	if replacer, ok := searcher.(graph.ContentFTSBatchReplacer); ok {
		if err := replacer.ReplaceContentFiles(idx.repoPrefix, replacements); err != nil {
			idx.logger.Warn("indexer: batched incremental content replacement failed; retaining full text",
				zap.Int("files", len(replacements)), zap.Error(err))
			return
		}
		for _, stage := range stages {
			for _, node := range stage.result.Nodes {
				if isContentNode(node) {
					leanContentNode(node)
				}
			}
		}
		return
	}
	for _, stage := range stages {
		idx.replaceContentSections(stage.graphPath, stage.result.Nodes, false)
	}
}

func (idx *Indexer) persistIncrementalSidecars(stages []*incrementalBatchStage) {
	if len(stages) == 0 {
		return
	}
	fileRows := make([]graph.FileMetaRow, 0, len(stages))
	constFiles := make([]string, 0, len(stages))
	var constRows []graph.ConstantValueRow
	for _, stage := range stages {
		if row, ok := idx.prepareFileMeta(stage.relPath, stage.src, stage.result); ok {
			fileRows = append(fileRows, row)
		}
		rows, _ := idx.prepareConstValues(stage.result)
		constRows = append(constRows, rows...)
		constFiles = append(constFiles, stage.graphPath)
	}
	persistFileMetaRows(idx.graph, idx.repoPrefix, fileRows)
	if writer, ok := idx.graph.(graph.ConstantValueWriter); ok {
		_ = writer.DeleteConstantValuesByFiles(idx.repoPrefix, constFiles)
		if len(constRows) > 0 {
			_ = writer.BulkSetConstantValues(idx.repoPrefix, constRows)
		}
	}
}

// recordFileReadVersionsBatched advances only receipts whose exact parsed
// version is still on disk after the graph/sidecar commit. One os.Stat per file
// is required to close the concurrent-write window; persistence remains one
// set-oriented SQLite write for the whole bounded chunk.
func (idx *Indexer) recordFileReadVersionsBatched(receipts []fileReadReceipt) (fresh, stale []string) {
	if len(receipts) == 0 {
		return nil, nil
	}
	mtimes := make(map[string]int64, len(receipts))
	for _, receipt := range receipts {
		if !receipt.readVersion.valid {
			idx.noteFileIndexFailure(receipt.absPath, errFileVersionChanged)
			stale = append(stale, receipt.absPath)
			continue
		}
		if receipt.readVersion.snapshot {
			// Read from an immutable content source: nothing on disk to
			// restat, and no working-tree mtime to advance the ledger to.
			fresh = append(fresh, receipt.absPath)
			idx.noteFileIndexFailure(receipt.absPath, nil)
			continue
		}
		current, err := os.Stat(receipt.absPath)
		if err != nil {
			idx.noteFileIndexFailure(receipt.absPath, err)
			stale = append(stale, receipt.absPath)
			continue
		}
		if !sameFileVersion(receipt.readVersion.info, current) {
			idx.noteFileIndexFailure(receipt.absPath, errFileVersionChanged)
			stale = append(stale, receipt.absPath)
			continue
		}
		mtimes[receipt.mtimeKey] = receipt.readVersion.mtime
		fresh = append(fresh, receipt.absPath)
		idx.noteFileIndexFailure(receipt.absPath, nil)
	}
	if len(mtimes) == 0 {
		return fresh, stale
	}
	idx.mtimeMu.Lock()
	idx.ensureFileMtimesWritableLocked()
	for path, mtime := range mtimes {
		idx.fileMtimes[path] = mtime
	}
	idx.mtimeMu.Unlock()
	if writer, ok := idx.graph.(graph.FileMtimeWriter); ok {
		if err := writer.BulkSetFileMtimes(idx.repoPrefix, mtimes); err != nil {
			idx.markFileMtimePersistenceDirty()
			idx.logger.Warn("persist file mtimes failed",
				zap.String("repo", idx.repoPrefix), zap.Int("files", len(mtimes)), zap.Error(err))
		}
	}
	return fresh, stale
}

func (idx *Indexer) materializeDataflowParamsForStages(stages []*incrementalBatchStage) {
	fromSet := make(map[string]struct{})
	fileSet := make(map[string]struct{}, len(stages))
	for _, stage := range stages {
		fileSet[stage.graphPath] = struct{}{}
		for _, node := range stage.result.Nodes {
			if node != nil && node.ID != "" {
				fromSet[node.ID] = struct{}{}
			}
		}
		for _, edge := range stage.result.Edges {
			if edge != nil && (edge.Kind == graph.EdgeArgOf || edge.Kind == graph.EdgeReturnsTo) && edge.From != "" {
				fromSet[edge.From] = struct{}{}
			}
		}
	}
	froms := make([]string, 0, len(fromSet))
	for id := range fromSet {
		froms = append(froms, id)
	}
	var edges []*graph.Edge
	for _, outgoing := range idx.graph.GetOutEdgesByNodeIDs(froms) {
		for _, edge := range outgoing {
			if edge == nil {
				continue
			}
			if _, changedFile := fileSet[edge.FilePath]; !changedFile {
				continue
			}
			if edge.Kind == graph.EdgeArgOf || edge.Kind == graph.EdgeReturnsTo {
				edges = append(edges, edge)
			}
		}
	}
	rewriteDataflowBatch(idx.graph, edges)
}

// materializeDataflowParamsForFiles is the receipt-frontier counterpart of the
// stage helper above. It reconstructs the small amount of post-resolve state it
// needs from SQLite in bounded set-oriented reads, so a 1,000-file branch
// switch does not retain 1,000 parse trees or issue one query per file.
func (idx *Indexer) materializeDataflowParamsForFiles(graphPaths []string) {
	graphPaths = appendUniqueSorted(nil, graphPaths...)
	for start := 0; start < len(graphPaths); start += deletedBatchFiles {
		end := min(start+deletedBatchFiles, len(graphPaths))
		paths := graphPaths[start:end]
		nodesByFile := idx.graph.GetFileNodesByPaths(paths)
		fromSet := make(map[string]struct{})
		fileSet := make(map[string]struct{}, len(paths))
		for _, graphPath := range paths {
			fileSet[graphPath] = struct{}{}
			for _, node := range nodesByFile[graphPath] {
				if node != nil && node.ID != "" {
					fromSet[node.ID] = struct{}{}
				}
			}
		}
		froms := make([]string, 0, len(fromSet))
		for id := range fromSet {
			froms = append(froms, id)
		}
		var edges []*graph.Edge
		for _, outgoing := range idx.graph.GetOutEdgesByNodeIDs(froms) {
			for _, edge := range outgoing {
				if edge == nil || (edge.Kind != graph.EdgeArgOf && edge.Kind != graph.EdgeReturnsTo) {
					continue
				}
				if _, changedFile := fileSet[edge.FilePath]; changedFile {
					edges = append(edges, edge)
				}
			}
		}
		rewriteDataflowBatch(idx.graph, edges)
	}
}

func (idx *Indexer) enrichAndMarkIncrementalStages(
	stages []*incrementalBatchStage,
	markerBatch *reparsePendingEnrichmentBatch,
) {
	providersPresent := idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders()
	pending := make(map[string]bool, len(stages))
	eligible := make([]*incrementalBatchStage, 0, len(stages))
	for _, stage := range stages {
		omitSecondarySourceScans := extractionDispositionFor(stage.result).omitSecondarySourceScans()
		pending[stage.graphPath] = providersPresent && !omitSecondarySourceScans
		if !omitSecondarySourceScans {
			eligible = append(eligible, stage)
		}
	}
	watchEnrichment := providersPresent && len(eligible) > 0 &&
		!idx.deferGlobalPasses.Load() && idx.semanticMgr.EnrichesOnWatch() &&
		(markerBatch == nil || !markerBatch.deferResolverCatchup)
	if watchEnrichment {
		paths := make([]string, 0, len(eligible))
		for _, stage := range eligible {
			paths = append(paths, stage.graphPath)
		}
		idx.observeIncrementalCatchup("semantic", paths)
		byLanguage := make(map[string][]string)
		for _, stage := range eligible {
			language := ""
			for _, node := range stage.result.Nodes {
				if node != nil && node.Kind == graph.KindFile {
					language = node.Language
					break
				}
			}
			if language != "" {
				byLanguage[language] = append(byLanguage[language], stage.graphPath)
			}
		}
		for language, paths := range byLanguage {
			if _, err := idx.semanticMgr.EnrichFiles(
				idx.graph, idx.repoPrefix, idx.rootPath, language, paths,
			); err != nil {
				idx.logger.Debug("indexer: batched incremental semantic enrichment failed",
					zap.String("language", language), zap.Int("files", len(paths)), zap.Error(err))
				continue
			}
			for _, path := range paths {
				pending[path] = false
			}
		}
	}
	for graphPath, value := range pending {
		if markerBatch == nil {
			idx.setReparsePendingEnrichment(graphPath, value)
			continue
		}
		if markerBatch.add(graphPath, value) {
			idx.flushReparsePendingEnrichment(markerBatch)
		}
	}
}

func snapshotAffectedByFromView(nodes []*graph.Node, view incrementalPriorView) *affectedBySnapshot {
	refNodes := make([]*graph.Node, 0, len(nodes))
	ownIDs := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		ownIDs[node.ID] = struct{}{}
		if node.Name != "" && graph.IsReferenceableSymbol(node.Kind) {
			refNodes = append(refNodes, node)
		}
	}
	if len(refNodes) == 0 {
		return nil
	}
	adj := symbolShapeAdjacency{
		inEdges: view.inByNode, outEdges: view.outByNode,
		nodes: view.nodesByID,
	}
	snap := &affectedBySnapshot{
		symbols:    make(map[string]symbolShape),
		refSources: make(map[string]map[string]struct{}),
		idsByKey:   make(map[string][]string),
	}
	keyByID := make(map[string]string, len(refNodes))
	for _, node := range refNodes {
		key := stableSymbolKey(node)
		shape := snap.symbols[key]
		shape.kind = node.Kind
		shape.shape += symbolShapeFromAdjacency(node, adj) + "\n"
		snap.symbols[key] = shape
		snap.idsByKey[key] = append(snap.idsByKey[key], node.ID)
		keyByID[node.ID] = key
	}
	for nodeID, key := range keyByID {
		for _, edge := range view.inByNode[nodeID] {
			if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) {
				continue
			}
			if _, own := ownIDs[edge.From]; own {
				continue
			}
			if snap.refSources[key] == nil {
				snap.refSources[key] = make(map[string]struct{})
			}
			snap.refSources[key][edge.From] = struct{}{}
		}
	}
	return snap
}

func affectedByDeltaFromExtraction(
	snap *affectedBySnapshot,
	nodes []*graph.Node,
	edges []*graph.Edge,
) []string {
	if snap == nil {
		return nil
	}
	adj := symbolShapeAdjacency{
		inEdges:  make(map[string][]*graph.Edge),
		outEdges: make(map[string][]*graph.Edge),
		nodes:    make(map[string]*graph.Node, len(nodes)),
	}
	for _, node := range nodes {
		if node != nil {
			adj.nodes[node.ID] = node
		}
	}
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		adj.outEdges[edge.From] = append(adj.outEdges[edge.From], edge)
		adj.inEdges[edge.To] = append(adj.inEdges[edge.To], edge)
	}
	current := make(map[string]symbolShape)
	for _, node := range nodes {
		if node == nil || node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
			continue
		}
		key := stableSymbolKey(node)
		shape := current[key]
		shape.kind = node.Kind
		shape.shape += symbolShapeFromAdjacency(node, adj) + "\n"
		current[key] = shape
	}
	var delta []string
	for key, old := range snap.symbols {
		fresh, exists := current[key]
		if !exists || fresh.kind != old.kind || fresh.shape != old.shape {
			delta = append(delta, key)
		}
	}
	sort.Strings(delta)
	return delta
}

type affectedByBatchPlan struct {
	files []string
}

func (b *reparsePendingEnrichmentBatch) mergeDeferredAffected(plan affectedByBatchPlan) {
	if b == nil {
		return
	}
	if len(plan.files) > 0 && b.deferredAffectedFiles == nil {
		b.deferredAffectedFiles = make(map[string]struct{}, len(plan.files))
	}
	for _, filePath := range plan.files {
		if filePath != "" {
			b.deferredAffectedFiles[filePath] = struct{}{}
		}
	}
}

func (b *reparsePendingEnrichmentBatch) deferredAffectedPlan() affectedByBatchPlan {
	if b == nil {
		return affectedByBatchPlan{}
	}
	files := make([]string, 0, len(b.deferredAffectedFiles))
	for filePath := range b.deferredAffectedFiles {
		files = append(files, filePath)
	}
	sort.Strings(files)
	return affectedByBatchPlan{files: files}
}

func (idx *Indexer) planAffectedByStages(stages []*incrementalBatchStage) affectedByBatchPlan {
	targetOwners := make(map[string]map[string]struct{})
	sourceOwners := make(map[string]map[string]struct{})
	filesByChanged := make(map[string]map[string]struct{})
	for _, stage := range stages {
		delta := affectedByDeltaFromExtraction(stage.abSnap, stage.result.Nodes, stage.result.Edges)
		if len(delta) == 0 {
			continue
		}
		filesByChanged[stage.graphPath] = make(map[string]struct{})
		for _, key := range delta {
			for _, targetID := range stage.abSnap.idsByKey[key] {
				if targetOwners[targetID] == nil {
					targetOwners[targetID] = make(map[string]struct{})
				}
				targetOwners[targetID][stage.graphPath] = struct{}{}
			}
			for sourceID := range stage.abSnap.refSources[key] {
				if sourceOwners[sourceID] == nil {
					sourceOwners[sourceID] = make(map[string]struct{})
				}
				sourceOwners[sourceID][stage.graphPath] = struct{}{}
			}
		}
	}
	if len(filesByChanged) == 0 {
		return affectedByBatchPlan{}
	}

	if reader, ok := idx.graph.(graph.RefFactsReader); ok && len(targetOwners) > 0 {
		targetIDs := make([]string, 0, len(targetOwners))
		for id := range targetOwners {
			targetIDs = append(targetIDs, id)
		}
		byFile, err := reader.LoadRefFactsByTargets(idx.repoPrefix, targetIDs)
		if err != nil {
			idx.logger.Debug("affected-by: batched ref-facts reverse lookup failed", zap.Error(err))
		} else {
			for filePath, facts := range byFile {
				for _, fact := range facts {
					for changedPath := range targetOwners[fact.ToID] {
						if filePath != "" && filePath != changedPath {
							filesByChanged[changedPath][filePath] = struct{}{}
						}
					}
				}
			}
		}
	}
	if len(sourceOwners) > 0 {
		sourceIDs := make([]string, 0, len(sourceOwners))
		for id := range sourceOwners {
			sourceIDs = append(sourceIDs, id)
		}
		for sourceID, node := range idx.graph.GetNodesByIDs(sourceIDs) {
			if node == nil || node.FilePath == "" {
				continue
			}
			for changedPath := range sourceOwners[sourceID] {
				if node.FilePath != changedPath {
					filesByChanged[changedPath][node.FilePath] = struct{}{}
				}
			}
		}
	}

	plan := affectedByBatchPlan{}
	union := make(map[string]struct{})
	for changedPath, fileSet := range filesByChanged {
		files := make([]string, 0, len(fileSet))
		for filePath := range fileSet {
			files = append(files, filePath)
		}
		sort.Strings(files)
		if maxFiles := idx.affectedByMaxFiles(); len(files) > maxFiles {
			idx.logger.Debug("affected-by: re-resolve set truncated",
				zap.String("file", changedPath), zap.Int("affected", len(files)),
				zap.Int("cap", maxFiles), zap.Int("dropped", len(files)-maxFiles))
			files = files[:maxFiles]
		}
		if len(files) == 0 {
			continue
		}
		for _, filePath := range files {
			union[filePath] = struct{}{}
		}
	}
	plan.files = make([]string, 0, len(union))
	for filePath := range union {
		plan.files = append(plan.files, filePath)
	}
	sort.Strings(plan.files)
	return plan
}

func (idx *Indexer) executeAffectedByPlan(plan affectedByBatchPlan) {
	if len(plan.files) == 0 {
		return
	}
	idx.observeIncrementalCatchup("affected_by", plan.files)
	idx.resolver.ResolveFilesAndIncoming(plan.files)
	resolver.SynthesizeExternalCallsForFiles(idx.graph, idx.externalCallSynthesisEnabled(), plan.files)
}

func (idx *Indexer) reresolveAffectedByStages(stages []*incrementalBatchStage) {
	plan := idx.planAffectedByStages(stages)
	idx.executeAffectedByPlan(plan)
	if len(plan.files) > 0 {
		idx.observeIncrementalCatchup("ref_facts", plan.files)
		idx.persistRefFactsForFiles(plan.files)
	}
}

type forcedFileEviction struct {
	result       *IndexResult
	batch        *reparsePendingEnrichmentBatch
	nodesRemoved int
	edgesRemoved int
}

// evictFileIncrementalRaw forcibly removes one canonical repository-relative
// path even when it still exists on disk. The caller already owns the stable
// repository lane. It reuses the bounded deletion core so every graph/search
// sidecar and mtime is removed and returns the exact invalidation frontier for
// the resolver/semantic/derived tail.
func (idx *Indexer) evictFileIncrementalRaw(relPath string) forcedFileEviction {
	batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
	result := &IndexResult{}
	if idx == nil || relPath == "" {
		return forcedFileEviction{result: result, batch: batch}
	}
	// Explicit eviction is intentionally unconditional. Ownership sidecars may
	// be partially missing after an interrupted or older indexing pass; the
	// bounded deletion core is idempotent and must still clear orphan mtimes,
	// search content, ref facts, contracts, and enrichment state.
	dependencyFiles := idx.semanticDependencyFrontierForDeletedFiles([]string{relPath})
	var invalidation DerivedInvalidationPlan
	nodesRemoved, edgesRemoved := idx.evictDeletedFilesBatched([]string{relPath}, &invalidation)
	graphPath := idx.prefixPath(relPath)
	idx.removeIncrementalContractsForFile(graphPath, &invalidation)
	invalidation.Files = appendUniqueSorted(invalidation.Files, dependencyFiles...)

	idx.noteTrigramDirty(filepath.ToSlash(relPath))
	idx.indexGen.Add(1)
	nodes, edges := idx.repoNodeEdgeCount()
	result.NodeCount = nodes
	result.EdgeCount = edges
	result.FileCount = idx.trackedFileCount()
	result.DeletedFileCount = 1
	result.DerivedInvalidation = invalidation
	idx.markPendingEnrichFiles(invalidation.Files)
	return forcedFileEviction{
		result:       result,
		batch:        batch,
		nodesRemoved: nodesRemoved,
		edgesRemoved: edgesRemoved,
	}
}

func (idx *Indexer) removeIncrementalContractsForFile(graphPath string, plan *DerivedInvalidationPlan) {
	reg := idx.ensureIncrementalContractRegistry()
	prior := reg.ByFile(graphPath)
	idx.contractCacheMu.Lock()
	delete(idx.contractCache, graphPath)
	idx.contractCacheMu.Unlock()
	if len(prior) == 0 {
		return
	}

	priorIDs := make(map[string]struct{}, len(prior))
	refresh := contractRefreshResult{Changed: true}
	refresh.addFrontier(prior)
	for _, contract := range prior {
		if contract.ID != "" {
			priorIDs[contract.ID] = struct{}{}
		}
	}
	refresh.Groups = mergeContractGroups(nil, refresh.Groups...)
	refresh.SymbolIDs = appendUniqueSorted(nil, refresh.SymbolIDs...)
	reg.ReplaceFile(graphPath, nil)
	idx.commitIncrementalContractFiles(reg, []string{graphPath}, priorIDs)

	plan.Flags |= DerivedInvalidatesContracts
	plan.ContractGroups = mergeContractGroups(plan.ContractGroups, refresh.Groups...)
	plan.ContractSymbolIDs = appendUniqueSorted(plan.ContractSymbolIDs, refresh.SymbolIDs...)
}

func (idx *Indexer) evictDeletedFilesBatched(deleted []string, plan *DerivedInvalidationPlan) (nodesRemoved, edgesRemoved int) {
	if len(deleted) == 0 {
		return 0, 0
	}
	defer idx.flushFileIndexFailures()
	for _, path := range deleted {
		idx.noteFileIndexFailure(filepath.Join(idx.rootPath, filepath.FromSlash(path)), nil)
	}
	for start := 0; start < len(deleted); start += deletedBatchFiles {
		end := min(start+deletedBatchFiles, len(deleted))
		relPaths := deleted[start:end]
		graphPaths := make([]string, len(relPaths))
		for i, relPath := range relPaths {
			graphPaths[i] = idx.prefixPath(relPath)
		}
		nodesByFile := idx.graph.GetFileNodesByPaths(graphPaths)
		stages := make([]*incrementalBatchStage, 0, len(graphPaths))
		var nodeIDs []string
		for i, graphPath := range graphPaths {
			priorNodes := nodesByFile[graphPath]
			stage := &incrementalBatchStage{graphPath: graphPath, priorNodes: priorNodes}
			stages = append(stages, stage)
			priorDerived := storedDerivedFingerprints(priorNodes)
			deletedPlan := derivedPlanForDelta(
				priorDerived, derivedFingerprints{}, true, graphPath, priorNodes, nil,
			)
			deletedPlan.LegacyFallback = !priorDerived.complete()
			plan.Merge(deletedPlan)
			for _, node := range priorNodes {
				if node == nil {
					continue
				}
				nodeIDs = append(nodeIDs, node.ID)
				idx.removeFromSearch(node)
			}
			_ = i
		}
		view := loadIncrementalPriorView(idx.graph, stages)
		plan.ContractBridgeNodeIDs = appendUniqueSorted(
			plan.ContractBridgeNodeIDs,
			contractBridgeNodeIDsFromPriorView(stages, view)...,
		)
		restubIncomingRefsFromView(idx.graph, stages, view)
		idx.deleteEnrichmentByNodeIDs(nodeIDs)
		idx.deleteSymbolFTS(nodeIDs)
		idx.deleteRefFactsForFiles(idx.repoPrefix, graphPaths)
		idx.deleteIncrementalSidecars(graphPaths)
		idx.clearIncrementalContent(graphPaths)
		nodes, edges := evictFilesBatched(idx.graph, graphPaths)
		nodesRemoved += nodes
		edgesRemoved += edges
	}
	idx.mtimeMu.Lock()
	idx.ensureFileMtimesWritableLocked()
	for _, relPath := range deleted {
		delete(idx.fileMtimes, relPath)
	}
	idx.mtimeMu.Unlock()
	idx.clearRecoveredParseErrors(nil, nil, deleted)
	return nodesRemoved, edgesRemoved
}

func (idx *Indexer) deleteEnrichmentByNodeIDs(nodeIDs []string) {
	if len(nodeIDs) == 0 {
		return
	}
	if writer, ok := idx.graph.(graph.ChurnEnrichmentWriter); ok {
		_ = writer.DeleteChurn(nodeIDs)
	}
	if writer, ok := idx.graph.(graph.CoverageEnrichmentWriter); ok {
		_ = writer.DeleteCoverage(nodeIDs)
	}
	if writer, ok := idx.graph.(graph.ReleaseEnrichmentWriter); ok {
		_ = writer.DeleteReleases(nodeIDs)
	}
	if writer, ok := idx.graph.(graph.BlameEnrichmentWriter); ok {
		_ = writer.DeleteBlame(nodeIDs)
	}
}

func (idx *Indexer) deleteIncrementalSidecars(graphPaths []string) {
	if writer, ok := idx.graph.(graph.FileMetaWriter); ok {
		_ = writer.DeleteFileMetasByFiles(idx.repoPrefix, graphPaths)
	}
	if writer, ok := idx.graph.(graph.ConstantValueWriter); ok {
		_ = writer.DeleteConstantValuesByFiles(idx.repoPrefix, graphPaths)
	}
}

func (idx *Indexer) clearIncrementalContent(graphPaths []string) {
	searcher := idx.contentSearcher()
	if searcher == nil || len(graphPaths) == 0 {
		return
	}
	if replacer, ok := searcher.(graph.ContentFTSBatchReplacer); ok {
		replacements := make([]graph.ContentFTSFileReplacement, len(graphPaths))
		for i, graphPath := range graphPaths {
			replacements[i].FilePath = graphPath
		}
		if err := replacer.ReplaceContentFiles(idx.repoPrefix, replacements); err != nil {
			idx.logger.Debug("indexer: delete content batch failed", zap.Error(err))
		}
		return
	}
	for _, graphPath := range graphPaths {
		_ = searcher.WipeContentFile(graphPath)
	}
}
