package indexer

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// synthesizeCapabilityEdgesForFiles is the exact-file partial counterpart to
// the global capability pass. It reconciles changed sources and only the
// transitive receiver callers whose indirect-mutation truth depends on them.
// The caller holds ResolveMutex.
func synthesizeCapabilityEdgesForFiles(
	g graph.Store,
	changedFiles []string,
) (readsEnv, execProc, fieldAccess int) {
	files := make([]string, 0, len(changedFiles))
	seenFiles := make(map[string]struct{}, len(changedFiles))
	for _, file := range changedFiles {
		if file == "" {
			continue
		}
		if _, seen := seenFiles[file]; seen {
			continue
		}
		seenFiles[file] = struct{}{}
		files = append(files, file)
	}
	if len(files) == 0 {
		return 0, 0, 0
	}

	fileNodes := g.GetFileNodesByPaths(files)
	changedNodes := make([]*graph.Node, 0)
	seedMethods := make([]*graph.Node, 0)
	for _, file := range files {
		for _, node := range fileNodes[file] {
			if node == nil {
				continue
			}
			changedNodes = append(changedNodes, node)
			if node.Kind == graph.KindMethod {
				seedMethods = append(seedMethods, node)
			}
		}
	}
	if len(changedNodes) == 0 {
		return 0, 0, 0
	}
	indirect, impactedMethods := indirectMutationEdgesForMethods(g, seedMethods)
	return writeCapabilityEdgesForSources(g, changedNodes, nil, indirect, impactedMethods)
}

// changedFileCapabilityNodes returns the current nodes of the changed files and
// the methods among them.
func changedFileCapabilityNodes(g graph.Store, changedFiles []string) ([]string, []*graph.Node, []*graph.Node) {
	files := make([]string, 0, len(changedFiles))
	seenFiles := make(map[string]struct{}, len(changedFiles))
	for _, file := range changedFiles {
		if file == "" {
			continue
		}
		if _, seen := seenFiles[file]; seen {
			continue
		}
		seenFiles[file] = struct{}{}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, nil, nil
	}
	fileNodes := g.GetFileNodesByPaths(files)
	changedNodes := make([]*graph.Node, 0)
	seedMethods := make([]*graph.Node, 0)
	for _, file := range files {
		for _, node := range fileNodes[file] {
			if node == nil {
				continue
			}
			changedNodes = append(changedNodes, node)
			if node.Kind == graph.KindMethod {
				seedMethods = append(seedMethods, node)
			}
		}
	}
	return files, changedNodes, seedMethods
}

// synthesizeCapabilityEdgesForFilesWithPrior is synthesizeCapabilityEdgesForFiles
// bounded by the capability state the mutation read before evicting the files.
// It re-derives (identity argument per group):
//
//   - every node of the changed files: their capability edges were deleted with
//     them and their inputs are the fresh extraction;
//   - every source outside the files whose accesses_field edges INTO the files
//     the eviction deleted (prior.restoration): its inputs (its own reads/writes
//     and receiver calls) are intact, so re-deriving it restores exactly the
//     rows its whole-index derivation holds;
//   - the transitive receiver callers of a root (a changed-file method or a
//     restored method) only when that root's mutated-field set or receiver
//     changed. A caller reads a callee's summary only through that set
//     (receiver-field calls test "does the callee mutate anything", receiver
//     calls copy the callee's fields; both filter by receiver), so when no
//     root's set changed every caller's stored edges already equal its
//     derivation and re-deriving it rewrites identical rows. A method with no
//     prior row (added) counts as changed.
//
// Before this bound every save re-derived every transitive receiver caller of
// every method of the file (5,280 edges per save of a large Server file) and
// evicted and re-inserted all of them. The caller holds ResolveMutex.
func synthesizeCapabilityEdgesForFilesWithPrior(
	g graph.Store,
	prior *capabilityPrior,
	changedFiles []string,
) (readsEnv, execProc, fieldAccess int) {
	if g == nil {
		return 0, 0, 0
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	_, changedNodes, seedMethods := changedFileCapabilityNodes(g, changedFiles)
	if len(changedNodes) == 0 && len(prior.restoration) == 0 {
		return 0, 0, 0
	}
	changedIDs := make(map[string]struct{}, len(changedNodes))
	for _, node := range changedNodes {
		changedIDs[node.ID] = struct{}{}
	}
	restorationIDs := make([]string, 0, len(prior.restoration))
	for id := range prior.restoration {
		if _, inside := changedIDs[id]; !inside {
			restorationIDs = append(restorationIDs, id)
		}
	}
	sort.Strings(restorationIDs)
	restorationNodes := g.GetNodesByIDs(restorationIDs)
	roots := append([]*graph.Node(nil), seedMethods...)
	for _, id := range restorationIDs {
		if node := restorationNodes[id]; node != nil && node.Kind == graph.KindMethod {
			roots = append(roots, node)
		}
	}

	// Evaluate the roots alone (forward receiver callees only).
	indirect, impacted := indirectMutationEdgesForRoots(g, roots, map[string]struct{}{})
	rootIDs := make([]string, 0, len(roots))
	for _, root := range roots {
		rootIDs = append(rootIDs, root.ID)
	}
	rootOut := g.GetOutEdgesByNodeIDs(rootIDs)
	writeTargetIDs := make([]string, 0)
	seenTargets := make(map[string]struct{})
	for _, id := range rootIDs {
		for _, edge := range rootOut[id] {
			if edge == nil || edge.Kind != graph.EdgeWrites {
				continue
			}
			if _, seen := seenTargets[edge.To]; !seen {
				seenTargets[edge.To] = struct{}{}
				writeTargetIDs = append(writeTargetIDs, edge.To)
			}
		}
	}
	writeTargets := g.GetNodesByIDs(writeTargetIDs)
	newWrites := make(map[string]map[string]struct{}, len(roots))
	for _, id := range rootIDs {
		set := make(map[string]struct{})
		for _, edge := range rootOut[id] {
			if edge == nil || edge.Kind != graph.EdgeWrites {
				continue
			}
			if field := writeTargets[edge.To]; field != nil && field.Kind == graph.KindField {
				set[edge.To] = struct{}{}
			}
		}
		newWrites[id] = set
	}
	for _, spec := range indirect {
		if set := newWrites[spec.from]; set != nil {
			set[spec.to] = struct{}{}
		}
	}
	expand := make(map[string]struct{})
	for _, root := range roots {
		var old map[string]struct{}
		if _, inside := changedIDs[root.ID]; inside {
			if _, existed := prior.priorIDs[root.ID]; !existed {
				expand[root.ID] = struct{}{}
				continue
			}
			receiver, _ := root.Meta["receiver"].(string)
			if prior.receivers[root.ID] != receiver {
				expand[root.ID] = struct{}{}
				continue
			}
			old = prior.writes[root.ID]
		} else {
			// A restored source keeps its accesses_field edges to targets
			// outside the files; the eviction deleted the ones inside.
			old = make(map[string]struct{})
			for target := range prior.restoration[root.ID] {
				old[target] = struct{}{}
			}
			for _, edge := range rootOut[root.ID] {
				if edge != nil && edge.Kind == graph.EdgeAccessesField && capabilityAccessIsWrite(edge) {
					old[edge.To] = struct{}{}
				}
			}
		}
		if !sameStringSet(old, newWrites[root.ID]) {
			expand[root.ID] = struct{}{}
		}
	}
	if len(expand) > 0 {
		indirect, impacted = indirectMutationEdgesForRoots(g, roots, expand)
	}
	extra := make([]*graph.Node, 0, len(restorationIDs))
	for _, id := range restorationIDs {
		if node := restorationNodes[id]; node != nil {
			extra = append(extra, node)
		}
	}
	return writeCapabilityEdgesForSources(g, changedNodes, extra, indirect, impacted)
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

// writeCapabilityEdgesForSources re-derives the direct capability edges of
// changedNodes, extraSources and impactedMethods, adds the indirect mutations,
// and replaces those sources' capability rows. The caller holds ResolveMutex.
func writeCapabilityEdgesForSources(
	g graph.Store,
	changedNodes []*graph.Node,
	extraSources []*graph.Node,
	indirect []indirectMutSpec,
	impactedMethods map[string]*graph.Node,
) (readsEnv, execProc, fieldAccess int) {
	sourceSet := make(map[string]struct{}, len(changedNodes)+len(extraSources)+len(impactedMethods))
	sourceIDs := make([]string, 0, len(changedNodes)+len(extraSources)+len(impactedMethods))
	for _, group := range [][]*graph.Node{changedNodes, extraSources} {
		for _, node := range group {
			if node == nil {
				continue
			}
			if _, seen := sourceSet[node.ID]; seen {
				continue
			}
			sourceSet[node.ID] = struct{}{}
			sourceIDs = append(sourceIDs, node.ID)
		}
	}
	impactedIDs := make([]string, 0, len(impactedMethods))
	for id := range impactedMethods {
		impactedIDs = append(impactedIDs, id)
	}
	sort.Strings(impactedIDs)
	for _, id := range impactedIDs {
		if _, seen := sourceSet[id]; seen {
			continue
		}
		sourceSet[id] = struct{}{}
		sourceIDs = append(sourceIDs, id)
	}
	if len(sourceIDs) == 0 {
		return 0, 0, 0
	}
	adjacency := g.GetOutEdgesByNodeIDs(sourceIDs)

	fieldTargetIDs := make([]string, 0)
	seenFieldTargets := make(map[string]struct{})
	for _, source := range sourceIDs {
		for _, edge := range adjacency[source] {
			if edge == nil || (edge.Kind != graph.EdgeReads && edge.Kind != graph.EdgeWrites) {
				continue
			}
			if _, seen := seenFieldTargets[edge.To]; seen {
				continue
			}
			seenFieldTargets[edge.To] = struct{}{}
			fieldTargetIDs = append(fieldTargetIDs, edge.To)
		}
	}
	fieldTargets := g.GetNodesByIDs(fieldTargetIDs)

	type edgeSpec struct {
		from, to, origin, file string
		line                   int
		kind                   graph.EdgeKind
		meta                   map[string]any
	}
	pending := make([]edgeSpec, 0)
	seen := make(map[string]bool)
	add := func(from, to string, kind graph.EdgeKind, origin, file string, line int, meta map[string]any) bool {
		key := string(kind) + "\x00" + from + "\x00" + to
		if via, _ := meta["via"].(string); via != "" {
			key += "\x00" + via
		}
		if seen[key] {
			return false
		}
		seen[key] = true
		pending = append(pending, edgeSpec{
			from: from, to: to, kind: kind, origin: origin, file: file, line: line, meta: meta,
		})
		return true
	}
	procNodes := make(map[string]*graph.Node)
	for _, source := range sourceIDs {
		for _, edge := range adjacency[source] {
			if edge == nil {
				continue
			}
			switch edge.Kind {
			case graph.EdgeReadsConfig:
				if strings.Contains(edge.To, "cfg::env::") && add(
					edge.From, edge.To, graph.EdgeReadsEnv, graph.OriginASTResolved,
					edge.FilePath, edge.Line, nil,
				) {
					readsEnv++
				}
			case graph.EdgeReads, graph.EdgeWrites:
				field := fieldTargets[edge.To]
				if field == nil || field.Kind != graph.KindField {
					continue
				}
				access := "read"
				if edge.Kind == graph.EdgeWrites {
					access = "write"
				}
				if add(edge.From, edge.To, graph.EdgeAccessesField, graph.OriginASTResolved,
					edge.FilePath, edge.Line, map[string]any{"access": access}) {
					fieldAccess++
				}
			case graph.EdgeCalls:
				mechanism := processExecMechanism(edge.To)
				if mechanism == "" {
					continue
				}
				procID := "string::process::" + mechanism
				if procNodes[procID] == nil {
					procNodes[procID] = &graph.Node{
						ID: procID, Kind: graph.KindString, Name: mechanism,
						Meta: map[string]any{"context": "process", "mechanism": mechanism},
					}
				}
				if add(edge.From, procID, graph.EdgeExecutesProcess, graph.OriginASTInferred,
					edge.FilePath, edge.Line, nil) {
					execProc++
				}
			}
		}
	}
	for _, spec := range indirect {
		if add(spec.from, spec.to, graph.EdgeAccessesField, graph.OriginASTInferred,
			spec.file, spec.line, map[string]any{
				"access": "write", "indirect": true, "via": spec.via,
			}) {
			fieldAccess++
		}
	}

	_, supported, err := graph.EvictEdgesFromSourcesByKindsBackground(g, sourceIDs, []graph.EdgeKind{
		graph.EdgeReadsEnv, graph.EdgeExecutesProcess, graph.EdgeAccessesField,
	})
	if err != nil || !supported {
		return 0, 0, 0
	}
	nodes := make([]*graph.Node, 0, len(procNodes))
	for _, node := range procNodes {
		nodes = append(nodes, node)
	}
	edges := make([]*graph.Edge, 0, len(pending))
	for _, spec := range pending {
		edges = append(edges, &graph.Edge{
			From: spec.from, To: spec.to, Kind: spec.kind,
			FilePath: spec.file, Line: spec.line, Origin: spec.origin, Meta: spec.meta,
		})
	}
	g.AddBatch(nodes, edges)
	return readsEnv, execProc, fieldAccess
}
