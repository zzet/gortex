package mcp

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// closureFocusCount is how many of the nearest closure nodes are placed
// in the manifest's focus tier (full source) before the rest fall to
// the outline tier. It is deliberately small: the closure is a wide
// dependency neighbourhood, and full source for everything would blow
// any token budget. The manifest's own budget demotion is the backstop.
const closureFocusCount = 6

// registerContextClosureTool wires context_closure — a multi-seed
// dependency-closure context selector. Given a set of seed files and/or
// symbols it walks the transitive dependency closure (imports / calls /
// references / depends_on), ranks the closure by graph distance to the
// nearest seed, and packs it through the same token-budgeted focus →
// ring → outline manifest smart_context's graded fidelity uses.
func (s *Server) registerContextClosureTool() {
	edgeKindList := strings.Join(query.KnownEdgeKinds(), ", ")
	s.addTool(
		mcp.NewTool("context_closure",
			mcp.WithDescription("Assemble a context pack from the dependency closure of a SET of seeds. Give it seed files and/or symbol IDs; it walks the transitive dependency graph (imports / calls / references / depends_on by default), ranks every reached symbol by its graph distance to the nearest seed, and packs the closest symbols under a token budget (full source for the nearest, signatures for the rest). Use it when you have a cluster of starting points and want everything they transitively depend on in one call, ordered by proximity."),
			mcp.WithString("files", mcp.Description("Comma-separated seed file paths (repo-relative or absolute). Every symbol defined in each file becomes a distance-0 seed.")),
			mcp.WithString("symbols", mcp.Description("Comma-separated seed symbol node IDs (e.g. pkg/server.go::HandleRequest). Each becomes a distance-0 seed.")),
			mcp.WithString("edge_kinds", mcp.Description("Comma-separated edge kinds the closure follows. Default: the dependency allowlist (imports, calls, references, depends_on, plus infrastructure edges). Valid kinds: "+edgeKindList+".")),
			mcp.WithNumber("token_budget", mcp.Description("Token ceiling for the packed manifest (default adaptive to repo size). Closure nodes are demoted full → compressed → outline as the budget fills.")),
			mcp.WithNumber("max_depth", mcp.Description("Hard cap on closure expansion depth from the seed set (default 6).")),
			mcp.WithNumber("max_nodes", mcp.Description("Cap on the number of closure members explored, nearest-first (default 400).")),
			mcp.WithString("rank", mcp.Description("How closure nodes are ordered for the focus tier: \"distance\" (default — nearest seed first) or \"proximity\" (a seeded random-walk-with-restart score, which favours nodes that are reachable from the seeds along many short paths, not just the single shortest one).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix.")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project.")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag.")),
			mcp.WithString("workspace", mcp.Description("Workspace override. In workspace-bound sessions this must match the active workspace.")),
			mcp.WithString("scope", mcp.Description("Saved scope name. Ignored for git diff scopes; explicit repo/project/ref filters take precedence.")),
		),
		s.handleContextClosure,
	)
}

// handleContextClosure resolves the seed set, walks the dependency
// closure, ranks it, and packs the ranked closure into a context
// manifest plus a closure-summary block.
func (s *Server) handleContextClosure(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	eng := s.engineFor(ctx)
	if eng == nil {
		return mcp.NewToolResultError("graph engine unavailable"), nil
	}

	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}

	// Resolve the seed set: every symbol defined in each seed file,
	// plus every explicit seed symbol ID. Files are resolved through
	// the engine's file-symbol accessor so the seeds carry the same
	// node identity the closure walk reads.
	seedIDs := make([]string, 0)
	seedSeen := make(map[string]bool)
	addSeed := func(id string) {
		if id == "" || seedSeen[id] {
			return
		}
		seedSeen[id] = true
		seedIDs = append(seedIDs, id)
	}

	var resolvedFiles, missingFiles []string
	for _, raw := range splitCSVArg(req.GetString("files", "")) {
		fp := s.normalizeClosureFilePath(raw)
		sg := eng.GetFileSymbols(fp)
		if sg == nil || len(sg.Nodes) == 0 {
			missingFiles = append(missingFiles, raw)
			continue
		}
		resolvedFiles = append(resolvedFiles, fp)
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			addSeed(n.ID)
		}
	}

	var missingSymbols []string
	for _, id := range splitCSVArg(req.GetString("symbols", "")) {
		if eng.GetSymbol(id) == nil {
			missingSymbols = append(missingSymbols, id)
			continue
		}
		addSeed(id)
	}

	if len(seedIDs) == 0 {
		return mcp.NewToolResultError("no seeds resolved: pass files= and/or symbols= that exist in the graph"), nil
	}

	edgeKinds, kindErr := query.ParseEdgeKindsCSV(req.GetString("edge_kinds", ""))
	if kindErr != nil {
		return mcp.NewToolResultError(kindErr.Error()), nil
	}

	closure := eng.ImportClosure(seedIDs, query.ClosureOptions{
		EdgeKinds:   edgeKinds,
		MaxDepth:    req.GetInt("max_depth", 0),
		MaxNodes:    req.GetInt("max_nodes", 0),
		WorkspaceID: resolved.WorkspaceID,
		ProjectID:   resolved.ProjectID,
		RepoAllow:   resolved.RepoAllow,
	})

	// Apply the repo filter (defence in depth alongside the scope the
	// closure already enforced) before ranking, so the manifest and the
	// summary agree on the member set.
	members := filterClosureNodes(closure.Nodes, resolved.RepoAllow)

	rankMode := strings.ToLower(strings.TrimSpace(req.GetString("rank", "distance")))
	if rankMode != "proximity" {
		rankMode = "distance"
	}

	// proximity carries a seeded random-walk-with-restart score per
	// member; nil under the default distance ranking. Feature wired in
	// the seeded-random-walk change. The member set goes in because it is
	// the set the walk must be able to score — a snapshot that does not
	// contain a member scores it zero, which is indistinguishable from
	// "unreachable" in the emitted row below.
	proximity := s.closureProximity(ctx, rankMode, closure.SeedIDs, members)

	ordered := orderClosureMembers(members, proximity, rankMode)

	// Split the ranked closure into the manifest's focus tier (nearest
	// / highest-proximity, full source) and the outline remainder.
	focus := make([]*graph.Node, 0, closureFocusCount)
	outline := make([]*graph.Node, 0, len(ordered))
	for i, m := range ordered {
		if i < closureFocusCount {
			focus = append(focus, m.Node)
		} else {
			outline = append(outline, m.Node)
		}
	}

	tokenBudget := req.GetInt("token_budget", 0)
	manifest := s.buildContextManifest(ctx, focus, outline, tokenBudget)

	// closure-summary block: every member with its distance to the
	// nearest seed and (when ranked by proximity) its restart score.
	memberRows := make([]map[string]any, 0, len(ordered))
	for _, m := range ordered {
		row := map[string]any{
			"id":        m.Node.ID,
			"name":      m.Node.Name,
			"kind":      string(m.Node.Kind),
			"file_path": m.Node.FilePath,
			"distance":  m.Distance,
		}
		if rankMode == "proximity" {
			row["proximity"] = roundClosureScore(proximity[m.Node.ID])
		}
		memberRows = append(memberRows, row)
	}

	result := map[string]any{
		"seeds":            closure.SeedIDs,
		"resolved_files":   resolvedFiles,
		"rank":             rankMode,
		"member_count":     len(ordered),
		"truncated":        closure.Truncated,
		"stopped_at_depth": closure.StoppedAtDepth,
		"members":          memberRows,
		"context_manifest": manifest,
	}
	if len(missingFiles) > 0 {
		result["missing_files"] = missingFiles
	}
	if len(missingSymbols) > 0 {
		result["missing_symbols"] = missingSymbols
	}

	return s.respondScopedJSONOrTOON(ctx, req, result, resolved)
}

// closureProximity returns a per-node seeded random-walk-with-restart
// score for ranking closure members by their proximity to the seed
// set. A nil result means "no proximity signal available" — the caller
// then orders purely by graph distance.
//
// The walk runs over the snapshot that describes the graph THIS request
// read: the shared analysis pass's CSR for a request the indexed corpus
// answers, and a bounded CSR built from the request's own reader for a
// request that selected a checkout of its own or composed editor buffers.
// Ranking a selected view with the shared pass's scores would score a
// correct graph from a snapshot it never contained. Until a snapshot exists
// at all (analysis has not run, or no seed resolves in the selected view)
// this returns nil and "proximity" ranking degrades to distance ranking.
//
// members is the set the caller emits a score for, and is what the bounded
// snapshot is rooted on. A CSR rooted on the seeds alone reaches only
// `depth` hops, so every member past that horizon would come back with no
// score — emitted as proximity 0, a number the caller cannot tell from a
// genuinely unreachable member, and one that sinks every truncated member to
// the bottom of the ranking in a tie.
func (s *Server) closureProximity(ctx context.Context, rankMode string, seeds []string, members []query.ClosureNode) map[string]float64 {
	if rankMode != "proximity" {
		return nil
	}
	memberIDs := make([]string, 0, len(members))
	for _, m := range members {
		if m.Node != nil {
			memberIDs = append(memberIDs, m.Node.ID)
		}
	}
	snap, scope := s.requestProximityAdjacency(ctx, seeds, memberIDs)
	if snap == nil {
		return nil
	}
	// Route through the Merkle-keyed walk cache (restart 0 -> the
	// snapshot's default restart probability), namespaced by the snapshot
	// that produced the walk so no two views share an entry.
	return s.personalizedPageRankScoped(scope, snap, seeds)
}

// normalizeClosureFilePath converts a seed file argument into the
// repo-prefixed key the graph stores its file_path under. An absolute
// path under a tracked repo is mapped to that key via repoRelative;
// everything else passes through unchanged (already a repo-prefixed or
// repo-relative path the agent supplied — the same contract
// get_file_summary honours).
func (s *Server) normalizeClosureFilePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if filepath.IsAbs(raw) {
		return s.repoRelative(raw)
	}
	return raw
}

// rankedClosureMember pairs a closure node with the score under the
// active ranking so ordering is computed once.
type rankedClosureMember struct {
	Node     *graph.Node
	Distance int
}

// orderClosureMembers sorts the closure members for the focus split.
// Under "distance" the order is (distance asc, ID asc) — already the
// order ImportClosure returns, but re-applied after the repo filter so
// it stays stable. Under "proximity" the order is (proximity desc,
// distance asc, ID asc): a high restart score wins, ties break toward
// the nearer node so the focus tier still favours short paths.
func orderClosureMembers(members []query.ClosureNode, proximity map[string]float64, rankMode string) []rankedClosureMember {
	out := make([]rankedClosureMember, 0, len(members))
	for _, m := range members {
		out = append(out, rankedClosureMember{Node: m.Node, Distance: m.Distance})
	}
	if rankMode == "proximity" {
		sort.SliceStable(out, func(i, j int) bool {
			pi, pj := proximity[out[i].Node.ID], proximity[out[j].Node.ID]
			if pi != pj {
				return pi > pj
			}
			if out[i].Distance != out[j].Distance {
				return out[i].Distance < out[j].Distance
			}
			return out[i].Node.ID < out[j].Node.ID
		})
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Distance != out[j].Distance {
			return out[i].Distance < out[j].Distance
		}
		return out[i].Node.ID < out[j].Node.ID
	})
	return out
}

// filterClosureNodes drops closure members outside the resolved repo
// filter. A nil/empty allow-set passes everything (no filter active).
//
// Admission routes through repoNarrowAdmits — the single definition of
// what a repo narrow does to a node carrying no prefix — rather than
// indexing the allow-set directly. A direct index rejects the
// unattributed nodes every other repo-scoping predicate admits, which
// turns a narrow into an emptying wherever prefixes are absent.
func filterClosureNodes(nodes []query.ClosureNode, allowed map[string]bool) []query.ClosureNode {
	if len(allowed) == 0 {
		return nodes
	}
	kept := make([]query.ClosureNode, 0, len(nodes))
	for _, m := range nodes {
		if m.Node == nil {
			continue
		}
		if repoNarrowAdmits(allowed, m.Node.RepoPrefix) {
			kept = append(kept, m)
		}
	}
	return kept
}

// splitCSVArg splits a comma-separated argument into trimmed, non-empty
// fields.
func splitCSVArg(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// roundClosureScore rounds a proximity score to a stable precision so
// the wire output is deterministic.
func roundClosureScore(v float64) float64 {
	return float64(int64(v*1e6+0.5)) / 1e6
}
