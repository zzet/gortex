package indexer

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// builderSemanticTarget is the target-side symbol evidence collected while the
// sparse closure already extracts changed files for introduced references.
// Evidence is complete per path: an unsupported, oversized, unreadable, or
// failed extraction deliberately stays incomplete and keeps conservative
// reverse fanout for that path.
type builderSemanticTarget struct {
	files map[string]builderSemanticFile
}

type builderSemanticFile struct {
	shapes   map[string]symbolShape
	complete bool
}

func newBuilderSemanticTarget(present map[string]struct{}) builderSemanticTarget {
	target := builderSemanticTarget{files: make(map[string]builderSemanticFile, len(present))}
	for rel := range present {
		target.files[rel] = builderSemanticFile{}
	}
	return target
}

func (t *builderSemanticTarget) record(rel string, result *parser.ExtractionResult) {
	if t == nil || result == nil {
		return
	}
	adj := symbolShapeAdjacencyFromExtraction(result.Nodes, result.Edges)
	t.files[rel] = builderSemanticFile{
		shapes:   semanticShapeSet(result.Nodes, adj),
		complete: true,
	}
}

// symbolShapeAdjacencyFromExtraction constructs the same bounded shape input
// used by affected-by, directly from one extraction and without a graph write.
func symbolShapeAdjacencyFromExtraction(
	nodes []*graph.Node,
	edges []*graph.Edge,
) symbolShapeAdjacency {
	adj := symbolShapeAdjacency{
		inEdges:  make(map[string][]*graph.Edge),
		outEdges: make(map[string][]*graph.Edge),
		nodes:    make(map[string]*graph.Node, len(nodes)),
	}
	for _, node := range nodes {
		if node != nil && node.ID != "" {
			adj.nodes[node.ID] = node
		}
	}
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		if edge.From != "" {
			adj.outEdges[edge.From] = append(adj.outEdges[edge.From], edge)
		}
		if edge.To != "" {
			adj.inEdges[edge.To] = append(adj.inEdges[edge.To], edge)
		}
	}
	return adj
}

type builderSemanticSeeds struct {
	all     []string
	reverse []string
}

// builderSemanticSeedNodeIDs returns every seed identity for forward closure
// walking and the conservative subset whose incoming references must be
// re-derived. A complete target extraction may suppress reverse fanout only for
// old referenceable symbols whose stable key, kind, and public shape are
// unchanged. Deleted paths and every incomplete or unshapeable case retain the
// old full-fanout behavior.
func builderSemanticSeedNodeIDs(
	ctx context.Context,
	req BuildRequest,
	seeds []string,
	deleted map[string]struct{},
	target builderSemanticTarget,
) (builderSemanticSeeds, error) {
	if err := ctx.Err(); err != nil {
		return builderSemanticSeeds{}, err
	}
	nodesByFile := req.Base.GetFileNodesByPaths(seeds)
	if err := ctx.Err(); err != nil {
		return builderSemanticSeeds{}, err
	}

	byPath := make(map[string][]*graph.Node, len(seeds))
	var allNodes []*graph.Node
	var shapeNodes []*graph.Node
	seen := make(map[string]struct{})
	for _, graphPath := range seeds {
		if err := ctx.Err(); err != nil {
			return builderSemanticSeeds{}, err
		}
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			byPath[graphPath] = append(byPath[graphPath], node)
			allNodes = append(allNodes, node)
			if node.Name != "" && graph.IsReferenceableSymbol(node.Kind) {
				shapeNodes = append(shapeNodes, node)
			}
		}
	}

	adj, err := builderLoadSymbolShapeAdjacency(ctx, req.Base, shapeNodes, allNodes)
	if err != nil {
		return builderSemanticSeeds{}, err
	}

	all := make([]string, 0, len(allNodes))
	reverse := make([]string, 0, len(allNodes))
	for _, graphPath := range seeds {
		if err := ctx.Err(); err != nil {
			return builderSemanticSeeds{}, err
		}
		rel, owned := builderRelPath(req.RepoPrefix, graphPath)
		fileEvidence, hasEvidence := target.files[rel]
		_, removed := deleted[rel]
		conservative := !owned || removed || !hasEvidence || !fileEvidence.complete

		changed := make(map[string]struct{})
		if !conservative {
			for _, key := range semanticShapeDelta(
				semanticShapeSet(byPath[graphPath], adj), fileEvidence.shapes,
			) {
				changed[key] = struct{}{}
			}
		}
		for _, node := range byPath[graphPath] {
			all = append(all, node.ID)
			if conservative || node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
				reverse = append(reverse, node.ID)
				continue
			}
			if _, shapeChanged := changed[stableSymbolKey(node)]; shapeChanged {
				reverse = append(reverse, node.ID)
			}
		}
	}
	sort.Strings(all)
	sort.Strings(reverse)
	return builderSemanticSeeds{all: all, reverse: reverse}, nil
}

func builderLoadSymbolShapeAdjacency(
	ctx context.Context,
	base LayerBase,
	symbols, knownNodes []*graph.Node,
) (symbolShapeAdjacency, error) {
	ids := make([]string, 0, len(symbols))
	seen := make(map[string]struct{}, len(symbols))
	for _, node := range symbols {
		if err := ctx.Err(); err != nil {
			return symbolShapeAdjacency{}, err
		}
		if node == nil || node.ID == "" {
			continue
		}
		if _, duplicate := seen[node.ID]; duplicate {
			continue
		}
		seen[node.ID] = struct{}{}
		ids = append(ids, node.ID)
	}
	adj := symbolShapeAdjacency{nodes: make(map[string]*graph.Node, len(knownNodes))}
	if len(ids) > 0 {
		adj.inEdges = base.GetInEdgesByNodeIDs(ids)
		if err := ctx.Err(); err != nil {
			return symbolShapeAdjacency{}, err
		}
		adj.outEdges = base.GetOutEdgesByNodeIDs(ids)
		if err := ctx.Err(); err != nil {
			return symbolShapeAdjacency{}, err
		}
	}
	for _, node := range knownNodes {
		if node != nil && node.ID != "" {
			adj.nodes[node.ID] = node
		}
	}
	missingSet := make(map[string]struct{})
	for _, edges := range adj.inEdges {
		for _, edge := range edges {
			if err := ctx.Err(); err != nil {
				return symbolShapeAdjacency{}, err
			}
			if edge == nil || edge.Kind != graph.EdgeParamOf || edge.From == "" {
				continue
			}
			if _, known := adj.nodes[edge.From]; !known {
				missingSet[edge.From] = struct{}{}
			}
		}
	}
	if len(missingSet) > 0 {
		missing := make([]string, 0, len(missingSet))
		for id := range missingSet {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		for id, node := range base.GetNodesByIDs(missing) {
			adj.nodes[id] = node
		}
		if err := ctx.Err(); err != nil {
			return symbolShapeAdjacency{}, err
		}
	}
	return adj, nil
}
