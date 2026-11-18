package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// InferImplementsForFrontier adds exactly the EdgeImplements edges
// InferImplementsScoped(frontier, frontier) adds, reading only the frontier's
// own adjacency instead of every member_of and implements edge of the frontier's
// repositories. The pairs that pass examines are (type, interface) with either
// side in the frontier, within one repository:
//
//   - a frontier type needs its own method set (its incoming member_of edges)
//     and the repository's interfaces (the same interface census);
//   - a frontier interface needs every type of its repository owning all of its
//     methods; each such type owns the interface's first method, so its
//     candidates are the owners of the methods with that name.
//
// An existing implements edge is recognised from the candidate type's own
// out-edges, which is where the scoped pass's repository census finds it. The
// per-save hierarchy pass paid 4-12 s streaming the repository's member_of
// edges for a one-file frontier.
func (r *Resolver) InferImplementsForFrontier(frontier map[string]bool) int {
	ids := sortedFrontierIDs(frontier)
	if len(ids) == 0 {
		return 0
	}
	repos := implementationInferenceRepos(r.graph, frontier, frontier)
	if len(repos) == 0 {
		return 0
	}
	ifacesByRepo := collectImplementationInterfaces(r.graph, repos)
	if len(ifacesByRepo) == 0 {
		return 0
	}
	repoSet := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		repoSet[repo] = struct{}{}
	}

	// Candidate owners: the frontier's own types and interfaces, plus the
	// owners of each frontier interface's first method.
	candidateOwners := make(map[string]struct{})
	frontierIfaces := make([]implementationInterface, 0)
	ifaceRepo := make(map[string]string)
	for repo, ifaces := range ifacesByRepo {
		for _, iface := range ifaces {
			ifaceRepo[iface.id] = repo
			if frontier[iface.id] {
				frontierIfaces = append(frontierIfaces, iface)
			}
		}
	}
	for _, id := range ids {
		candidateOwners[id] = struct{}{}
	}
	if len(frontierIfaces) > 0 {
		anchors := make([]string, 0, len(frontierIfaces))
		seenAnchor := make(map[string]struct{})
		for _, iface := range frontierIfaces {
			if _, seen := seenAnchor[iface.methods[0]]; !seen {
				seenAnchor[iface.methods[0]] = struct{}{}
				anchors = append(anchors, iface.methods[0])
			}
		}
		methodIDs := make([]string, 0)
		for _, nodes := range r.graph.FindNodesByNames(anchors) {
			for _, node := range nodes {
				if node == nil || node.Kind != graph.KindMethod {
					continue
				}
				if _, ok := repoSet[node.RepoPrefix]; !ok {
					continue
				}
				methodIDs = append(methodIDs, node.ID)
			}
		}
		for _, edges := range r.graph.GetOutEdgesByNodeIDs(methodIDs) {
			for _, edge := range edges {
				if edge != nil && edge.Kind == graph.EdgeMemberOf {
					candidateOwners[edge.To] = struct{}{}
				}
			}
		}
	}
	ownerIDs := make([]string, 0, len(candidateOwners))
	for id := range candidateOwners {
		ownerIDs = append(ownerIDs, id)
	}
	sort.Strings(ownerIDs)
	types := r.frontierMemberMethodSets(ownerIDs, repoSet, func(owner *graph.Node) bool {
		return len(ifacesByRepo[owner.RepoPrefix]) > 0
	})
	if len(types) == 0 {
		return 0
	}

	existing := make(map[string]struct{})
	typeIDs := make([]string, 0, len(types))
	for id := range types {
		typeIDs = append(typeIDs, id)
	}
	sort.Strings(typeIDs)
	for _, edges := range r.graph.GetOutEdgesByNodeIDs(typeIDs) {
		for _, edge := range edges {
			if edge != nil && edge.Kind == graph.EdgeImplements {
				existing[inferencePairKey(edge.From, edge.To)] = struct{}{}
			}
		}
	}

	var batch []*graph.Edge
	added := 0
	emit := func(typeInfo *implementationType, iface implementationInterface) {
		if iface.id == typeInfo.node.ID {
			return
		}
		for _, required := range iface.methods {
			if _, ok := typeInfo.methods[required]; !ok {
				return
			}
		}
		key := inferencePairKey(typeInfo.node.ID, iface.id)
		if _, found := existing[key]; found {
			return
		}
		existing[key] = struct{}{}
		batch = append(batch, &graph.Edge{
			From:     typeInfo.node.ID,
			To:       iface.id,
			Kind:     graph.EdgeImplements,
			FilePath: typeInfo.node.FilePath,
			Line:     typeInfo.node.StartLine,
			Meta:     map[string]any{"via": MetaViaMethodSetInference},
		})
		added++
	}
	for _, typeID := range typeIDs {
		typeInfo := types[typeID]
		for _, iface := range ifacesByRepo[typeInfo.node.RepoPrefix] {
			if frontier[typeID] || frontier[iface.id] {
				emit(typeInfo, iface)
			}
		}
	}
	for start := 0; start < len(batch); start += inferenceMutationBatchSize {
		end := min(start+inferenceMutationBatchSize, len(batch))
		r.graph.AddBatch(nil, batch[start:end])
	}
	return added
}

// frontierMemberMethodSets builds the implementation-type records of ownerIDs
// the way collectImplementationTypes builds them from the repository's
// member_of stream: an owner is a type or interface admitted by keep, and its
// methods are the method-kind sources of its member_of edges whose repository
// is in repos.
func (r *Resolver) frontierMemberMethodSets(
	ownerIDs []string,
	repos map[string]struct{},
	keep func(*graph.Node) bool,
) map[string]*implementationType {
	if len(ownerIDs) == 0 {
		return nil
	}
	owners := r.graph.GetNodesByIDs(ownerIDs)
	incoming := r.graph.GetInEdgesByNodeIDs(ownerIDs)
	methodIDs := make([]string, 0)
	seenMethod := make(map[string]struct{})
	for _, id := range ownerIDs {
		for _, edge := range incoming[id] {
			if edge == nil || edge.Kind != graph.EdgeMemberOf {
				continue
			}
			if _, seen := seenMethod[edge.From]; !seen {
				seenMethod[edge.From] = struct{}{}
				methodIDs = append(methodIDs, edge.From)
			}
		}
	}
	methods := r.graph.GetNodesByIDs(methodIDs)
	out := make(map[string]*implementationType)
	for _, id := range ownerIDs {
		owner := owners[id]
		if owner == nil || (owner.Kind != graph.KindType && owner.Kind != graph.KindInterface) || !keep(owner) {
			continue
		}
		for _, edge := range incoming[id] {
			if edge == nil || edge.Kind != graph.EdgeMemberOf {
				continue
			}
			method := methods[edge.From]
			if method == nil || method.Kind != graph.KindMethod {
				continue
			}
			if _, ok := repos[method.RepoPrefix]; !ok {
				continue
			}
			typeInfo := out[id]
			if typeInfo == nil {
				typeInfo = &implementationType{node: owner, methods: make(map[string]struct{})}
				out[id] = typeInfo
			}
			typeInfo.methods[method.Name] = struct{}{}
		}
	}
	return out
}

// InferOverridesForFrontier adds exactly the EdgeOverrides edges (and origin
// upgrades) InferOverridesScoped(frontier) makes, reading only the structural
// parent edges incident to the frontier and the member methods of the types
// they connect, instead of every member_of and parent edge of the frontier's
// repositories. When a connected type has two member methods of one name, the
// scoped pass keeps the one with the later edge row, an order the adjacency
// reads do not carry; that frontier runs the scoped pass instead.
func (r *Resolver) InferOverridesForFrontier(frontier map[string]bool) int {
	ids := sortedFrontierIDs(frontier)
	if len(ids) == 0 {
		return 0
	}
	repos := overrideInferenceRepos(r.graph, frontier)
	if len(repos) == 0 {
		return 0
	}
	repoSet := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		repoSet[repo] = struct{}{}
	}

	out := r.graph.GetOutEdgesByNodeIDs(ids)
	in := r.graph.GetInEdgesByNodeIDs(ids)
	type parentRow struct {
		from, to, origin string
	}
	seenEdge := make(map[graph.EdgeIdentity]struct{})
	var parentEdges []*graph.Edge
	endpointIDs := make([]string, 0)
	seenEndpoint := make(map[string]struct{})
	for _, id := range ids {
		for _, edges := range [][]*graph.Edge{out[id], in[id]} {
			for _, edge := range edges {
				if edge == nil || !isOverrideParentKind(edge.Kind) || edge.From == edge.To {
					continue
				}
				identity := graph.EdgeIdentityFor(edge)
				if _, dup := seenEdge[identity]; dup {
					continue
				}
				seenEdge[identity] = struct{}{}
				parentEdges = append(parentEdges, edge)
				for _, endpoint := range []string{edge.From, edge.To} {
					if _, seen := seenEndpoint[endpoint]; !seen {
						seenEndpoint[endpoint] = struct{}{}
						endpointIDs = append(endpointIDs, endpoint)
					}
				}
			}
		}
	}
	if len(parentEdges) == 0 {
		return 0
	}
	endpoints := r.graph.GetNodesByIDs(endpointIDs)
	var rows []parentRow
	methodRepos := make(map[string]struct{}, len(repoSet))
	for repo := range repoSet {
		methodRepos[repo] = struct{}{}
	}
	typeIDSet := make(map[string]struct{})
	for _, edge := range parentEdges {
		source, target := endpoints[edge.From], endpoints[edge.To]
		if source == nil || target == nil {
			continue
		}
		if _, ok := repoSet[source.RepoPrefix]; !ok {
			continue
		}
		row := graph.ScopedEdgeRow{Edge: edge, Source: source, Target: target}
		if !relevantOverrideParent(row, frontier) {
			continue
		}
		methodRepos[target.RepoPrefix] = struct{}{}
		rows = append(rows, parentRow{from: edge.From, to: edge.To, origin: edge.Origin})
		typeIDSet[edge.From] = struct{}{}
		typeIDSet[edge.To] = struct{}{}
	}
	if len(rows) == 0 {
		return 0
	}
	typeIDs := make([]string, 0, len(typeIDSet))
	for id := range typeIDSet {
		typeIDs = append(typeIDs, id)
	}
	sort.Strings(typeIDs)
	incoming := r.graph.GetInEdgesByNodeIDs(typeIDs)
	methodIDs := make([]string, 0)
	seenMethod := make(map[string]struct{})
	for _, id := range typeIDs {
		for _, edge := range incoming[id] {
			if edge != nil && edge.Kind == graph.EdgeMemberOf {
				if _, seen := seenMethod[edge.From]; !seen {
					seenMethod[edge.From] = struct{}{}
					methodIDs = append(methodIDs, edge.From)
				}
			}
		}
	}
	methods := r.graph.GetNodesByIDs(methodIDs)
	methodsByType := make(map[string]map[string]*graph.Node, len(typeIDs))
	for _, id := range typeIDs {
		for _, edge := range incoming[id] {
			if edge == nil || edge.Kind != graph.EdgeMemberOf {
				continue
			}
			method := methods[edge.From]
			if method == nil || method.Kind != graph.KindMethod {
				continue
			}
			if _, ok := methodRepos[method.RepoPrefix]; !ok {
				continue
			}
			byName := methodsByType[id]
			if byName == nil {
				byName = make(map[string]*graph.Node)
				methodsByType[id] = byName
			}
			if prior := byName[method.Name]; prior != nil && prior.ID != method.ID {
				// Duplicate member names: the scoped pass's winner is the later
				// member_of row, which only its ordered stream knows.
				return r.InferOverridesScoped(frontier)
			}
			byName[method.Name] = method
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	added := 0
	candidates := make([]overrideCandidate, 0, inferenceMutationBatchSize)
	flush := func() {
		added += r.flushOverrideCandidates(candidates)
		candidates = make([]overrideCandidate, 0, inferenceMutationBatchSize)
	}
	for _, row := range rows {
		childMethods := methodsByType[row.from]
		parentMethods := methodsByType[row.to]
		if len(childMethods) == 0 || len(parentMethods) == 0 {
			continue
		}
		names := make([]string, 0, len(childMethods))
		for name := range childMethods {
			names = append(names, name)
		}
		sort.Strings(names)
		origin := overrideOrigin(row.origin)
		for _, name := range names {
			child, parent := childMethods[name], parentMethods[name]
			if child == nil || parent == nil || parent.ID == child.ID {
				continue
			}
			candidates = append(candidates, overrideCandidate{from: child, to: parent, origin: origin})
			if len(candidates) == inferenceMutationBatchSize {
				flush()
			}
		}
	}
	flush()
	return added
}

func sortedFrontierIDs(frontier map[string]bool) []string {
	ids := make([]string, 0, len(frontier))
	for id, affected := range frontier {
		if affected && id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
