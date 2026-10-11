package resolver

import "github.com/zzet/gortex/internal/graph"

// Receiver-type gating for C# member-call attribution.
//
// The extractor stamps Meta["receiver_type"] on a member-call candidate when
// the local type environment knows the receiver. When such a call cannot bind
// to a member of that exact type (nor of a base/interface it derives from) and
// a weak resolver tier falls back to a same-named member on an *unrelated*
// type, the attribution is wrong: an edge that names its receiver type must not
// attach to a same-named member of an unrelated type. This pass demotes those
// edges to the speculative tier so they drop out of every default query and
// min_tier filter — while a genuine inherited / interface-dispatch call (where
// the target's receiver is a super-type of the receiver_type) and a valid
// extension-method binding are both preserved, so the gate adds no false
// negatives.
//
// Demotions are persisted with one exact-identity ReindexEdges batch. That
// updates detached SQLite edge copies without the coarse (from,to,kind)
// RemoveEdge operation, so legitimate sibling call sites remain untouched.

// demoteCSharpMisattributedMemberCalls demotes weak-tier C# member calls whose
// bound target belongs to a type unrelated to the edge's receiver_type. Returns
// the number of edges demoted.
func demoteCSharpMisattributedMemberCalls(g graph.Store) int {
	return demoteCSharpMisattributedMemberCallsScoped(g, nil)
}

// demoteCSharpMisattributedMemberCallsScoped evaluates only calls sourced by a
// changed repository or targeting one of its changed methods. Endpoint, type,
// and hierarchy state is fetched in bounded batches; a nil scope preserves the
// full/cold whole-graph candidate set.
func demoteCSharpMisattributedMemberCallsScoped(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}
	var calls []*graph.Edge
	if scope == nil {
		calls = csharpReceiverGateProjectedCalls(g)
	} else {
		calls = frameworkCallsForScope(g, scope)
	}
	return demoteCSharpMisattributedMemberCallCandidates(g, calls, scope, false)
}

// csharpReceiverGateProjectedCalls selects only call identities carrying the
// receiver_type marker used by the gate. The projection cursor is exhausted
// before exact-refetching current full edges, so SQLite store re-entry is safe
// and opaque edge metadata is preserved for mutation.
func csharpReceiverGateProjectedCalls(g graph.Store) []*graph.Edge {
	identities := make([]graph.EdgeIdentity, 0)
	seen := make(map[graph.EdgeIdentity]struct{})
	for row := range graph.FrameworkCensusEdgesSeq(g, graph.EdgeCalls) {
		if row.ReceiverType == "" {
			continue
		}
		if _, duplicate := seen[row.EdgeIdentity]; duplicate {
			continue
		}
		seen[row.EdgeIdentity] = struct{}{}
		identities = append(identities, row.EdgeIdentity)
	}

	current := findFrameworkEdgesByIdentities(g, identities)
	calls := make([]*graph.Edge, 0, len(identities))
	for _, identity := range identities {
		edge := current[identity]
		if edge == nil || edge.Kind != graph.EdgeCalls || edge.Meta == nil {
			continue
		}
		receiverType, _ := edge.Meta["receiver_type"].(string)
		if receiverType != "" {
			calls = append(calls, edge)
		}
	}
	return calls
}

func demoteCSharpMisattributedMemberCallsScopedForFiles(
	g graph.Store,
	scope map[string]bool,
	filePaths []string,
	csharpHierarchyChanged bool,
) int {
	if g == nil {
		return 0
	}
	if len(filePaths) == 0 || csharpHierarchyChanged {
		return demoteCSharpMisattributedMemberCallsScoped(g, scope)
	}
	return demoteCSharpMisattributedMemberCallCandidates(
		g, csharpCallCandidatesForFiles(g, scope, filePaths), scope, true,
	)
}

func demoteCSharpMisattributedMemberCallCandidates(
	g graph.Store,
	calls []*graph.Edge,
	scope map[string]bool,
	exactHierarchy bool,
) int {
	if g == nil || len(calls) == 0 {
		return 0
	}
	endpointIDs := make([]string, 0, len(calls)*2)
	for _, edge := range calls {
		if edge != nil {
			endpointIDs = append(endpointIDs, edge.From, edge.To)
		}
	}
	nodes := g.GetNodesByIDs(endpointIDs)

	// Only type names present on candidate receiver/target pairs can affect a
	// demotion verdict. Resolve all of them through one name-index query.
	typeNames := make([]string, 0)
	seenNames := make(map[string]bool)
	for _, edge := range calls {
		if edge == nil || edge.Meta == nil {
			continue
		}
		if receiver, _ := edge.Meta["receiver_type"].(string); receiver != "" && !seenNames[receiver] {
			seenNames[receiver] = true
			typeNames = append(typeNames, receiver)
		}
		if target := nodes[edge.To]; target != nil && target.Meta != nil {
			if receiver, _ := target.Meta["receiver"].(string); receiver != "" && !seenNames[receiver] {
				seenNames[receiver] = true
				typeNames = append(typeNames, receiver)
			}
		}
	}
	byName := g.FindNodesByNames(typeNames)
	nameToTypeIDs := map[string][]string{}
	typeNameByID := map[string]string{}
	hierarchyRepos := map[string]bool{}
	hierarchyRoots := make([]*graph.Node, 0)
	for name, matches := range byName {
		for _, node := range matches {
			if node == nil || node.Language != "csharp" ||
				(node.Kind != graph.KindType && node.Kind != graph.KindInterface) {
				continue
			}
			nameToTypeIDs[name] = append(nameToTypeIDs[name], node.ID)
			typeNameByID[node.ID] = node.Name
			hierarchyRepos[node.RepoPrefix] = true
			hierarchyRoots = append(hierarchyRoots, node)
		}
	}
	if len(nameToTypeIDs) == 0 {
		return 0
	}

	up := map[string][]string{}
	// incompleteHier[name] marks a C# type that declares a base or interface the
	// index could not resolve (an external assembly, a generic type parameter) —
	// its hierarchy is only partially known, so an "unrelated to the target"
	// verdict for a receiver of that type is unreliable.
	incompleteHier := map[string]bool{}
	recordHierarchyEdge := func(edge *graph.Edge) {
		if edge == nil || edge.From == "" {
			return
		}
		if graph.IsUnresolvedTarget(edge.To) {
			if name := typeNameByID[edge.From]; name != "" {
				incompleteHier[name] = true
				return
			}
			if from := nodes[edge.From]; from != nil && from.Language == "csharp" && from.Name != "" {
				incompleteHier[from.Name] = true
			}
			return
		}
		up[edge.From] = append(up[edge.From], edge.To)
	}

	switch {
	case exactHierarchy:
		hierarchyEdges, hierarchyNodes := csharpHierarchyClosure(g, hierarchyRoots)
		for id, node := range hierarchyNodes {
			nodes[id] = node
		}
		for _, edge := range hierarchyEdges {
			recordHierarchyEdge(edge)
		}
	case scope == nil:
		// Full reconciliation needs only hierarchy identities. Stream the
		// metadata-free projection to completion before any later store re-entry;
		// relevant unresolved sources are named by the already-hydrated type set.
		for edge := range graph.EdgesLightSeq(g, graph.EdgeExtends, graph.EdgeImplements) {
			recordHierarchyEdge(edge)
		}
	default:
		hierarchyEdges := frameworkRepoEdges(
			g, hierarchyRepos, graph.EdgeExtends, graph.EdgeImplements,
		)
		hierarchyNodeIDs := make([]string, 0, len(hierarchyEdges)*2)
		for _, edge := range hierarchyEdges {
			if edge != nil {
				hierarchyNodeIDs = append(hierarchyNodeIDs, edge.From)
				if !graph.IsUnresolvedTarget(edge.To) {
					hierarchyNodeIDs = append(hierarchyNodeIDs, edge.To)
				}
			}
		}
		for id, node := range g.GetNodesByIDs(hierarchyNodeIDs) {
			nodes[id] = node
		}
		for _, edge := range hierarchyEdges {
			recordHierarchyEdge(edge)
		}
	}

	reindex := make([]graph.EdgeReindex, 0)
	for _, edge := range calls {
		if !csharpShouldDemote(nodes, edge, nameToTypeIDs, up, incompleteHier) {
			continue
		}
		edge.Origin = graph.OriginSpeculative
		if edge.Meta == nil {
			edge.Meta = map[string]any{}
		}
		edge.Meta[graph.MetaSpeculative] = true
		edge.Meta["demoted"] = "receiver_type_mismatch"
		reindex = append(reindex, graph.EdgeReindex{
			Edge: edge, OldFrom: edge.From, OldTo: edge.To,
			OldFilePath: edge.FilePath, OldLine: edge.Line, RefreshIdentity: true,
		})
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return len(reindex)
}

func csharpCallCandidatesForFiles(g graph.Store, scope map[string]bool, filePaths []string) []*graph.Edge {
	prefixes := frameworkScopePrefixes(scope)
	seen := make(map[graph.EdgeIdentity]struct{})
	var projected []*graph.Edge
	for row := range graph.EdgesInScopeSeq(g, prefixes, filePaths, graph.EdgeCalls) {
		projected = appendUniqueFrameworkEdges(projected, seen, row.Edge)
	}

	methodIDs := make([]string, 0)
	// With a file frontier, the same rows and order as graph.NodesInScopeSeq
	// read through the file index (frameworkFileFrontierNodes).
	methods := graph.NodesInScopeSeq(g, prefixes, filePaths, graph.KindMethod)
	if len(filePaths) > 0 {
		methods = frameworkFileFrontierNodes(g, prefixes, filePaths, graph.KindMethod)
	}
	for node := range methods {
		if node != nil {
			methodIDs = append(methodIDs, node.ID)
		}
	}
	incoming := g.GetInEdgesByNodeIDs(methodIDs)
	for _, methodID := range methodIDs {
		for _, edge := range incoming[methodID] {
			if edge != nil && edge.Kind == graph.EdgeCalls {
				projected = appendUniqueFrameworkEdges(projected, seen, edge)
			}
		}
	}

	identities := make([]graph.EdgeIdentity, 0, len(projected))
	for _, edge := range projected {
		identities = append(identities, graph.EdgeIdentityFor(edge))
	}
	current := findFrameworkEdgesByIdentities(g, identities)
	calls := make([]*graph.Edge, 0, len(identities))
	for _, identity := range identities {
		if edge := current[identity]; edge != nil && edge.Kind == graph.EdgeCalls {
			calls = append(calls, edge)
		}
	}
	return calls
}

func csharpHierarchyClosure(g graph.Store, roots []*graph.Node) ([]*graph.Edge, map[string]*graph.Node) {
	nodes := make(map[string]*graph.Node, len(roots))
	seenNodes := make(map[string]struct{}, len(roots))
	queue := make([]string, 0, len(roots))
	for _, node := range roots {
		if node == nil || node.ID == "" {
			continue
		}
		if _, duplicate := seenNodes[node.ID]; duplicate {
			continue
		}
		seenNodes[node.ID] = struct{}{}
		nodes[node.ID] = node
		queue = append(queue, node.ID)
	}

	seenEdges := make(map[graph.EdgeIdentity]struct{})
	var edges []*graph.Edge
	for len(queue) > 0 {
		batch := queue
		queue = nil
		bySource := g.GetOutEdgesByNodeIDs(batch)
		targetIDs := make([]string, 0)
		requestedTargets := make(map[string]struct{})
		for _, sourceID := range batch {
			for _, edge := range bySource[sourceID] {
				if edge == nil || (edge.Kind != graph.EdgeExtends && edge.Kind != graph.EdgeImplements) {
					continue
				}
				identity := graph.EdgeIdentityFor(edge)
				if _, duplicate := seenEdges[identity]; !duplicate {
					seenEdges[identity] = struct{}{}
					edges = append(edges, edge)
				}
				if edge.To == "" || graph.IsUnresolvedTarget(edge.To) {
					continue
				}
				if _, visited := seenNodes[edge.To]; visited {
					continue
				}
				if _, requested := requestedTargets[edge.To]; requested {
					continue
				}
				requestedTargets[edge.To] = struct{}{}
				targetIDs = append(targetIDs, edge.To)
			}
		}
		if len(targetIDs) == 0 {
			continue
		}
		fetched := g.GetNodesByIDs(targetIDs)
		for _, targetID := range targetIDs {
			node := fetched[targetID]
			if node == nil || node.Language != "csharp" ||
				(node.Kind != graph.KindType && node.Kind != graph.KindInterface) {
				continue
			}
			if _, visited := seenNodes[targetID]; visited {
				continue
			}
			seenNodes[targetID] = struct{}{}
			nodes[targetID] = node
			queue = append(queue, targetID)
		}
	}
	return edges, nodes
}

// csharpShouldDemote reports whether a resolved C# member-call edge is a
// same-named-unrelated-type misattribution that should be demoted.
func csharpShouldDemote(nodes map[string]*graph.Node, e *graph.Edge, nameToTypeIDs, up map[string][]string, incompleteHier map[string]bool) bool {
	if e == nil || e.Meta == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
		return false
	}
	rt, _ := e.Meta["receiver_type"].(string)
	if rt == "" {
		return false
	}
	// Only the weak tiers are gated; never demote ast_resolved / lsp evidence.
	// An empty Origin resolves to its confidence-derived tier.
	if graph.OriginRank(e.EffectiveOrigin()) > graph.OriginRank(graph.OriginASTInferred) {
		return false
	}
	caller := nodes[e.From]
	if caller == nil || caller.Language != "csharp" {
		return false
	}
	target := nodes[e.To]
	if target == nil || target.Kind != graph.KindMethod || target.Language != "csharp" || target.Meta == nil {
		return false
	}
	// A valid extension binding names the extension's static host class as
	// the target receiver, which is by definition unrelated to the receiver
	// it extends — never demote an extension target, however it was
	// reached. The exemption belongs to the TARGET, not to the edge's
	// resolution tag: a stale `extension_method` tag can survive a restub
	// onto a plain method, and exempting on the tag alone would disable
	// the gate for exactly the edges most likely to be misattributed.
	if isCSharpExtension(target) {
		return false
	}
	tr, _ := target.Meta["receiver"].(string)
	if tr == "" || tr == rt {
		return false
	}
	// Only demote when both endpoints are known indexed types — otherwise we
	// cannot establish that the mismatch is a genuinely unrelated-type
	// misattribution, and keeping the edge avoids a false negative.
	if len(nameToTypeIDs[rt]) == 0 || len(nameToTypeIDs[tr]) == 0 {
		return false
	}
	// A receiver whose own hierarchy is incompletely indexed may reach the
	// target through the unindexed base/interface, so the "unrelated" verdict is
	// unreliable — keep rather than demote a possibly-legitimate polymorphic
	// call. This is the same conservatism as the both-endpoints-known guard
	// above, extended to hierarchy completeness.
	if incompleteHier[rt] {
		return false
	}
	// A related receiver (the target lives on a base type / interface the
	// receiver_type derives from) is a legitimate polymorphic call — keep.
	return !csharpTypesRelated(nameToTypeIDs, up, rt, tr)
}

// csharpTypesRelated reports whether type names a and b are related through the
// C# type hierarchy in either direction (one derives from / implements the
// other, transitively).
func csharpTypesRelated(nameToTypeIDs, up map[string][]string, a, b string) bool {
	if a == b {
		return true
	}
	return csharpNameReaches(nameToTypeIDs, up, a, b) || csharpNameReaches(nameToTypeIDs, up, b, a)
}

// csharpNameReaches reports whether any type named `from` reaches any type named
// `to` by following super-type / interface (up) edges transitively.
func csharpNameReaches(nameToTypeIDs, up map[string][]string, from, to string) bool {
	targets := map[string]bool{}
	for _, id := range nameToTypeIDs[to] {
		targets[id] = true
	}
	if len(targets) == 0 {
		return false
	}
	visited := map[string]bool{}
	queue := append([]string{}, nameToTypeIDs[from]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		for _, p := range up[cur] {
			if targets[p] {
				return true
			}
			if !visited[p] {
				queue = append(queue, p)
			}
		}
	}
	return false
}
