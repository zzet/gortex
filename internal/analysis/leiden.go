package analysis

import (
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// DetectCommunitiesLeiden runs the Leiden algorithm (Traag, Waltman
// & van Eck, 2019) on the same weighted graph that DetectCommunities
// uses for Louvain. Differences from our Louvain implementation:
//
//  1. Fast local moves — a queue-based loop that only re-examines a
//     node when one of its neighbours changed community. Converges
//     in fewer iterations than Louvain's full-pass scan.
//
//  2. Refinement phase — after local moves stabilise, each
//     community is re-clustered internally by running a sub-pass of
//     local moves restricted to that community. This produces
//     refined sub-communities that are guaranteed to be at least as
//     well-connected as the originals, and crucially lets the next
//     aggregation level move *whole sub-communities* across phase-1
//     boundaries — moves single-node Louvain cannot express.
//
//  3. Iteration — aggregation builds a meta-graph from the refined
//     sub-communities, with each meta-node initialised into the
//     phase-1 community its members belonged to. We then re-run
//     phase 1 + refine + aggregate on the meta-graph until no
//     further coarsening helps.
//
// Result has the same shape as DetectCommunities so the call site
// can swap them out without other changes.
func DetectCommunitiesLeiden(g graph.Store) *CommunityResult {
	result, _ := detectCommunitiesLeidenRaw(g, defaultLeidenOptions(), nil)
	return result
}

// LeidenOptions tunes the Leiden community detector.
type LeidenOptions struct {
	// Resolution (γ) scales the expected-edges (null-model) penalty in
	// the modularity-gain calculation. The standard modularity score is
	//
	//	Q = Σ_c ( e_in[c]/m − γ·(Σtot[c]/2m)² )
	//
	// so γ multiplies the null-model term only. γ = 1.0 is plain
	// modularity (the historical default). Higher γ makes the penalty
	// larger, so the optimizer favours smaller, denser communities;
	// lower γ favours fewer, larger ones. Zero or negative values are
	// treated as the 1.0 default.
	Resolution float64

	// Note: only the modularity objective is supported. A Constant
	// Potts Model (CPM) objective — which replaces the degree-based
	// null model with a per-pair γ penalty driven by community node
	// counts — would need node-weight bookkeeping threaded through the
	// refinement and aggregation phases (which currently track only
	// degree sums and edge weights), so it is intentionally left as a
	// follow-up rather than shipped partially.
}

// defaultLeidenOptions returns the options that reproduce the
// historical Leiden behaviour exactly: γ = 1.0 (a no-op multiply on
// the penalty term, so the partition is bit-identical to the
// pre-resolution implementation).
func defaultLeidenOptions() LeidenOptions {
	return LeidenOptions{Resolution: 1.0}
}

// resolution returns the effective γ, normalising the zero value and
// any non-positive input to the 1.0 default.
func (o LeidenOptions) resolution() float64 {
	if o.Resolution <= 0 {
		return 1.0
	}
	return o.Resolution
}

// DetectCommunitiesLeidenWith runs the Leiden pipeline with explicit
// options (resolution γ). Passing defaultLeidenOptions() — or an
// options value whose Resolution is 1.0 — yields a partition that is
// byte-identical to DetectCommunitiesLeiden.
func DetectCommunitiesLeidenWith(g graph.Store, opts LeidenOptions) *CommunityResult {
	result, _ := detectCommunitiesLeidenRaw(g, opts, nil)
	return result
}

// detectCommunitiesLeidenRaw runs the full Leiden pipeline and
// returns both the labelled CommunityResult and the raw partition
// (original-node-id → stable raw community key, pre-renumbering)
// alongside the weighted adjacency it was computed on. The raw
// partition is what the incremental path caches and re-seeds from:
// the public CommunityResult only carries renumbered "community-N"
// ids and drops singletons, neither of which can drive a restricted
// re-optimization. The returned partition is nil when the graph has
// no clustering-relevant edges (the result is then empty too).
func detectCommunitiesLeidenRaw(g graph.Store, opts LeidenOptions, pace *Pace) (*CommunityResult, *leidenPartition) {
	resolution := opts.resolution()
	lg := buildLeidenGraph(g, pace)
	if lg == nil {
		return &CommunityResult{NodeToComm: make(map[string]string)}, nil
	}
	symbolNodes := lg.symbolNodes
	neighbors := lg.neighbors
	totalWeight := lg.totalWeight
	degree := lg.degree

	// Per-iteration state. Each iteration shrinks the graph by
	// replacing nodes with refined sub-communities. We keep
	// `origPartition` updated as the iteration descends so the
	// final result is expressed in terms of original node ids.
	currentNodes := sortedKeys(symbolNodes)
	currentNbrs := neighbors
	currentDeg := degree
	currentTotal := totalWeight

	currentComm := make(map[string]string, len(currentNodes))
	for _, id := range currentNodes {
		currentComm[id] = id
	}

	// origPartition[origID] = current-iteration node-id the orig belongs to.
	origPartition := make(map[string]string, len(currentNodes))
	for _, id := range currentNodes {
		origPartition[id] = id
	}

	const maxIters = 12
	for iter := 0; iter < maxIters; iter++ {
		// Phase 1: fast local moves.
		currentComm = leidenFastLocalMoves(currentNodes, currentNbrs, currentDeg, currentTotal, currentComm, resolution, pace)

		// Phase 2: refinement. Each phase-1 community is internally
		// re-clustered by running local moves on the induced sub-graph.
		refined := leidenRefine(currentComm, currentNbrs, currentDeg, resolution, pace)

		// If refinement didn't merge anything (every refined comm is
		// a singleton w.r.t. current nodes), no further aggregation
		// can help — we're done.
		if !willAggregate(currentNodes, refined) {
			break
		}

		// Phase 3: aggregate the graph based on refined sub-communities.
		newNodes, newComm, newNbrs, newDeg, newTotal := leidenAggregate(
			currentNodes, currentComm, refined, currentNbrs, pace,
		)

		// Propagate origPartition through this aggregation.
		for orig, cur := range origPartition {
			origPartition[orig] = refined[cur]
		}

		currentNodes, currentComm, currentNbrs, currentDeg, currentTotal = newNodes, newComm, newNbrs, newDeg, newTotal
	}

	// Map each original node back to its final community via the
	// origPartition trail.
	finalComm := make(map[string]string, len(symbolNodes))
	for orig, cur := range origPartition {
		finalComm[orig] = currentComm[cur]
	}

	// Renumber, build Community structs, label, disambiguate, group
	// — same downstream pipeline as Louvain so the result is
	// indistinguishable in shape.
	result := buildCommunityResult(g, finalComm, neighbors, totalWeight, degree, pace)
	return result, &leidenPartition{
		comm:        finalComm,
		neighbors:   neighbors,
		degree:      degree,
		totalWeight: totalWeight,
		symbolNodes: symbolNodes,
	}
}

// leidenFastLocalMoves is the queue-based phase-1 routine. Each
// node starts in `initial` community; moves are taken whenever they
// improve modularity, and any node whose community changed pushes
// its neighbours back onto the work queue so they get to react.
// Returns the final node → community map.
func leidenFastLocalMoves(
	nodeIDs []string,
	neighbors map[string]map[string]float64,
	degree map[string]float64,
	totalWeight float64,
	initial map[string]string,
	resolution float64,
	pace *Pace,
) map[string]string {
	comm := make(map[string]string, len(nodeIDs))
	commMembers := make(map[string]map[string]bool)
	sigmaTot := make(map[string]float64)
	for _, id := range nodeIDs {
		cid := initial[id]
		comm[id] = cid
		if commMembers[cid] == nil {
			commMembers[cid] = make(map[string]bool)
		}
		commMembers[cid][id] = true
		sigmaTot[cid] += degree[id]
	}

	// Queue + in-queue flag avoids duplicates. Start with every
	// node so the first pass evaluates each one.
	queue := make([]string, len(nodeIDs))
	copy(queue, nodeIDs)
	inQueue := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		inQueue[id] = true
	}

	for len(queue) > 0 {
		pace.Tick()
		id := queue[0]
		queue = queue[1:]
		delete(inQueue, id)

		currentComm := comm[id]
		commWeights := make(map[string]float64)
		for nbr, w := range neighbors[id] {
			commWeights[comm[nbr]] += w
		}
		ki := degree[id]
		kiIn := commWeights[currentComm]
		// Subtract the self-loop (one direction) since it shouldn't
		// participate in the move calculation — it stays with the node.
		if loop, ok := neighbors[id][id]; ok {
			kiIn -= loop
		}
		// γ (resolution) scales the expected-edges (null-model) penalty
		// only; the actual-edges terms (kiIn, wc) are untouched. With
		// resolution == 1.0 the multiply is the IEEE-754 identity, so
		// the default partition is bit-identical to the unscaled form.
		removeDelta := kiIn - resolution*((sigmaTot[currentComm]-ki)*ki/(2*totalWeight))

		bestComm := currentComm
		bestGain := 0.0
		for c, wc := range commWeights {
			if c == currentComm {
				continue
			}
			gain := wc - resolution*(sigmaTot[c]*ki/(2*totalWeight)) - removeDelta
			if gain > bestGain {
				bestGain = gain
				bestComm = c
			}
		}

		if bestComm == currentComm {
			continue
		}

		// Apply move.
		delete(commMembers[currentComm], id)
		if len(commMembers[currentComm]) == 0 {
			delete(commMembers, currentComm)
		}
		sigmaTot[currentComm] -= ki

		comm[id] = bestComm
		if commMembers[bestComm] == nil {
			commMembers[bestComm] = make(map[string]bool)
		}
		commMembers[bestComm][id] = true
		sigmaTot[bestComm] += ki

		// Wake up any neighbour that isn't already queued — their
		// modularity-best community might have changed.
		for nbr := range neighbors[id] {
			if nbr == id {
				continue
			}
			if !inQueue[nbr] {
				queue = append(queue, nbr)
				inQueue[nbr] = true
			}
		}
	}

	return comm
}

// leidenRefine performs Leiden's refinement step. For each phase-1
// community, we extract the induced sub-graph and run a fresh
// local-moves pass on it (starting from singletons) — that finds
// sub-communities within the original cluster. The returned map is
// nodeID → refinedSubCommID. Singletons in the input map to
// themselves.
//
// Refinement is constrained to stay within phase-1 communities by
// construction (we only look at intra-community edges).
func leidenRefine(
	comm map[string]string,
	neighbors map[string]map[string]float64,
	degree map[string]float64,
	resolution float64,
	pace *Pace,
) map[string]string {
	byComm := make(map[string][]string)
	for id, cid := range comm {
		byComm[cid] = append(byComm[cid], id)
	}

	refined := make(map[string]string, len(comm))
	for _, members := range byComm {
		pace.Tick()
		if len(members) == 1 {
			refined[members[0]] = members[0]
			continue
		}

		memberSet := make(map[string]bool, len(members))
		for _, id := range members {
			memberSet[id] = true
		}
		// Induced sub-graph: edges from `members` to `members`.
		subNbrs := make(map[string]map[string]float64, len(members))
		subDeg := make(map[string]float64, len(members))
		var subTotal float64
		for _, id := range members {
			subNbrs[id] = make(map[string]float64)
			for nbr, w := range neighbors[id] {
				if !memberSet[nbr] {
					continue
				}
				subNbrs[id][nbr] = w
				subDeg[id] += w
				subTotal += w
			}
		}
		subTotal /= 2

		if subTotal == 0 {
			// No intra-community edges to optimise on — each member
			// becomes its own refined sub-community. Rare; mostly
			// happens with single isolated edges between cohesive
			// blocks.
			for _, id := range members {
				refined[id] = id
			}
			continue
		}

		// Sort for deterministic visitation.
		sort.Strings(members)
		// Singleton initial partition for the refinement pass.
		init := make(map[string]string, len(members))
		for _, id := range members {
			init[id] = id
		}
		subComm := leidenFastLocalMoves(members, subNbrs, subDeg, subTotal, init, resolution, pace)
		for _, id := range members {
			refined[id] = subComm[id]
		}

		// Silence unused-variable lint for degree on the off-chance
		// some compiler enforces it; we keep `degree` in the signature
		// for parity with the louvain helpers.
		_ = degree[members[0]]
	}
	return refined
}

// willAggregate reports whether the refined partition has any
// non-singleton communities — if every refined sub-comm contains
// exactly one current node, aggregation would produce the same graph
// and we'd loop forever.
func willAggregate(nodes []string, refined map[string]string) bool {
	count := make(map[string]int, len(nodes))
	for _, id := range nodes {
		count[refined[id]]++
		if count[refined[id]] >= 2 {
			return true
		}
	}
	return false
}

// leidenAggregate builds the meta-graph for the next iteration.
// Each refined sub-community becomes one meta-node. Meta-edges sum
// the underlying edge weights. Crucially the *meta-community*
// initialisation comes from the phase-1 partition, not from the
// refined one — that's what lets the next phase-1 pass discover
// merges between phase-1 communities by moving whole sub-comms.
func leidenAggregate(
	nodes []string,
	comm map[string]string,
	refined map[string]string,
	neighbors map[string]map[string]float64,
	pace *Pace,
) (
	newNodes []string,
	newComm map[string]string,
	newNbrs map[string]map[string]float64,
	newDeg map[string]float64,
	newTotal float64,
) {
	// Discover all meta-nodes (refined sub-comm ids).
	metaSet := make(map[string]bool)
	memberOf := make(map[string][]string) // meta-node → its current nodes
	for _, id := range nodes {
		r := refined[id]
		metaSet[r] = true
		memberOf[r] = append(memberOf[r], id)
	}

	newNodes = make([]string, 0, len(metaSet))
	for r := range metaSet {
		newNodes = append(newNodes, r)
	}
	sort.Strings(newNodes)

	// Initial meta-community = phase-1 community of any member.
	newComm = make(map[string]string, len(newNodes))
	for r, members := range memberOf {
		newComm[r] = comm[members[0]]
	}

	// Aggregate edge weights.
	newNbrs = make(map[string]map[string]float64, len(newNodes))
	newDeg = make(map[string]float64, len(newNodes))
	for src, srcNbrs := range neighbors {
		pace.Tick()
		srcMeta := refined[src]
		if _, ok := metaSet[srcMeta]; !ok {
			continue
		}
		if newNbrs[srcMeta] == nil {
			newNbrs[srcMeta] = make(map[string]float64)
		}
		for dst, w := range srcNbrs {
			dstMeta := refined[dst]
			newNbrs[srcMeta][dstMeta] += w
			newDeg[srcMeta] += w
		}
	}
	for _, w := range newDeg {
		newTotal += w
	}
	newTotal /= 2

	return
}

// buildCommunityResult turns a final node → community map into a
// CommunityResult of the same shape as Louvain returns. Re-uses the
// label / hub / disambiguation / parent-grouping pipeline so the UI
// can render Leiden output identically.
func buildCommunityResult(
	g graph.Store,
	finalComm map[string]string,
	neighbors map[string]map[string]float64,
	totalWeight float64,
	degree map[string]float64,
	pace *Pace,
) *CommunityResult {
	nodeMap := make(map[string]*graph.Node, len(finalComm))
	for n := range graph.NodesLightSeq(g) {
		pace.Tick()
		if _, ok := finalComm[n.ID]; ok {
			nodeMap[n.ID] = n
		}
	}

	// Bucket original nodes by their final community.
	byComm := make(map[string][]string)
	for nid, cid := range finalComm {
		byComm[cid] = append(byComm[cid], nid)
	}

	// Renumber to "community-N" deterministically.
	oldIDs := make([]string, 0, len(byComm))
	for cid := range byComm {
		if len(byComm[cid]) >= 2 {
			oldIDs = append(oldIDs, cid)
		}
	}
	sort.Strings(oldIDs)
	commRemap := make(map[string]string, len(oldIDs))
	for i, cid := range oldIDs {
		commRemap[cid] = fmt.Sprintf("community-%d", i)
	}

	result := &CommunityResult{NodeToComm: make(map[string]string, len(finalComm))}
	for nid, cid := range finalComm {
		if newID, ok := commRemap[cid]; ok {
			result.NodeToComm[nid] = newID
		}
	}

	for oldID, members := range byComm {
		newID, ok := commRemap[oldID]
		if !ok {
			continue
		}
		fileSet := make(map[string]bool)
		for _, mid := range members {
			if n, ok := nodeMap[mid]; ok {
				fileSet[n.FilePath] = true
			}
		}
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)

		c := Community{
			ID:       newID,
			Label:    inferCommunityLabel(members, nodeMap, files),
			Members:  members,
			Files:    files,
			Size:     len(members),
			Cohesion: computeCohesion(members, neighbors),
			Hub:      findHub(members, nodeMap, neighbors),
		}
		result.Communities = append(result.Communities, c)
	}

	disambiguateLabels(result.Communities)
	assignDirectoryParents(result.Communities)
	sort.Slice(result.Communities, func(i, j int) bool {
		return result.Communities[i].Size > result.Communities[j].Size
	})

	// Modularity over original graph using final partition.
	result.Modularity = computeModularity(finalComm, neighbors, degree, totalWeight)
	return result
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
