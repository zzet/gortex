package indexer

import (
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

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
	changedByID := make(map[string]*graph.Node, len(changedNodes))
	for _, node := range changedNodes {
		changedIDs[node.ID] = struct{}{}
		changedByID[node.ID] = node
	}
	// A changed-file node whose complete capability input is what it was
	// gets its prior rows back (capabilityFullInputSignature) unless an
	// expansion below reaches it; only the others are roots.
	reuse := capabilityReusableNodes(g, prior, changedNodes)
	skipped := 0
	if len(reuse) > 0 {
		kept := seedMethods[:0:0]
		for _, method := range seedMethods {
			if _, reused := reuse[method.ID]; !reused {
				kept = append(kept, method)
			} else {
				skipped++
			}
		}
		seedMethods = kept
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

	capabilityRootsEvaluated.Add(int64(len(roots)))
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
			// The set a changed-file method derives changes only when what
			// it is derived from changes: compare the inputs, not the stored
			// rows, which an older derivation may have written.
			if signature, ok := prior.inputs[root.ID]; ok {
				if signature != capabilityInputSignature(rootOut[root.ID], func(id string) *graph.Node {
					if node := changedByID[id]; node != nil {
						return node
					}
					return writeTargets[id]
				}) {
					expand[root.ID] = struct{}{}
				}
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
			for target := range capabilityFieldWrites(rootOut[root.ID]) {
				old[target] = struct{}{}
			}
		}
		if !sameStringSet(old, newWrites[root.ID]) {
			expand[root.ID] = struct{}{}
		}
	}
	capabilityRootsExpanded.Add(int64(len(expand)))
	if len(expand) > 0 {
		indirect, impacted = indirectMutationEdgesForRoots(g, roots, expand)
	}
	extra := make([]*graph.Node, 0, len(restorationIDs))
	for _, id := range restorationIDs {
		if node := restorationNodes[id]; node != nil {
			extra = append(extra, node)
		}
	}
	for id := range impacted {
		delete(reuse, id)
	}
	for _, spec := range indirect {
		delete(reuse, spec.from)
	}
	// A reused method's indirect mutations are its prior ones, as long as
	// each still stands on a receiver call it makes at that site; its direct
	// rows are derived from its own inputs like every other node's, so a row
	// an older derivation wrote does not survive the save.
	changedStarts := make(map[string]int, len(reuse))
	for _, node := range changedNodes {
		if node != nil {
			changedStarts[node.ID] = node.StartLine
		}
	}
	reusedRows := 0
	for id := range reuse {
		rows := reusableIndirectRows(prior.rows[id], prior.starts[id], reuse[id], changedStarts[id])
		reusedRows += len(rows)
		for _, row := range rows {
			via, _ := row.Meta["via"].(string)
			indirect = append(indirect, indirectMutSpec{from: row.From, to: row.To, file: row.FilePath, via: via, line: row.Line})
		}
	}
	capabilityRowsReused.Add(int64(reusedRows))
	capabilitySourcesReused.Add(int64(skipped))
	return writeCapabilityEdgesForSources(g, changedNodes, extra, indirect, impacted)
}

// reusableIndirectRows is the prior indirect mutations of a method that still
// stand on a receiver call the method makes: the same line relative to the
// method's start (priorStart before the save, start now), and a callee named
// as the row's via. Each row comes back at its site's current line.
func reusableIndirectRows(prior []*graph.Edge, priorStart int, out []*graph.Edge, start int) []*graph.Edge {
	if len(prior) == 0 {
		return nil
	}
	sites := make(map[string]struct{}, len(out))
	for _, edge := range out {
		if edge == nil || edge.Kind != graph.EdgeCalls || edge.Meta == nil {
			continue
		}
		field, _ := edge.Meta["recv_field"].(string)
		self, _ := edge.Meta["recv_self"].(bool)
		if field == "" && !self {
			continue
		}
		sites[strconv.Itoa(edge.Line-start)+"\x00"+bareCallName(edge.To)] = struct{}{}
	}
	var rows []*graph.Edge
	for _, row := range prior {
		if row == nil || row.Kind != graph.EdgeAccessesField || row.Meta == nil {
			continue
		}
		if indirect, _ := row.Meta["indirect"].(bool); !indirect {
			continue
		}
		via, _ := row.Meta["via"].(string)
		if _, ok := sites[strconv.Itoa(row.Line-priorStart)+"\x00"+via]; !ok {
			continue
		}
		if shift := start - priorStart; shift != 0 {
			moved := *row
			moved.Line += shift
			row = &moved
		}
		rows = append(rows, row)
	}
	return rows
}

// capabilitySourcesReused counts the changed-file methods a per-save pass did
// not evaluate as roots (their inputs were unchanged), and
// capabilityRowsReused the indirect mutations it took from the prior for them
// (tests and measurement). capabilityRootsEvaluated counts the roots a pass
// evaluated and capabilityRootsExpanded the ones whose mutated-field set
// changed, so the transitive receiver callers were re-derived.
var capabilitySourcesReused, capabilityRowsReused, capabilityRootsEvaluated, capabilityRootsExpanded atomic.Int64

// capabilityReuseCounts samples the capability reuse counters.
func capabilityReuseCounts() [4]int64 {
	return [4]int64{capabilitySourcesReused.Load(), capabilityRowsReused.Load(), capabilityRootsEvaluated.Load(), capabilityRootsExpanded.Load()}
}

// capabilityReusableNodes returns the changed-file nodes whose complete
// capability input equals the prior's (same identity, same receiver, same
// reads, writes, config reads and calls at the same sites), each with its
// current outgoing rows.
func capabilityReusableNodes(g graph.Store, prior *capabilityPrior, changedNodes []*graph.Node) map[string][]*graph.Edge {
	if prior == nil || len(prior.fullInputs) == 0 || len(changedNodes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(changedNodes))
	for _, node := range changedNodes {
		if _, ok := prior.fullInputs[node.ID]; ok {
			ids = append(ids, node.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := g.GetOutEdgesByNodeIDs(ids)
	reuse := make(map[string][]*graph.Edge, len(ids))
	for _, node := range changedNodes {
		signature, ok := prior.fullInputs[node.ID]
		if !ok || signature != capabilityFullInputSignature(node, out[node.ID]) {
			continue
		}
		reuse[node.ID] = out[node.ID]
	}
	return reuse
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
		// The whole-index derivation keeps, per (source, target, kind), the
		// input row first in capabilityRepresentativeLess order; visiting the
		// inputs in that order makes add keep the same row here.
		inputs := append([]*graph.Edge(nil), adjacency[source]...)
		sort.SliceStable(inputs, func(i, j int) bool {
			return capabilityRepresentativeLess(inputs[i], inputs[j])
		})
		for _, edge := range inputs {
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
	g.AddBatch(nodes, collapseCapabilityIdentities(edges))
	return readsEnv, execProc, fieldAccess
}

// collapseCapabilityIdentities keeps one row per stored edge identity (from,
// to, kind, path, line). Two receiver calls on one line that both mutate a
// field (`s.a(); s.b()` on one line) derive two indirect rows the store
// keeps under one identity, and which one it kept depended on the order the
// writer listed them — the whole-index pass and the per-save pass listed
// them differently. The row with the smallest via wins, in both.
func collapseCapabilityIdentities(edges []*graph.Edge) []*graph.Edge {
	type identity struct {
		from, to, file string
		kind           graph.EdgeKind
		line           int
	}
	at := make(map[identity]int, len(edges))
	out := edges[:0:0]
	via := func(e *graph.Edge) string {
		v, _ := e.Meta["via"].(string)
		return v
	}
	for _, e := range edges {
		key := identity{from: e.From, to: e.To, file: e.FilePath, kind: e.Kind, line: e.Line}
		if i, dup := at[key]; dup {
			if via(e) < via(out[i]) {
				out[i] = e
			}
			continue
		}
		at[key] = len(out)
		out = append(out, e)
	}
	return out
}

// capabilityRepresentativeLess is the whole-index capability pass's order
// over the input rows of one source (synthesizeCapabilityEdges' lessSource
// within a repository): target, then input kind (a read before a write),
// then recorded path, then line.
func capabilityRepresentativeLess(a, b *graph.Edge) bool {
	if a == nil || b == nil {
		return a != nil
	}
	if a.To != b.To {
		return a.To < b.To
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.FilePath != b.FilePath {
		return a.FilePath < b.FilePath
	}
	return a.Line < b.Line
}
