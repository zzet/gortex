package analysis

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/testpath"
)

// RiskLevel represents the severity of a change's impact.
type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

// ImpactEntry is a symbol affected at a specific depth.
type ImpactEntry struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	FilePath        string  `json:"file_path"`
	Line            int     `json:"start_line"`
	RepoPrefix      string  `json:"repo_prefix,omitempty"`
	EdgeConfidence  float64 `json:"edge_confidence,omitempty"`
	ConfidenceLabel string  `json:"confidence_label,omitempty"`
}

// ImpactResult is the output of risk-tiered impact analysis.
type ImpactResult struct {
	Risk                RiskLevel                `json:"risk"`
	Summary             string                   `json:"summary"`
	ByDepth             map[int][]ImpactEntry    `json:"by_depth"`
	AffectedProcesses   []string                 `json:"affected_processes,omitempty"`
	AffectedCommunities []string                 `json:"affected_communities,omitempty"`
	TestFiles           []string                 `json:"test_files,omitempty"`
	TotalAffected       int                      `json:"total_affected"`
	CrossRepoImpact     bool                     `json:"cross_repo_impact,omitempty"`
	ByRepo              map[string][]ImpactEntry `json:"by_repo,omitempty"`
	// LowerBound is set when the blast radius crosses a dynamic-dispatch /
	// interface site the resolver could not bind: the true affected count is
	// then a floor (">=TotalAffected, could be more"), not an exact number.
	LowerBound bool `json:"lower_bound,omitempty"`
	// Truncated means traversal or response fan-out hit a safety budget.
	// ByDepth and TotalAffected are then a lower bound, never proof that the
	// omitted portion is empty.
	Truncated bool `json:"truncated,omitempty"`
	// Boundaries names the unresolved/dispatch sites that make the count a
	// floor, so an agent can act on them (e.g. find_implementations on the
	// interface). Omitted when empty.
	Boundaries []graph.EpistemicBoundary `json:"boundaries,omitempty"`
}

// AnalyzeImpact performs depth-tiered blast radius analysis on a set of symbols.
//
// Fast path: when every seed has a complete reach index record
// (`Node.Meta["reach_d1/d2/d3"]` with matching build and completion
// markers), the
// depth-1/2/3 ByDepth tiers are constructed from those sets without
// a live BFS — turning the dominant cost from O(reach) edge walks
// into O(reach) map lookups. The representative in-edge per tier
// entry is recovered with a linear scan of the entry's incoming
// edges, matching the live walk's behavior. Fall back to live BFS
// when a seed cannot be indexed (for example, a non-symbol node) — the
// slow path is identical to the pre-index implementation so consumer
// semantics never diverge. Missing or interrupted symbol records are
// recomputed and atomically published by reach.Lookup.
func AnalyzeImpact(g graph.Reader, symbolIDs []string, communities *CommunityResult, processes *ProcessResult) *ImpactResult {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return AnalyzeImpactContext(ctx, g, symbolIDs, communities, processes)
}

// AnalyzeImpactContext is the cancellable form used by interactive callers.
// The compatibility wrapper above supplies a strict deadline so even callers
// that have not yet propagated their request context cannot hang on a hub.
func AnalyzeImpactContext(ctx context.Context, g graph.Reader, symbolIDs []string, communities *CommunityResult, processes *ProcessResult) *ImpactResult {
	if ctx == nil {
		ctx = context.Background()
	}
	result := &ImpactResult{
		ByDepth: make(map[int][]ImpactEntry),
	}
	if !fillImpactFromReach(ctx, g, result, symbolIDs) {
		fillImpactLive(ctx, g, result, symbolIDs)
	}

	// Trim noise from the transitive tiers: a resolution edge with
	// confidence == 0 AND ConfidenceLabel == "INFERRED" means the
	// resolver produced the link without type info — essentially a
	// name-text match. At d=2 and d=3 these multiply the blast radius
	// through shared upstream helpers (e.g. every analyze_* handler
	// sharing respondJSONOrTOON), turning a leaf change into hundreds
	// of "transitively affected" rows the user can't act on. d=1 is
	// preserved untouched because direct dependents are always
	// informative even at low confidence.
	for depth := 2; depth <= 3; depth++ {
		result.ByDepth[depth] = filterHeuristicEntries(result.ByDepth[depth])
	}
	// Hard fan-out cap per tier so a pathological hub doesn't blow up
	// the response. Sorted ID order is already deterministic from the
	// reach index, so the cap is stable.
	for depth := 1; depth <= 3; depth++ {
		if len(result.ByDepth[depth]) > maxImpactEntriesPerTier {
			result.Truncated = true
			result.ByDepth[depth] = result.ByDepth[depth][:maxImpactEntriesPerTier]
		}
	}

	// Deduplicate test files
	result.TestFiles = dedup(result.TestFiles)

	// Count total
	for _, entries := range result.ByDepth {
		result.TotalAffected += len(entries)
	}

	// Determine risk level
	d1 := len(result.ByDepth[1])
	d2 := len(result.ByDepth[2])
	result.Risk = assessRisk(d1, d2)

	// Find affected processes
	if processes != nil {
		procSet := make(map[string]bool)
		for _, id := range symbolIDs {
			for _, pid := range processes.NodeToProcs[id] {
				procSet[pid] = true
			}
		}
		for depth := 1; depth <= 3; depth++ {
			for _, entry := range result.ByDepth[depth] {
				for _, pid := range processes.NodeToProcs[entry.ID] {
					procSet[pid] = true
				}
			}
		}
		for pid := range procSet {
			result.AffectedProcesses = append(result.AffectedProcesses, pid)
		}
		sort.Strings(result.AffectedProcesses)
	}

	// Find affected communities
	if communities != nil {
		commSet := make(map[string]bool)
		for _, id := range symbolIDs {
			if cid, ok := communities.NodeToComm[id]; ok {
				commSet[cid] = true
			}
		}
		for depth := 1; depth <= 3; depth++ {
			for _, entry := range result.ByDepth[depth] {
				if cid, ok := communities.NodeToComm[entry.ID]; ok {
					commSet[cid] = true
				}
			}
		}
		for cid := range commSet {
			result.AffectedCommunities = append(result.AffectedCommunities, cid)
		}
		sort.Strings(result.AffectedCommunities)
	}

	seedNodes, seedNodeErr := getImpactNodesContext(ctx, g, symbolIDs)
	if seedNodeErr != nil {
		result.Truncated = true
	}

	// Epistemic lower bound: blast radius is a count of *callers*, so a seed
	// that implements/overrides an interface may be reached through dynamic
	// dispatch the resolver could not attribute — the count is then a floor.
	boundaries, boundaryTruncated := graph.CallerBoundariesContext(ctx, g, symbolIDs, 0)
	result.Boundaries = boundaries
	result.Truncated = result.Truncated || boundaryTruncated
	result.LowerBound = result.Truncated || graph.LowerBoundCaveat(result.Boundaries)
	// A partial traversal is never a LOW-risk verdict. MEDIUM is the minimum
	// conservative posture; observed direct/transitive fan-out can still raise
	// it to HIGH or CRITICAL through assessRisk above.
	if result.LowerBound && result.Risk == RiskLow {
		result.Risk = RiskMedium
	}

	// Summary
	result.Summary = fmt.Sprintf(
		"%d direct dependents, %d transitively affected, %d test files, risk: %s",
		d1, result.TotalAffected, len(result.TestFiles), result.Risk,
	)
	if result.Truncated {
		result.Summary += " — lower bound: reach traversal or output budget was reached; more callers may exist"
	}
	if graph.LowerBoundCaveat(result.Boundaries) {
		result.Summary += fmt.Sprintf(
			" — lower bound: %d dispatch boundary(ies) may add more callers",
			len(result.Boundaries),
		)
	}

	// Group affected symbols by RepoPrefix and detect cross-repo impact.
	repoSet := make(map[string]bool)
	byRepo := make(map[string][]ImpactEntry)
	for _, id := range symbolIDs {
		if n := seedNodes[id]; n != nil && n.RepoPrefix != "" {
			repoSet[n.RepoPrefix] = true
		}
	}
	for depth := 1; depth <= 3; depth++ {
		for _, entry := range result.ByDepth[depth] {
			if entry.RepoPrefix != "" {
				repoSet[entry.RepoPrefix] = true
				byRepo[entry.RepoPrefix] = append(byRepo[entry.RepoPrefix], entry)
			}
		}
	}
	if len(repoSet) > 1 {
		result.CrossRepoImpact = true
		result.ByRepo = byRepo
	}

	return result
}

// fillImpactLive is the pre-precomputed-reach implementation: a
// depth-3 BFS over incoming edges that materialises one ImpactEntry
// per discovered node, attributing the in-edge that introduced it to
// EdgeConfidence / ConfidenceLabel. Kept as the always-correct
// fallback for fillImpactFromReach.
const maxImpactEntriesPerTier = 50

type impactNodesContextGetter interface {
	GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
}

func getImpactNodesContext(ctx context.Context, g graph.Reader, ids []string) (map[string]*graph.Node, error) {
	if getter, ok := g.(impactNodesContextGetter); ok {
		return getter.GetNodesByIDsContext(ctx, ids)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return g.GetNodesByIDs(ids), nil
}

func fillImpactLive(ctx context.Context, g graph.Reader, result *ImpactResult, symbolIDs []string) {
	const maxLiveImpactEdges = maxImpactEntriesPerTier + 1

	type boundedIncomingEdgeReader interface {
		GetInEdgesByNodeIDsContext(context.Context, []string, int) (map[string][]*graph.Edge, bool, error)
	}
	readIncoming := func(ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
		if reader, ok := g.(boundedIncomingEdgeReader); ok {
			return reader.GetInEdgesByNodeIDsContext(ctx, ids, limit)
		}
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		all := g.GetInEdgesByNodeIDs(ids)
		out := make(map[string][]*graph.Edge, len(all))
		count := 0
		for _, id := range ids {
			for _, edge := range all[id] {
				if count >= limit {
					return out, true, nil
				}
				out[id] = append(out[id], edge)
				count++
			}
		}
		return out, false, nil
	}

	visited := make(map[string]bool)
	for _, id := range symbolIDs {
		visited[id] = true
	}
	current := append([]string(nil), symbolIDs...)
	edgesRemaining := maxLiveImpactEdges
	for depth := 1; depth <= 3 && len(current) > 0; depth++ {
		if ctx.Err() != nil || edgesRemaining <= 0 {
			result.Truncated = true
			break
		}
		inEdges, limited, err := readIncoming(current, edgesRemaining)
		if err != nil {
			result.Truncated = true
			break
		}

		type candidate struct {
			id   string
			edge *graph.Edge
		}
		next := make([]string, 0)
		candidates := make([]candidate, 0)
		for _, id := range current {
			for _, edge := range inEdges[id] {
				edgesRemaining--
				if edge == nil || visited[edge.From] {
					continue
				}
				if edge.Kind == graph.EdgeDefines || edge.Kind == graph.EdgeMemberOf {
					continue
				}
				visited[edge.From] = true
				next = append(next, edge.From)
				candidates = append(candidates, candidate{id: edge.From, edge: edge})
			}
		}

		nodes, err := getImpactNodesContext(ctx, g, next)
		if err != nil {
			result.Truncated = true
			return
		}
		emitted := 0
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				result.Truncated = true
				break
			}
			n := nodes[candidate.id]
			if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			if emitted >= maxImpactEntriesPerTier {
				result.Truncated = true
				continue
			}
			result.ByDepth[depth] = append(result.ByDepth[depth], ImpactEntry{
				ID:              n.ID,
				Name:            n.Name,
				Kind:            string(n.Kind),
				FilePath:        n.FilePath,
				Line:            n.StartLine,
				RepoPrefix:      n.RepoPrefix,
				EdgeConfidence:  candidate.edge.Confidence,
				ConfidenceLabel: graph.ConfidenceLabelFor(candidate.edge.Kind, candidate.edge.Confidence),
			})
			emitted++
			if isTestFile(n.FilePath) {
				result.TestFiles = append(result.TestFiles, n.FilePath)
			}
		}
		current = next
		if limited {
			result.Truncated = true
			break
		}
	}
}

// fillImpactFromReach is the indexed fast path. Returns false if any seed
// cannot produce a complete reach record — the caller must then run
// fillImpactLive. A merely stamped but incomplete record is never a hit.
// The union of per-seed reach_d1 sets becomes the
// depth-1 tier; depth-2 is the union of per-seed reach_d2 minus
// seeds and minus the depth-1 set; depth-3 is built the same way
// against (seeds ∪ d1 ∪ d2). For each tier-N entry we look up the
// representative in-edge with a linear scan of the node's incoming
// edges, picking the first one whose source is in the seeds (N=1) or
// in the prior tier's accumulated set (N≥2) — matching the live walk's
// deterministic-by-shard-iteration choice closely enough for tests
// that compare ByDepth ID sets, which is the contract consumers rely
// on. EdgeConfidence is set from that representative edge.
//
// The reach index is built over the base corpus and is keyed off node Meta
// plus the store's resolve mutex, so it is only consulted when the reader is
// the base store itself. A reader that layers unsaved edits over the base
// (a request overlay view) is not a store, and the miss sends the caller to
// fillImpactLive, which walks the reader's own edges and so reflects the
// layered state.
func fillImpactFromReach(ctx context.Context, r graph.Reader, result *ImpactResult, symbolIDs []string) bool {
	if len(symbolIDs) == 0 {
		return true
	}
	g, ok := r.(graph.Store)
	if !ok {
		return false
	}
	// Single-seed shortcut. The precomputed tier slices are already
	// unique and sorted by ID (BuildIndex calls sortTierByID), so the
	// generic multi-seed path's per-depth merge + sort + seen-map are
	// pure overhead here. Stream directly into ByDepth with the
	// destination slice pre-sized — measurable difference on hot
	// blast-radius queries (1000-caller fan-in: ~2x faster than the
	// generic path).
	if len(symbolIDs) == 1 {
		seedID := symbolIDs[0]
		d1, d2, d3, hit, truncated := reach.LookupContext(ctx, g, seedID)
		result.Truncated = result.Truncated || truncated
		if !hit {
			if truncated {
				// Cancellation before the seed could be read is still a bounded
				// result; do not fall into the unbounded legacy walk.
				return true
			}
			return false
		}
		for depth, tier := range [3][]reach.Entry{d1, d2, d3} {
			if len(tier) == 0 {
				continue
			}
			selected := make([]reach.Entry, 0, min(len(tier), maxImpactEntriesPerTier))
			for _, entry := range tier {
				if entry.ID == seedID {
					continue
				}
				if len(selected) == maxImpactEntriesPerTier {
					result.Truncated = true
					break
				}
				selected = append(selected, entry)
			}
			ids := make([]string, len(selected))
			for i := range selected {
				ids[i] = selected[i].ID
			}
			nodes, err := getImpactNodesContext(ctx, g, ids)
			if err != nil {
				result.Truncated = true
				return true
			}
			out := make([]ImpactEntry, 0, len(selected))
			for _, entry := range selected {
				if ctx.Err() != nil {
					result.Truncated = true
					break
				}
				n := nodes[entry.ID]
				if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}
				out = append(out, ImpactEntry{
					ID:              n.ID,
					Name:            n.Name,
					Kind:            string(n.Kind),
					FilePath:        n.FilePath,
					Line:            n.StartLine,
					RepoPrefix:      n.RepoPrefix,
					EdgeConfidence:  entry.Conf,
					ConfidenceLabel: entry.Label,
				})
				if isTestFile(n.FilePath) {
					result.TestFiles = append(result.TestFiles, n.FilePath)
				}
			}
			result.ByDepth[depth+1] = out
		}
		return true
	}

	perSeed := make([][3][]reach.Entry, len(symbolIDs))
	for i, id := range symbolIDs {
		d1, d2, d3, hit, truncated := reach.LookupContext(ctx, g, id)
		result.Truncated = result.Truncated || truncated
		if !hit {
			if truncated {
				continue
			}
			return false
		}
		perSeed[i] = [3][]reach.Entry{d1, d2, d3}
	}

	// `seen` tracks every ID already emitted at a prior depth (and
	// the seed set itself) so a node appears in at most one ByDepth
	// slot — matches the BFS visited-set discipline the live walk has.
	// First per-seed appearance wins on cross-seed overlap, mirroring
	// the live walk's BFS-by-depth order.
	seen := make(map[string]struct{}, len(symbolIDs)+32)
	for _, id := range symbolIDs {
		seen[id] = struct{}{}
	}
	for depth := 1; depth <= 3; depth++ {
		var tier []reach.Entry
		for s := range perSeed {
			for _, e := range perSeed[s][depth-1] {
				if _, already := seen[e.ID]; already {
					continue
				}
				seen[e.ID] = struct{}{}
				tier = append(tier, e)
			}
		}
		// Deterministic emission — matches each per-seed slice's
		// build-time sort + makes the JSON payload diff-stable.
		sort.Slice(tier, func(i, j int) bool { return tier[i].ID < tier[j].ID })
		if len(tier) > maxImpactEntriesPerTier {
			result.Truncated = true
			tier = tier[:maxImpactEntriesPerTier]
		}
		ids := make([]string, len(tier))
		for i := range tier {
			ids[i] = tier[i].ID
		}
		nodes, err := getImpactNodesContext(ctx, g, ids)
		if err != nil {
			result.Truncated = true
			return true
		}
		for _, entry := range tier {
			if ctx.Err() != nil {
				result.Truncated = true
				break
			}
			n := nodes[entry.ID]
			if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			result.ByDepth[depth] = append(result.ByDepth[depth], ImpactEntry{
				ID:              n.ID,
				Name:            n.Name,
				Kind:            string(n.Kind),
				FilePath:        n.FilePath,
				Line:            n.StartLine,
				RepoPrefix:      n.RepoPrefix,
				EdgeConfidence:  entry.Conf,
				ConfidenceLabel: entry.Label,
			})
			if isTestFile(n.FilePath) {
				result.TestFiles = append(result.TestFiles, n.FilePath)
			}
		}
	}
	return true
}

// filterHeuristicEntries strips ImpactEntries whose representative
// edge was a heuristic / text-matched resolution (Confidence == 0 +
// label == "INFERRED"). Returns the kept prefix to avoid an extra
// allocation. The input slice is mutated.
func filterHeuristicEntries(entries []ImpactEntry) []ImpactEntry {
	kept := entries[:0]
	for _, e := range entries {
		if e.EdgeConfidence == 0 && e.ConfidenceLabel == "INFERRED" {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

func assessRisk(directDeps, transitiveDeps int) RiskLevel {
	if directDeps >= 10 || (directDeps >= 5 && transitiveDeps >= 20) {
		return RiskCritical
	}
	if directDeps >= 5 || transitiveDeps >= 10 {
		return RiskHigh
	}
	if directDeps >= 2 || transitiveDeps >= 5 {
		return RiskMedium
	}
	return RiskLow
}

// IsTestFile reports whether path looks like a test source file — the same
// predicate the impact traversal uses to collect covering tests, exported
// for callers that need to probe whether a graph indexes tests at all.
//
// It delegates to internal/testpath, the convention table the indexer's
// test-edge pass classifies with. Before that, this was a seven-fragment
// substring match of its own, and the two disagreed: a `_spec.rb`,
// a `FooTest.java`, or anything under a `tests/` directory got a `tests`
// edge from the indexer but was not counted as a covering test here, so the
// same change read as untested in the review's per-file rows and covered in
// the risk receipt. The old match was also too loose in the other
// direction — "test_" anywhere in the path made `src/latest_release.go` a
// test file.
func IsTestFile(path string) bool { return isTestFile(path) }

func isTestFile(path string) bool { return testpath.IsTestFile(path) }

func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
