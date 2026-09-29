package indexer

import (
	"fmt"
	"maps"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// Declared closure context, seeded instead of parsed.
//
// A body edit's closure is the edited file plus its declared context: the
// files its resolved references bind into, and the manifests. The declared
// files are read so the edited file's references resolve the way a whole index
// resolves them, and are withheld from the generation afterwards
// (withholdContextPayload): their bytes are unchanged and no declaration they
// hold changed shape, so the layer below already serves their payload.
//
// Parsing them was the whole cost of reading them — on a real repository 25
// declared files for a one-line edit, most of the pass. The layer below holds
// exactly what the parse re-derives for an unchanged file, already resolved
// against the whole repository instead of against a bounded corpus, so the
// pass seeds the declared files' payload from it: every node recorded at each
// path and every edge recorded there out of those nodes. The resolver and
// every subpass then find each declaration where parsing would have put it,
// and the corpus filter withholds the seeded payload again before the drain,
// exactly as it withholds a parsed declared file. What the generation writes
// is unchanged: the edited file's payload.
//
// Two groups stay parsed:
//   - the root manifests (go.mod, package.json, ...): their parse feeds
//     manifest-derived state (module contracts, the resolver's import bridge)
//     that is built from source, not from graph nodes;
//   - every dependent (a file whose own derivation the change can move): it
//     is re-derived and compared, never seeded.
//
// Edges into the seeded nodes from elsewhere are not seeded: they are other
// files' payload, and those files are not part of the pass. Of the edges
// recorded at a declared path, only those whose target the pass corpus holds
// are seeded: an unresolved target would be re-queued for a resolution that
// can only land in withheld payload, and a target outside the corpus (a stdlib
// or dependency stub, a declaration in a file outside the closure) is an
// outcome of the whole index that the pass must not see as evidence.
//
// On by default; GORTEX_CLOSURE_SEED_DECLARED=off restores parsing the
// declared context (the comparison arm). On a real repository (a body edit in
// internal/config/config.go and in internal/mcp/checkout_binding.go) the two
// arms write row-identical node and edge payload and serve a row-identical
// composed view; the seeded arm writes no clone-corpus rows for the context it
// did not parse.
//
// The seed carries full base rows, not endpoint projections: derived passes
// over the pass corpus read edge Meta (receiver stamps, field access), and an
// endpoint-only seed changes what they derive over the declared files (it
// changed the corpus's accesses_field count 331 -> 303 on config.go). The
// edited file's output happened not to move on the two measured edits, but a
// derivation that follows a call into declared context can.

// declaredContextSeedEnabled reports whether declared context is seeded from
// the layer below instead of parsed.
func declaredContextSeedEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_CLOSURE_SEED_DECLARED"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// decideDeclaredSeed returns the repository-relative declared context paths
// the pass seeds instead of parsing, sorted, and the layer below's nodes at
// them. It is decided once, before the pass, and rides on the plan, so both
// context routes (the in-memory filter and the withdrawal) see the same set.
//
// A declared path is seeded only when its withholding is certain: the root
// manifests are always parsed, and so is a declared path whose layer-below
// payload carries a content section (a content body lives in an index the
// withdrawal does not reach, so such a path is kept — and a kept path must be
// re-derived in full, never written as the seeded subset).
func decideDeclaredSeed(req BuildRequest, plan buildPlan) ([]string, map[string][]*graph.Node) {
	if len(plan.declared) == 0 || !declaredContextSeedEnabled() || req.Base == nil {
		return nil, nil
	}
	manifests := make(map[string]struct{})
	for _, manifest := range rootManifests() {
		manifests[path.Clean(manifest.path)] = struct{}{}
	}
	candidates := make([]string, 0, len(plan.declared))
	for rel := range plan.declared {
		if _, isManifest := manifests[path.Clean(rel)]; isManifest {
			continue
		}
		candidates = append(candidates, rel)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Strings(candidates)
	graphPaths := make([]string, 0, len(candidates))
	for _, rel := range candidates {
		graphPaths = append(graphPaths, builderGraphPath(req.RepoPrefix, rel))
	}
	byPath := req.Base.GetFileNodesByPaths(graphPaths)
	var seeded []string
	for i, rel := range candidates {
		content := false
		for _, node := range byPath[graphPaths[i]] {
			if graph.IsContentNode(node) {
				content = true
				break
			}
		}
		if !content {
			seeded = append(seeded, rel)
		}
	}
	return seeded, byPath
}

// parsedPlanPaths is plan.indexed without the seeded paths.
func parsedPlanPaths(indexed, seeded []string) []string {
	if len(seeded) == 0 {
		return indexed
	}
	skip := make(map[string]struct{}, len(seeded))
	for _, rel := range seeded {
		skip[rel] = struct{}{}
	}
	out := make([]string, 0, len(indexed))
	for _, rel := range indexed {
		if _, isSeeded := skip[rel]; isSeeded {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// declaredContextSeed reads the layer below's payload for the seeded paths
// and adds a private copy of it to the pass graph.
func declaredContextSeed(base LayerBase, repoPrefix string, seeded []string, baseNodes map[string][]*graph.Node, logger *zap.Logger) func(graph.Store) error {
	return func(corpus graph.Store) error {
		started := time.Now()
		if len(seeded) == 0 {
			return nil
		}
		if base == nil {
			return fmt.Errorf("no layer below to seed %d declared context files from", len(seeded))
		}
		graphPaths := make([]string, 0, len(seeded))
		atPath := make(map[string]struct{}, len(seeded))
		for _, rel := range seeded {
			graphPath := builderGraphPath(repoPrefix, rel)
			graphPaths = append(graphPaths, graphPath)
			atPath[graphPath] = struct{}{}
		}
		byPath := baseNodes
		if byPath == nil {
			byPath = base.GetFileNodesByPaths(graphPaths)
		}
		nodesRead := time.Since(started)
		var selection map[string]struct{}
		if seedScopeNames() {
			selection = seedSelectionByName(corpus, byPath, graphPaths)
		}
		var nodes []*graph.Node
		var ids []string
		seededIDs := make(map[string]struct{})
		byID := make(map[string]*graph.Node)
		for _, graphPath := range graphPaths {
			for _, node := range byPath[graphPath] {
				if node == nil || node.ID == "" {
					continue
				}
				byID[node.ID] = node
				if selection != nil {
					if _, keep := selection[node.ID]; !keep {
						continue
					}
				}
				nodes = append(nodes, seedNodeCopy(node))
				ids = append(ids, node.ID)
				seededIDs[node.ID] = struct{}{}
			}
		}
		if len(nodes) == 0 {
			return nil
		}
		var edges []*graph.Edge
		edgesStarted := time.Now()
		// Full rows (line, Meta) of every edge recorded at the seeded paths out
		// of a seeded node. A whole-file seed reads them by file; a name-scoped
		// seed reads its (few) sources by node. Both are the same row set.
		var outEdges map[string][]*graph.Edge
		if reader, ok := graph.RecordedEdgesOf(base); ok && selection == nil {
			outEdges = make(map[string][]*graph.Edge)
			for _, edge := range reader.RecordedEdgesAt(graphPaths) {
				if edge == nil {
					continue
				}
				if _, fromSeed := seededIDs[edge.From]; fromSeed {
					outEdges[edge.From] = append(outEdges[edge.From], edge)
				}
			}
		} else {
			outEdges = base.GetOutEdgesByNodeIDs(ids)
			if selection != nil {
				// Type-position closure: a seeded declaration's result, field
				// or embedded type is what a member access through it binds
				// against, so the declared node it names is seeded too (with
				// its own out-edges), until nothing new is reached.
				for frontier := seedTypePositionTargets(outEdges, byID, seededIDs, atPath); len(frontier) > 0; frontier = seedTypePositionTargets(outEdges, byID, seededIDs, atPath) {
					for _, id := range frontier {
						nodes = append(nodes, seedNodeCopy(byID[id]))
						ids = append(ids, id)
						seededIDs[id] = struct{}{}
					}
					for id, list := range base.GetOutEdgesByNodeIDs(frontier) {
						outEdges[id] = list
					}
				}
			}
		}
		edgesRead := time.Since(edgesStarted)
		for _, list := range outEdges {
			for _, edge := range list {
				if edge == nil {
					continue
				}
				if _, recordedHere := atPath[edge.FilePath]; !recordedHere {
					continue
				}
				// Only edges that stay inside the pass corpus: an edge the
				// layer below resolved to an identity the pass does not hold
				// (a stdlib or dependency stub, a declaration in a file outside
				// the closure) is a resolution the parse could not have made
				// against this corpus, and handing it to the pass leaks the
				// whole index's outcome into what the pass derives for the
				// edited file (the extern arm and the function-value
				// synthesizer both bind to identities they see as targets).
				if !seedEdgeTargetInCorpus(edge.To, seededIDs, corpus) {
					continue
				}
				edges = append(edges, seedEdgeCopy(edge))
			}
		}
		addStarted := time.Now()
		corpus.AddBatch(nodes, edges)
		if logger != nil {
			logger.Info("indexer: seeded declared context from the layer below",
				zap.Int("files", len(seeded)),
				zap.Int("nodes", len(nodes)),
				zap.Int("edges", len(edges)),
				zap.Duration("node_read", nodesRead),
				zap.Duration("edge_read", edgesRead),
				zap.Duration("add", time.Since(addStarted)),
				zap.Duration("elapsed", time.Since(started)))
		}
		return nil
	}
}

// seedNodeCopy is a private copy of a node read from the layer below: the pass
// mutates what it holds (metadata stamps, resolution bookkeeping), and a reader
// may hand out shared objects.
func seedNodeCopy(node *graph.Node) *graph.Node {
	out := *node
	if node.Meta != nil {
		out.Meta = maps.Clone(node.Meta)
	}
	return &out
}

func seedEdgeCopy(edge *graph.Edge) *graph.Edge {
	out := *edge
	if edge.Meta != nil {
		out.Meta = maps.Clone(edge.Meta)
	}
	return &out
}

// seedEdgeTargetInCorpus reports whether a declared file's edge lands on an
// identity the pass corpus holds as a file-backed declaration: one of the
// seeded nodes, or a node the parse placed at a path. A pathless identity (a
// stdlib, dependency or placeholder stub) is excluded even when the parse
// happened to mint the same stub: its presence as a resolved target is a
// whole-index outcome.
func seedEdgeTargetInCorpus(target string, seeded map[string]struct{}, corpus graph.Store) bool {
	if _, inSeed := seeded[target]; inSeed {
		return true
	}
	node := corpus.GetNode(target)
	return node != nil && node.FilePath != ""
}

// seedScopeNames reports whether the seed is scoped to the declarations the
// change set names (the default) rather than whole declared files
// (GORTEX_CLOSURE_SEED_SCOPE=files).
func seedScopeNames() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("GORTEX_CLOSURE_SEED_SCOPE")), "files")
}

// seedSelectionByName picks, from the declared files' nodes, the ones the
// change set can bind to: every node whose name the parsed change set
// references (an unresolved placeholder's bare name, or a resolved target's
// last name segment), the owner type of every such method or field, and every
// file and package node (the resolver's directory and package indexes are
// built from them). corpus holds only the parsed change set when the seed
// runs.
func seedSelectionByName(corpus graph.Store, byPath map[string][]*graph.Node, graphPaths []string) map[string]struct{} {
	names := make(map[string]struct{})
	addName := func(id string) {
		if id == "" {
			return
		}
		raw := id
		if graph.IsUnresolvedTarget(id) {
			raw = graph.UnresolvedName(id)
		}
		if name := closureBareName(raw); name != "" {
			names[name] = struct{}{}
		}
		// An import specifier binds to whatever node its path spells — a
		// module file, or a qualified resource (`Service/logger` binds a
		// Kubernetes Service named logger) — so every segment is a name.
		if spec, isImport := strings.CutPrefix(raw, "import::"); isImport {
			for _, segment := range strings.FieldsFunc(spec, func(r rune) bool { return r == '/' || r == '.' || r == ':' }) {
				names[segment] = struct{}{}
			}
		}
	}
	for _, edge := range corpus.AllEdges() {
		if edge != nil {
			addName(edge.From)
			addName(edge.To)
		}
	}
	selection := make(map[string]struct{})
	owners := make(map[string]struct{})
	for _, graphPath := range graphPaths {
		for _, node := range byPath[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			keep := node.Kind == graph.KindFile || node.Kind == graph.KindPackage
			if _, named := names[node.Name]; named {
				keep = true
			}
			if !keep {
				continue
			}
			selection[node.ID] = struct{}{}
			if node.Kind == graph.KindMethod || node.Kind == graph.KindField {
				if _, owner := graph.EnclosingFromID(node.ID, node.Kind); owner != "" {
					owners[owner] = struct{}{}
				}
			}
		}
	}
	// Every interface stays, with its members: structural satisfaction
	// (method-set inference) matches a changed type against interfaces the
	// change set never names. The owner types of seeded members stay too, and
	// so do the members of every kept type, so a method set is never partial.
	for _, graphPath := range graphPaths {
		for _, node := range byPath[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			_, isOwner := owners[node.Name]
			if node.Kind == graph.KindInterface || (isOwner && node.Kind == graph.KindType) {
				selection[node.ID] = struct{}{}
				owners[node.Name] = struct{}{}
			}
		}
	}
	for _, graphPath := range graphPaths {
		for _, node := range byPath[graphPath] {
			if node == nil || node.ID == "" || (node.Kind != graph.KindMethod && node.Kind != graph.KindField) {
				continue
			}
			if _, owner := graph.EnclosingFromID(node.ID, node.Kind); owner != "" {
				if _, kept := owners[owner]; kept {
					selection[node.ID] = struct{}{}
				}
			}
		}
	}
	return selection
}

// seedTypePositionTargets returns the declared, not yet seeded nodes that a
// seeded node's type-position edges recorded at a declared path point at.
func seedTypePositionTargets(outEdges map[string][]*graph.Edge, byID map[string]*graph.Node, seeded map[string]struct{}, atPath map[string]struct{}) []string {
	var out []string
	queued := make(map[string]struct{})
	for from, list := range outEdges {
		if _, isSeeded := seeded[from]; !isSeeded {
			continue
		}
		for _, edge := range list {
			if edge == nil {
				continue
			}
			switch edge.Kind {
			case graph.EdgeReturns, graph.EdgeTypedAs, graph.EdgeInstantiates,
				graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes:
			default:
				continue
			}
			if _, here := atPath[edge.FilePath]; !here {
				continue
			}
			if _, declared := byID[edge.To]; !declared {
				continue
			}
			if _, done := seeded[edge.To]; done {
				continue
			}
			if _, dup := queued[edge.To]; dup {
				continue
			}
			queued[edge.To] = struct{}{}
			out = append(out, edge.To)
		}
	}
	sort.Strings(out)
	return out
}
