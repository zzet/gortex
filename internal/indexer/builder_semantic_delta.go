package indexer

import (
	"context"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

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
	nodes, edges := storedExtractionIdentities(result.Nodes, result.Edges)
	adj := symbolShapeAdjacencyFromExtraction(nodes, edges)
	t.files[rel] = builderSemanticFile{
		shapes:   semanticShapeSet(nodes, adj),
		complete: true,
	}
}

// storedExtractionIdentities is an extraction as a graph store holds it: one
// node per id and one edge per logical edge identity (from, to, kind, file,
// line), the later write replacing the earlier one, which is what both the
// SQLite store and the in-memory graph do on a re-add. The target-side shapes
// must be computed from that, because the layer-below shapes they are compared
// with were: an extraction that emits two edges with one identity — the two
// `returns string` edges of `(ref, commit string, err error)`, both on the
// signature's line — or two nodes with one id would otherwise differ from the
// stored shape of an unchanged declaration, and every referrer of it would be
// re-derived as a dependent.
func storedExtractionIdentities(nodes []*graph.Node, edges []*graph.Edge) ([]*graph.Node, []*graph.Edge) {
	nodeAt := make(map[string]int, len(nodes))
	outNodes := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			continue
		}
		if i, seen := nodeAt[node.ID]; seen {
			outNodes[i] = node
			continue
		}
		nodeAt[node.ID] = len(outNodes)
		outNodes = append(outNodes, node)
	}
	type edgeIdentity struct {
		from, to string
		kind     graph.EdgeKind
		file     string
		line     int
	}
	edgeAt := make(map[edgeIdentity]int, len(edges))
	outEdges := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		key := edgeIdentity{from: edge.From, to: edge.To, kind: edge.Kind, file: edge.FilePath, line: edge.Line}
		if i, seen := edgeAt[key]; seen {
			outEdges[i] = edge
			continue
		}
		edgeAt[key] = len(outEdges)
		outEdges = append(outEdges, edge)
	}
	return outNodes, outEdges
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
		importersUnmoved := false
		if !conservative {
			before := semanticShapeSet(byPath[graphPath], adj)
			for _, key := range semanticShapeDelta(before, fileEvidence.shapes) {
				changed[key] = struct{}{}
			}
			importersUnmoved = introducesNothingImportable(rel, before, fileEvidence.shapes)
		}
		for _, node := range byPath[graphPath] {
			all = append(all, node.ID)
			if importersUnmoved && node.Kind == graph.KindFile {
				// A file node is bound by path: an import, include or
				// relative reference resolves to it through the file's
				// place in the tree, never its content, and a modified
				// file keeps its place (a deleted one is conservative).
				// What an importer can newly bind through it is a name the
				// change introduces; when there is none an importer can
				// reach, the importers of the package whose anchor file
				// this is are not moved by the edit (a removed or reshaped
				// declaration reaches its referrers through its own node).
				continue
			}
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

// introducesNothingImportable reports whether a modified file's new shape set
// adds no declaration another package could bind to through an import of it:
// every key it adds is, for a Go file, an unexported name (reachable only from
// inside its own package, whose files do not import it). For any other
// language every added declaration counts, so only a change that adds nothing
// qualifies. An importer that already names what the change adds — a call
// `b.New()` parked on an external-call placeholder until b defines New — is
// re-derived through the file node when this reports false.
func introducesNothingImportable(rel string, before, after map[string]symbolShape) bool {
	goFile := strings.HasSuffix(rel, ".go")
	for key := range after {
		if _, existed := before[key]; existed {
			continue
		}
		if !goFile {
			return false
		}
		name := stableSymbolKeyName(key)
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(name); unicode.IsUpper(r) {
			return false
		}
	}
	return true
}
