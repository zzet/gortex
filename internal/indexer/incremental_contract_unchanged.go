package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

type unchangedFileContracts struct {
	records []contracts.Contract
	targets []*graph.Node
}

// stageUnchangedContracts uses complete file-local durable records, not an
// empty global registry. Unsupported, legacy, cross-file, changed or incomplete
// inputs retain the existing full-registry path before structural eviction.
func (idx *Indexer) stageUnchangedContracts(stage *incrementalBatchStage) bool {
	started := time.Now()
	defer func() { idx.contractValidationTime += time.Since(started) }()
	if idx.contractShortcutReasons == nil {
		idx.contractShortcutReasons = make(map[string]int)
	}
	fallback := func(reason string) bool {
		idx.contractShortcutReasons[reason]++
		return false
	}
	if stage == nil || stage.prepared == nil || stage.result == nil {
		return fallback("incomplete_extraction")
	}
	if idx.contractProjectionNeedsWitness && idx.contractInputWitness == nil {
		return fallback("input_ancestry_unavailable")
	}
	priorNodes := stage.priorNodes
	var reconstructed *contractDependencyInputs
	if _, complete := storedContractDependencyInputs(priorNodes); !complete && idx.priorContractInputs != nil {
		if receipt, ok := idx.priorContractInputs(stage); ok {
			reconstructed = &receipt
			priorNodes = nodesWithContractInputReceipt(priorNodes, receipt)
			idx.contractShortcutReasons["legacy_inputs_reconstructed"]++
		}
	}
	if !sameContractDependencyInputs(priorNodes, stage.result.Nodes) {
		priorSharedChanged := idx.contractSharedInputsChanged
		idx.noteChangedContractDependencyInputs(priorNodes, stage.result.Nodes)
		if len(priorNodes) == 0 && idx.checkedContractFileWasAbsent(stage.graphPath) && !acceptedExtractionProducesSharedContractInputs(stage.result) {
			// A checked new ordinary source has no unknown prior mounts or
			// constants to remove. Existing contract-file refresh is sufficient;
			// retain any shared-input change established by another staged file.
			idx.contractSharedInputsChanged = priorSharedChanged
			// Absence narrows the dependency frontier even though this file
			// still takes normal extraction. Carry its captured input witness
			// through final publication just like unchanged-record proofs.
			idx.contractProofUsed = true
			return fallback("new_file_without_shared_inputs")
		}
		old, oldComplete := storedContractDependencyInputs(priorNodes)
		fresh, freshComplete := storedContractDependencyInputs(stage.result.Nodes)
		if !oldComplete || !freshComplete {
			return fallback("dependency_stamp_missing")
		}
		if old.CrossFile || fresh.CrossFile {
			return fallback("cross_file_dependency_inputs")
		}
		return fallback("dependency_inputs_changed")
	}
	if idx.contractRegistry != nil {
		return fallback("registry_already_ready")
	}
	if _, ok := idx.graph.(contracts.EndpointConstStore); !ok {
		return fallback("constant_reader_unavailable")
	}
	reader, ok := idx.graph.(graph.ContractFileProjectionReader)
	if !ok {
		return fallback("file_projection_unavailable")
	}
	ctx := idx.contractProjectionContext
	if ctx == nil {
		// IndexFile's legacy public API has no context argument. Generation
		// builds install their actual cancellable context above.
		ctx = context.Background()
	}
	projectionStarted := time.Now()
	projection, err := reader.LoadContractFileProjectionContext(ctx, idx.repoPrefix, []string{stage.graphPath})
	idx.contractProjectionTime += time.Since(projectionStarted)
	if errors.Is(err, graph.ErrContractProjectionStale) {
		idx.rememberContractInputError(err)
		return fallback("file_projection_stale")
	}
	if err != nil {
		if !errors.Is(err, graph.ErrContractProjectionUnsupported) && !errors.Is(err, graph.ErrContractProjectionIncomplete) && !errors.Is(err, graph.ErrContractProjectionLimit) {
			idx.rememberContractInputError(err)
		}
		return fallback("file_projection_incomplete")
	}
	selectedFileNodes := projection.FileNodes[stage.graphPath]
	if reconstructed != nil {
		selectedFileNodes = nodesWithContractInputReceipt(selectedFileNodes, *reconstructed)
	}
	if len(projection.OffFileOwnerRows) != 0 ||
		!sameContractDependencyInputs(selectedFileNodes, stage.result.Nodes) {
		return fallback("file_projection_incomplete_or_changed")
	}
	prior, ok := contractRecordsForUnchangedFile(projection, idx.repoPrefix, idx.workspaceID, idx.projectID, stage.graphPath)
	if !ok {
		return fallback("legacy_or_shared_scalar")
	}
	_, byLanguage := idx.buildPerFileContractExtractors()
	var fileID string
	for _, node := range stage.result.Nodes {
		if node != nil && node.Kind == graph.KindFile && node.FilePath == stage.graphPath {
			fileID = node.ID
			break
		}
	}
	if fileID == "" {
		return fallback("accepted_file_node_missing")
	}
	var fileEdges []*graph.Edge
	for _, edge := range stage.result.Edges {
		if edge != nil && edge.From == fileID {
			fileEdges = append(fileEdges, edge)
		}
	}
	fresh := idx.runContractExtractorsForFile(stage.graphPath, stage.src, stage.result.Nodes, fileEdges,
		byLanguage[stage.prepared.lang], stage.result.Tree)
	for _, edge := range stage.result.Edges {
		if record, valid := diContractFromEdge(edge); valid && record.FilePath == stage.graphPath {
			record.RepoPrefix, record.WorkspaceID, record.ProjectID = idx.repoPrefix, idx.workspaceID, idx.projectID
			fresh = append(fresh, record)
		}
	}
	// Reproduce the bounded record enrichment applied before durable owner
	// rows are written. The temporary registry never enters the global cache;
	// only the fresh records' referenced names/IDs are loaded. Shape snapshots
	// and wrapper/spec cohort expansion remain on the ordinary fallback path.
	local := contracts.NewRegistry()
	for _, record := range fresh {
		if record.Type == contracts.ContractHTTP && record.Role == contracts.RoleProvider {
			schemaSource, _ := record.Meta["schema_source"].(string)
			trail, _ := record.Meta["handler_trail"].(string)
			ident, _ := record.Meta["handler_ident"].(string)
			if schemaSource != "extracted" && schemaSource != "partial" && (trail != "" || ident != "") {
				return fallback("cross_file_handler_normalization")
			}
			if len(unresolvedContractBindingNames(record)) != 0 {
				// The legacy body-facts normalizer rereads disk. Until it accepts
				// this prepared source/tree, do not use it as an input proof.
				return fallback("unresolved_schema_inputs")
			}
		}
		// Durable owner payloads explicitly store effective boundaries.
		// Normalize equivalent implicit repo boundaries before comparison.
		record.WorkspaceID, record.ProjectID = record.EffectiveWorkspace(), record.EffectiveProject()
		local.Add(record)
	}
	idx.upgradeContractBareTypeRefs(local)
	idx.resolveProviderHandlers(local)
	idx.resolveCallReturnTypes(local)
	idx.inlineEnvelopeShapes(local)
	fresh = local.All()
	if idx.contractInputError() != nil {
		return fallback("constant_or_name_input_read_failed")
	}
	oldFingerprint, err := contracts.FingerprintRecords(prior)
	if err != nil {
		return fallback("prior_record_fingerprint_failed")
	}
	newFingerprint, err := contracts.FingerprintRecords(fresh)
	if err != nil || oldFingerprint.Full != newFingerprint.Full {
		return fallback("records_changed_or_fingerprint_failed")
	}
	state := &unchangedFileContracts{records: copyContracts(fresh)}
	seen := make(map[string]bool)
	for _, record := range prior {
		if !seen[record.ID] {
			// Projection rows may still belong to an in-memory ancestry layer.
			// AddBatch is allowed to retain and mutate its input, so detach the
			// canonical payload before restoring it into this build.
			target := *projection.Targets[record.ID]
			target.Meta = copyContractMetaMap(target.Meta)
			state.targets = append(state.targets, &target)
			seen[record.ID] = true
		}
	}
	if idx.incrementalUnchangedContracts == nil {
		idx.incrementalUnchangedContracts = make(map[string]*unchangedFileContracts)
	}
	idx.incrementalUnchangedContracts[stage.graphPath] = state
	idx.contractUnchangedFiles++
	idx.contractProofUsed = true
	idx.contractShortcutReasons["unchanged"]++
	return true
}

func contractRecordsForUnchangedFile(p graph.ContractFileProjection, repo, workspace, project, path string) ([]contracts.Contract, bool) {
	var records []contracts.Contract
	seen := make(map[string]bool)
	selectedIDs := make(map[string]bool)
	for _, row := range p.OwnerRows {
		edge := row.Edge
		if edge == nil || edge.FilePath != path || row.RepoPrefix != repo ||
			(edge.Kind != graph.EdgeProvides && edge.Kind != graph.EdgeConsumes) {
			continue
		}
		target := p.Targets[edge.To]
		if target == nil || target.Kind != graph.KindContract {
			return nil, false
		}
		for _, key := range []string{"contract_owner_repo_prefix", "contract_owner_workspace", "contract_owner_project",
			"contract_owner_symbol_id", "contract_owner_type", "contract_owner_confidence", "contract_owner_meta"} {
			if _, present := edge.Meta[key]; !present {
				return nil, false
			}
		}
		for _, key := range []string{"contract_owner_repo_prefix", "contract_owner_workspace", "contract_owner_project",
			"contract_owner_symbol_id", "contract_owner_type"} {
			if _, valid := edge.Meta[key].(string); !valid {
				return nil, false
			}
		}
		if edge.Meta["contract_owner_type"] == "" {
			return nil, false
		}
		switch edge.Meta["contract_owner_confidence"].(type) {
		case float64, float32, int, int64, json.Number:
		default:
			return nil, false
		}
		if raw := edge.Meta["contract_owner_meta"]; raw != nil {
			if _, complete := raw.(map[string]any); !complete {
				return nil, false
			}
		}
		record, complete := contracts.ContractFromOwnerEdge(target, edge, repo, workspace, project)
		if !complete || record.ID == "" || record.RepoPrefix != repo || record.FilePath != path {
			return nil, false
		}
		fingerprint, err := contracts.FingerprintRecords([]contracts.Contract{record})
		if err != nil {
			return nil, false
		}
		if !seen[fingerprint.Full] {
			seen[fingerprint.Full] = true
			records = append(records, record)
			selectedIDs[record.ID] = true
		}
	}
	for _, scalar := range p.ScalarNodes {
		if scalar == nil || scalar.FilePath != path || scalar.RepoPrefix != repo {
			continue
		}
		if !selectedIDs[scalar.ID] {
			// A last-writer canonical row can name this file while every
			// remaining owner is elsewhere. Empty local records must not
			// authorize deleting that shared canonical during file eviction.
			return nil, false
		}
		ownerBacked, _ := scalar.Meta["contract_owner_record"].(bool)
		removed, _ := scalar.Meta["contract_owner_removed"].(bool)
		if !ownerBacked && !removed {
			// Legacy scalar fallback needs its full liveness protocol. Do not
			// manufacture an empty file snapshot from an ambiguous scalar.
			return nil, false
		}
	}
	return records, true
}

func (idx *Indexer) restateUnchangedContracts(stages []*incrementalBatchStage) {
	for _, stage := range stages {
		state := idx.incrementalUnchangedContracts[stage.graphPath]
		if state == nil || len(state.records) == 0 {
			continue
		}
		_, edges, missing := contractGraphRows(idx.graph, state.records, true)
		if missing != 0 {
			idx.contractRestatementErr = fmt.Errorf("indexer: unchanged contract restatement lost %d source owners in %s", missing, stage.graphPath)
			continue
		}
		// Records are exactly unchanged. Preserve the selected canonical
		// payload and other files' ownership instead of choosing a canonical
		// row from an incomplete local registry or replacing owner groups.
		idx.graph.AddBatch(state.targets, edges)
	}
}

func (idx *Indexer) noteChangedContractDependencyInputs(prior, fresh []*graph.Node) {
	old, oldOK := storedContractDependencyInputs(prior)
	current, currentOK := storedContractDependencyInputs(fresh)
	if !oldOK || !currentOK || old.Constants != current.Constants || old.SharedDeclarations != current.SharedDeclarations || old.CrossFile || current.CrossFile {
		idx.contractDependenciesChanged = true
	}
	// Missing accepted provenance cannot exclude a removed mount or a
	// previous-empty consumer of a changed shared input. Unknown legacy
	// inputs retain the conservative full frontier; complete receipts keep ordinary edits
	// on the local path.
	if !oldOK || !currentOK || old.Constants != current.Constants || old.SharedDeclarations != current.SharedDeclarations || old.CrossFile || current.CrossFile {
		idx.contractSharedInputsChanged = true
	}
}

func (idx *Indexer) checkedContractFileWasAbsent(path string) bool {
	reader, ok := idx.graph.(graph.ContractFileProjectionReader)
	if !ok {
		return false
	}
	ctx := idx.contractProjectionContext
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	p, err := reader.LoadContractFileProjectionContext(ctx, idx.repoPrefix, []string{path})
	idx.contractProjectionTime += time.Since(started)
	if err != nil {
		idx.rememberContractInputError(err)
		return false
	}
	return len(p.FileNodes[path]) == 0 && len(p.ScalarNodes) == 0 && len(p.OwnerRows) == 0 && len(p.OffFileOwnerRows) == 0
}

func acceptedExtractionProducesSharedContractInputs(result *parser.ExtractionResult) bool {
	stamp, complete := storedContractDependencyInputs(result.Nodes)
	if !complete || stamp.CrossFile || len(result.ConstValues) != 0 {
		return true
	}
	emptyShared, ok := contractSharedDeclarationInputs(&parser.ExtractionResult{})
	return !ok || stamp.SharedDeclarations != emptyShared
}
