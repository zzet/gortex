package indexer

import (
	"context"
	"encoding/json"
	"github.com/zzet/gortex/internal/graph"
)

// The private context marker narrows only the captured core's physical edge
// projection. ReadCoreFile wrappers still observe every read; custom readers
// may retain the complete checked fallback. Full enrichment never sets it.
type contractExtractionCoreContextKey struct{}

// The fixed per-file extractor list in buildPerFileContractExtractors consumes
// source and nodes, not edges. Extraction retains only these DI/Spring edges
// for the unit's enrichment seeds. Full nodes and full retained edge payloads
// remain proof inputs; common file loads and generated-core proofs stay full.
func contractUnitSeedEdge(edge *graph.Edge) bool {
	if edge == nil {
		return false
	}
	if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
		_, di := diContractFromEdge(edge)
		return di
	}
	return edge.Kind == graph.EdgeCalls && edge.Meta["via"] == "spring.Bean"
}

// loadExtractionFile is used only by ordinary per-file extraction, whose
// returned inputs are exactly those recorded here. Enrichment uses loadFile.
func (e *contractFollowupEvidence) loadExtractionFile(path string) ([]*graph.Node, []*graph.Edge, error) {
	e.ledger.owner(path)
	e.ledger.key("path:", path)
	defer e.guard.enter()()
	nodes, edges, err := e.loadFileRowsContext(path, context.WithValue(e.ctx, contractExtractionCoreContextKey{}, true), true)
	if err != nil {
		return nil, nil, err
	}
	var seeds []*graph.Edge
	for _, edge := range edges {
		if contractUnitSeedEdge(edge) {
			seeds = append(seeds, edge)
		}
	}
	e.ledger.read(contractReadExtractionFile, []string{path}, contractFileAnswer{Nodes: nodes, Edges: seeds}, path)
	return nodes, seeds, nil
}

// A replay question needs the selected input, not a second scratch import.
// All rows and metadata still participate in the same answer digest and caps.
func (e *contractFollowupEvidence) readExtractionFileAnswer(path string) ([]*graph.Node, []*graph.Edge, error) {
	e.ledger.owner(path)
	e.ledger.key("path:", path)
	defer e.guard.enter()()
	if e.err != nil {
		return nil, nil, e.err
	}
	if err := e.ctx.Err(); err != nil {
		e.fail(err)
		return nil, nil, err
	}
	ctx := context.WithValue(e.ctx, contractExtractionCoreContextKey{}, true)
	nodes, edges, err := e.readSelectedFileRowsContext(path, ctx)
	if err != nil {
		return nil, nil, err
	}
	// Preserve the exact bound formerly checked by the scratch import, including
	// ordinary call rows that are not part of the returned extraction seeds.
	if err = contractEvidenceRowsWithinLimit(nodes, edges); err != nil {
		e.fail(err)
		return nil, nil, err
	}
	var seeds []*graph.Edge
	for _, edge := range edges {
		if contractUnitSeedEdge(edge) {
			seeds = append(seeds, edge)
		}
	}
	if err = e.ctx.Err(); err != nil {
		e.fail(err)
		return nil, nil, err
	}
	e.ledger.read(contractReadExtractionFile, []string{path}, contractFileAnswer{Nodes: nodes, Edges: seeds}, path)
	return nodes, seeds, nil
}

func contractEvidenceRowsWithinLimit(nodes []*graph.Node, edges []*graph.Edge) error {
	encodedRows, err := json.Marshal(struct {
		Nodes []*graph.Node
		Edges []*graph.Edge
	}{nodes, edges})
	if err != nil {
		return err
	}
	if len(encodedRows) > contractFollowupCompactLimit {
		return graph.ErrContractProjectionLimit
	}
	return nil
}
