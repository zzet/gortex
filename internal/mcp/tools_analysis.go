package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

func (s *Server) registerAnalysisTools() {
	s.addTool(
		mcp.NewTool("get_communities",
			mcp.WithDescription("Returns functional clusters discovered by community detection. Without id: list all communities with summaries. With id: full details of a specific community (members, files, cohesion). Members and files are clamped to the session workspace."),
			mcp.WithString("id", mcp.Description("Optional community ID (e.g. community-0). When set, returns full details of that community instead of the list.")),
			mcp.WithString("repo", mcp.Description("Narrow to a single repository prefix (tracked name or path). The partition is computed over the whole index, so this is clamped and widened to the session workspace; the response discloses it.")),
			mcp.WithString("project", mcp.Description("Narrow to the repositories in a project. Clamped and widened to the session workspace like repo.")),
			mcp.WithString("workspace", mcp.Description("Restrict to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope). Clamped and widened to the session workspace like repo.")),
		),
		s.handleGetCommunities,
	)

	s.addTool(
		mcp.NewTool("get_processes",
			mcp.WithDescription("Returns discovered execution flows — named chains of function calls starting from entry points. Without id: list all processes. With id: full step-by-step call chain for that process."),
			mcp.WithString("id", mcp.Description("Optional process ID (e.g. process-0). When set, returns the full step-by-step call chain for that process instead of the list.")),
			mcp.WithString("repo", mcp.Description("Narrow to a single repository prefix (tracked name or path), clamped to the session workspace. Steps outside it are excised from the chains.")),
			mcp.WithString("project", mcp.Description("Narrow to the repositories in a project, clamped to the session workspace.")),
			mcp.WithString("workspace", mcp.Description("Restrict to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — its repositories narrow the flows, clamped to the session workspace.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetProcesses,
	)

	s.addTool(
		mcp.NewTool("detect_changes",
			mcp.WithDescription("Maps uncommitted git changes to symbols in the graph and runs blast radius analysis. The key pre-commit review tool. changed_files and file_changes are the file-granular view (added/modified/deleted/renamed) and stay populated for changes that carry no indexed symbol, so an empty changed_symbols never means 'nothing changed'. Untracked and ignored files are not observed — every scope reads `git diff`."),
			mcp.WithString("scope", mcp.Description("unstaged (default), staged, all, or compare")),
			mcp.WithString("base_ref", mcp.Description("Branch/commit for compare scope (default: main)")),
			mcp.WithString("repo", mcp.Description("Repository prefix or path (multi-repo mode); defaults to the lone tracked repo or the session's cwd-bound repo")),
			mcp.WithBoolean("summary_only", mcp.Description("Return only by_depth_counts and drop the per-depth row lists — the cheapest blast-radius shape.")),
			mcp.WithNumber("offset", mcp.Description("Skip this many affected rows (depth order) before returning by_depth — pairs with limit to page a large blast radius.")),
			mcp.WithNumber("limit", mcp.Description("Max affected rows to return in by_depth (default 100). by_depth_counts always reports the full per-depth totals.")),
		),
		s.handleDetectChanges,
	)

	s.addTool(
		mcp.NewTool("suggest_queries",
			mcp.WithDescription("Cold-start helper: returns 5-10 starter exploration queries for an unfamiliar repository, derived from its entry points, load-bearing hubs, community bridges, and largest subsystems. Run at session start to orient before reaching for search_symbols / smart_context."),
			mcp.WithNumber("limit", mcp.Description("Max suggestions to return (default 8, capped at 20).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleSuggestQueries,
	)

	s.addTool(
		mcp.NewTool("search_text",
			mcp.WithDescription("Trigram-accelerated literal (or regexp) code search across the indexed repository — the alt grep backbone. Each hit carries the enclosing graph symbol (symbol_id / symbol_name) so you see which function or method a match landed in without a follow-up call. A trigram index narrows the candidate files, so a repo-wide search costs roughly the size of the matching files, not the whole tree. Use for literal-string / regexp lookups; use search_symbols for symbol-name / concept queries."),
			mcp.WithString("query", mcp.Description("Literal substring (case-sensitive) to search for — or a regular expression when regexp=true.")),
			mcp.WithBoolean("regexp", mcp.Description("Treat query as a regular expression instead of a literal substring. An invalid pattern is returned as a tool error. Default false.")),
			mcp.WithNumber("limit", mcp.Description("Max matching lines to return (default 100, capped at 1000).")),
			mcp.WithString("path", mcp.Description("Restrict matches to one or more sub-paths (comma-separated) -- a monorepo-service slice. Anchored, slash-segment-boundary prefixes relative to the repo root.")),
			mcp.WithString("repo", mcp.Description("Restrict matches to a single repository prefix.")),
			mcp.WithString("project", mcp.Description("Restrict matches to repositories in a specific project.")),
			mcp.WithString("workspace", mcp.Description("Restrict matches to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) -- its repositories and paths narrow the matches. Ignored for repositories when an explicit repo / project / ref is also given.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleSearchText,
	)
}

// handleGetCommunities lists (or details) the cached community partition,
// clamped to the caller's workspace.
//
// `analyze kind=communities` is a facade alias that forwards straight to
// this tool, so the analyze dispatcher's resolveScope never runs here — the
// clamp has to be applied by the handler, exactly as PR #395 did for
// audit_health and find_clones. Community detection runs one partition over
// the whole index, so the clamp is the same workspace-only shape the sibling
// kinds use (analyze clusters / concepts / suggest_boundaries): a repo /
// project / scope narrowing is resolved for the response's scope_applied and
// then widened to the workspace, which workspaceScopeBlock discloses in the
// body. Repo-narrowing the members of a partition cell would return a
// "community" of no graph the caller can name, and would make this kind
// disagree with `analyze kind=clusters` over the very same cached partition.
func (s *Server) handleGetCommunities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resolved, errResult := s.resolveScope(ctx, req, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	if pending := s.ensureAnalysis(); !pending.Ready {
		return s.respondScopedJSONOrTOON(ctx, req, analysisPendingPayload(pending, "communities"), resolved)
	}
	// Clamp before the id branch: an out-of-workspace id then reports the
	// same "not found" as a fabricated one, so the boundary is not
	// probeable, and a community that straddles the boundary still has its
	// foreign members dropped.
	comms := s.communitiesInSessionScope(ctx, s.getCommunities())
	// The partition is the server-wide one, computed over the base corpus
	// rather than through this request's reader, so under a view every answer
	// built from it describes the base.
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph)

	// If id is provided, return the single community in detail.
	if id := req.GetString("id", ""); id != "" {
		if comms == nil {
			return mcp.NewToolResultError("no communities detected yet"), nil
		}
		for _, c := range comms.Communities {
			if c.ID == id {
				return s.respondScopedJSONOrTOON(ctx, req, c, resolved)
			}
		}
		return mcp.NewToolResultError("community not found: " + id), nil
	}

	// Otherwise return the list of summaries.
	if comms == nil || len(comms.Communities) == 0 {
		empty := map[string]any{
			"communities": []any{},
			"message":     "no communities detected yet — run index_repository first",
		}
		if blk := s.workspaceScopeBlock(ctx, req, "communities"); blk != nil {
			empty["scope"] = blk
		}
		return s.respondScopedJSONOrTOON(ctx, req, empty, resolved)
	}

	// List mode deliberately omits per-community `files` (can be hundreds
	// of paths each). Callers who want that drill into a specific
	// community via `id`; the detail response includes the full member
	// set. `file_count` preserves size signal without the string array.
	// `repo_prefix` is the majority repo of the community's members so
	// UIs can render a badge without paging through every member id.
	type summary struct {
		ID         string  `json:"id"`
		Label      string  `json:"label"`
		Size       int     `json:"size"`
		FileCount  int     `json:"file_count"`
		Cohesion   float64 `json:"cohesion"`
		RepoPrefix string  `json:"repo_prefix"`
		ParentID   string  `json:"parent_id,omitempty"`
	}
	var summaries []summary
	for _, c := range comms.Communities {
		summaries = append(summaries, summary{
			ID:         c.ID,
			Label:      c.Label,
			Size:       c.Size,
			FileCount:  len(c.Files),
			Cohesion:   c.Cohesion,
			RepoPrefix: majorityRepoPrefix(c.Members),
			ParentID:   c.ParentID,
		})
	}
	// `modularity` and the per-row `cohesion` are the global partition's
	// scores — the clamp drops members, it does not re-run detection — so
	// they are labelled rather than silently re-reported as if they had
	// been recomputed for the narrowed set.
	payload := map[string]any{
		"communities":      summaries,
		"total":            len(summaries),
		"modularity":       comms.Modularity,
		"modularity_scope": "global-partition",
	}
	if blk := s.workspaceScopeBlock(ctx, req, "communities"); blk != nil {
		payload["scope"] = blk
	}
	return s.respondScopedJSONOrTOON(ctx, req, payload, resolved)
}

// handleGetProcesses lists (or details) the discovered execution flows,
// clamped to the caller's scope.
//
// `analyze kind=processes` is a facade alias that forwards straight to this
// tool, so the analyze dispatcher's resolveScope never runs here. Unlike the
// community partition, a flow's rows are per-step and every step is
// attributable to exactly one repo, so this kind honours the full
// repo/project/scope narrowing on top of the workspace ceiling — see
// processesInSessionScope for the subtree-excision rule that keeps a
// filtered chain honest.
func (s *Server) handleGetProcesses(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resolved, errResult := s.resolveScope(ctx, req, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	ctx = withRepoAllow(ctx, resolved.RepoAllow)
	if pending := s.ensureAnalysis(); !pending.Ready {
		return s.respondScopedJSONOrTOON(ctx, req, analysisPendingPayload(pending, "processes"), resolved)
	}
	// Clamp before the id branch so an out-of-scope process id reports the
	// same "not found" as a fabricated one.
	procs := s.processesInSessionScope(ctx, s.getProcesses())
	// Process discovery is the server-wide pass over the base corpus, not a
	// walk of this request's reader, so under a view every answer built from
	// it describes the base.
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph)

	// If id is provided, return the single process in detail.
	if id := req.GetString("id", ""); id != "" {
		if procs == nil {
			return mcp.NewToolResultError("no processes discovered yet"), nil
		}
		for _, p := range procs.Processes {
			if p.ID == id {
				return s.respondScopedJSONOrTOON(ctx, req, p, resolved)
			}
		}
		return mcp.NewToolResultError("process not found: " + id), nil
	}

	// Otherwise return the list of summaries.
	if procs == nil || len(procs.Processes) == 0 {
		return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
			"processes": []any{},
			"message":   "no processes discovered yet — run index_repository first",
		}, resolved)
	}

	// `repo_prefixes` is the ordered set of distinct "owner/repo" prefixes
	// the flow's steps cross — the UI renders these as trail badges
	// without needing the full step id list.
	type summary struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		EntryPoint   string   `json:"entry_point"`
		StepCount    int      `json:"step_count"`
		FileCount    int      `json:"file_count"`
		Score        float64  `json:"score"`
		RepoPrefixes []string `json:"repo_prefixes"`
	}
	var summaries []summary
	for _, p := range procs.Processes {
		summaries = append(summaries, summary{
			ID:           p.ID,
			Name:         p.Name,
			EntryPoint:   p.EntryPoint,
			StepCount:    p.StepCount,
			FileCount:    len(p.Files),
			Score:        p.Score,
			RepoPrefixes: uniqueRepoPrefixesFromSteps(p.Steps),
		})
	}
	return s.respondScopedJSONOrTOON(ctx, req, map[string]any{
		"processes": summaries,
		"total":     len(summaries),
	}, resolved)
}

// repoPrefixOf extracts the repo prefix from a node ID of the form
// "<repoPrefix>/<file-path>::<symbol>". The first `/` separates the
// repo name from the file path, and `::` separates the file from the
// symbol. IDs that don't contain `/` before the `::` (e.g.
// "unresolved::OSTRACE") have no repo prefix and return empty.
func repoPrefixOf(id string) string {
	pathPart := id
	if i := strings.Index(id, "::"); i >= 0 {
		pathPart = id[:i]
	}
	if j := strings.Index(pathPart, "/"); j >= 0 {
		return pathPart[:j]
	}
	return ""
}

// majorityRepoPrefix returns the most common repo prefix from a list of
// node IDs. Empty when no ID carries a prefix.
func majorityRepoPrefix(ids []string) string {
	counts := make(map[string]int, 4)
	for _, id := range ids {
		if p := repoPrefixOf(id); p != "" {
			counts[p]++
		}
	}
	best := ""
	bestN := 0
	for k, n := range counts {
		if n > bestN {
			best = k
			bestN = n
		}
	}
	return best
}

// uniqueRepoPrefixesFromSteps returns the ordered set of distinct repo
// prefixes touched by a process flow, preserving DFS order so the UI
// renders "crosses" badges in call sequence rather than alphabetical.
func uniqueRepoPrefixesFromSteps(steps []analysis.Step) []string {
	seen := make(map[string]struct{}, 4)
	out := make([]string, 0, 4)
	for _, s := range steps {
		p := repoPrefixOf(s.ID)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// detectUnobservedNote names what `git diff` cannot see, so a caller reading an
// empty or short result knows which changes this tool never had a chance to
// report rather than concluding the working tree is clean.
const detectUnobservedNote = "untracked and ignored files are not observed: every scope reads `git diff`, which lists tracked files only"

// detectEmptySymbolSummary distinguishes the two causes of an empty symbol set.
// Files that carry no indexed symbol — docs, manifests, fixtures — and files
// deleted or moved wholesale are real changes with a real blast radius, and
// reporting them with the same sentence as a clean tree hides them.
func detectEmptySymbolSummary(changedFiles int) string {
	if changedFiles == 0 {
		return "no changes detected in tracked files"
	}
	return fmt.Sprintf("%d changed file(s), none mapping to indexed symbols — see changed_files and file_changes", changedFiles)
}

func (s *Server) handleDetectChanges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "unstaged")
	baseRef := req.GetString("base_ref", "main")

	// Resolve the working tree: explicit repo selector, lone tracked repo,
	// or the session's cwd-bound repo. The "." fallback keeps the standalone
	// (indexer-less) server working from its own cwd.
	repoSelector := strings.TrimSpace(req.GetString("repo", ""))
	control := checkoutControlFromContext(ctx)
	var repoRoot, repoPrefix string
	var rootErr error
	if control != nil && control.CheckoutScoped {
		rootErr = control.validateRepoSelector(repoSelector)
		repoRoot, repoPrefix = control.Checkout.RootPath, control.RepoPrefix
	} else {
		repoRoot, repoPrefix, rootErr = s.resolveDiffRoot(ctx, repoSelector)
	}
	if rootErr != nil {
		return mcp.NewToolResultError(rootErr.Error()), nil
	}
	if scopeErr := s.repoPrefixInSessionScope(ctx, repoPrefix, repoPrefix); scopeErr != nil {
		return mcp.NewToolResultError(scopeErr.Error()), nil
	}
	var pending bool
	var freshnessErr error
	if control != nil && control.CheckoutScoped {
		pending, freshnessErr = s.mutationFreshnessSummaryForRepos(ctx, repoPrefix)
	} else if err := s.awaitMutationFreshnessForRepos(ctx, repoPrefix); err != nil {
		return mcp.NewToolResultError("change detection refused a stale graph: " + err.Error()), nil
	}
	reader := s.readerFor(ctx)
	graphStatus, graphDetail := "ready", ""
	if pending {
		graphStatus, graphDetail = "pending", "checkout changes are committed but graph publication is pending"
	}
	if freshnessErr != nil {
		graphStatus, graphDetail = "failed", freshnessErr.Error()
	}
	if control != nil && control.CheckoutScoped {
		view := requestViewFromContext(ctx)
		if view == nil || view.rider == nil || !view.rider.Exact || !view.routed() {
			if graphStatus == "ready" {
				graphStatus, graphDetail = "pending", "the selected checkout has no exact published graph view"
			}
		}
	}
	if graphStatus != "ready" {
		// Git can still report file changes, but stale/base symbols must not
		// impersonate the selected checkout's changed symbols or impact.
		reader = nil
	}

	diff, err := analysis.MapGitDiff(reader, repoRoot, repoPrefix, scope, baseRef)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	changedFiles := diff.ChangedFiles
	if changedFiles == nil {
		// Never answer with a null file list: a caller cannot tell a null
		// apart from "the field was omitted", and this branch is exactly
		// where that reads as "nothing changed".
		changedFiles = []string{}
	}
	fileChanges := diff.FileChanges
	if fileChanges == nil {
		fileChanges = []analysis.FileChange{}
	}
	if graphStatus != "ready" {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"changed_symbols": []any{}, "changed_files": changedFiles, "file_changes": fileChanges,
			"risk": "UNKNOWN", "complete": false, "graph_status": graphStatus,
			"summary": "Git file changes are available; symbol mapping and impact await an exact published graph",
			"detail":  graphDetail, "scope": scope, "repo": repoPrefix, "repo_root": repoRoot,
		})
	}

	if len(diff.ChangedSymbols) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"changed_symbols": []any{},
			"changed_files":   changedFiles,
			"file_changes":    fileChanges,
			"risk":            "NONE",
			// An empty symbol set has two very different causes, and calling
			// both of them "no changes" is what makes a delete-only or
			// docs-only change look like a clean tree.
			"summary": detectEmptySymbolSummary(len(changedFiles)),
			"note":    detectUnobservedNote,
			// Echo the resolved scope so a caller can name the tree it got
			// answered about. diffRepoScope prefers an explicit selector, then
			// the lone tracked repo, then the session cwd — so the caller
			// cannot derive this from its own cwd without risking a wrong name.
			"scope":     scope,
			"repo":      repoPrefix,
			"repo_root": repoRoot,
		})
	}

	// Run impact analysis on the changed symbols
	symbolIDs := make([]string, len(diff.ChangedSymbols))
	for i, cs := range diff.ChangedSymbols {
		symbolIDs[i] = cs.ID
	}

	impact := analysis.AnalyzeImpact(s.readerFor(ctx), symbolIDs, s.getCommunities(), s.getProcesses())

	detectResult := map[string]any{
		"changed_symbols":      diff.ChangedSymbols,
		"changed_files":        changedFiles,
		"file_changes":         fileChanges,
		"risk":                 impact.Risk,
		"summary":              impact.Summary,
		"by_depth":             impact.ByDepth,
		"affected_processes":   impact.AffectedProcesses,
		"affected_communities": impact.AffectedCommunities,
		"test_files":           impact.TestFiles,
		"total_affected":       impact.TotalAffected,
		// See the zero-symbol branch above: the resolved scope rides along so
		// callers can report which working tree these changes came from.
		"scope":     scope,
		"repo":      repoPrefix,
		"repo_root": repoRoot,
	}
	applyImpactDepthPaging(detectResult, impact.ByDepth,
		req.GetBool("summary_only", false),
		req.GetInt("offset", 0),
		req.GetInt("limit", 100))
	return s.respondJSONOrTOON(ctx, req, detectResult)
}

// tryImpactAnalysisSnapshots reads optional cached enrichments without waiting
// behind a background community/process rebuild. Impact is a mandatory safety
// gate; cached labels must never determine whether the core blast radius can
// return within its deadline.
func (s *Server) tryImpactAnalysisSnapshots() (*analysis.CommunityResult, *analysis.ProcessResult) {
	if !s.analysisMu.TryRLock() {
		return nil, nil
	}
	defer s.analysisMu.RUnlock()
	return s.communities, s.processes
}

// handleEnhancedChangeImpact replaces the original explain_change_impact with risk tiering
// and cross-community warnings.
func (s *Server) handleEnhancedChangeImpact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	if freshnessErr := s.awaitMutationFreshnessForRepos(ctx, s.mutationReposForSymbolIDs(ctx, ids)...); freshnessErr != nil {
		return mcp.NewToolResultError("change impact refused a stale graph: " + freshnessErr.Error()), nil
	}

	// Keep the mandatory pre-edit safety gate well below host transport
	// timeouts. Every lower layer receives this deadline and must return a
	// conservative truncated result rather than leaving the daemon busy after
	// the client has already abandoned the call.
	impactCtx, cancelImpact := context.WithTimeout(ctx, 3*time.Second)
	defer cancelImpact()
	communities, processes := s.tryImpactAnalysisSnapshots()
	impact := analysis.AnalyzeImpactContext(impactCtx, s.readerFor(ctx), ids, communities, processes)

	result := map[string]any{
		"risk":                 impact.Risk,
		"summary":              impact.Summary,
		"complete":             impactComplete(impact),
		"truncated":            impact.Truncated,
		"by_depth":             impact.ByDepth,
		"affected_processes":   impact.AffectedProcesses,
		"affected_communities": impact.AffectedCommunities,
		"test_files":           impact.TestFiles,
		"total_affected":       impact.TotalAffected,
		"cross_repo_impact":    impact.CrossRepoImpact,
	}

	// GNX-3: by_depth_counts is the headline; the heavy by_depth rows are
	// paged (offset / limit) or dropped (summary_only) so the agent gets the
	// "47 affected, 3 at depth-1" summary by default and the rows on demand.
	applyImpactDepthPaging(result, impact.ByDepth,
		req.GetBool("summary_only", false),
		req.GetInt("offset", 0),
		req.GetInt("limit", 100))

	// Include per-repo grouping when cross-repo impact is detected.
	if impact.CrossRepoImpact {
		result["by_repo"] = impact.ByRepo
	}

	// Epistemic lower bound: the affected count is a floor when the blast
	// radius crosses a dynamic-dispatch / interface site the resolver could
	// not bind. Surface the flag + the boundary list so an agent knows
	// ">=N, could be more" and can act on each named site.
	if impact.LowerBound {
		result["lower_bound"] = true
	}
	if len(impact.Boundaries) > 0 {
		result["boundaries"] = impact.Boundaries
	}

	// When the blast radius is empty, an agent cannot tell genuinely
	// safe-to-change symbols apart from symbols the extractor never
	// wired up. Classify each input so a safety gate is not disarmed
	// by a false "0 affected".
	if impact.TotalAffected == 0 && !impact.Truncated {
		// The classifier costs a handful of indexed point lookups per input
		// symbol (node fetch plus its in/out edges), which every backend serves
		// cheaply — so the safety gate is armed everywhere, not only on small
		// embedded graphs.
		var caveats []graph.ZeroImpactCaveat
		reader := s.readerFor(ctx)
		for _, id := range ids {
			if id == "" {
				continue
			}
			if c := graph.CaveatForZeroEdge(reader, id); c != nil {
				caveats = append(caveats, graph.ZeroImpactCaveat{
					ID:      id,
					Class:   c.Class,
					Message: c.Message,
				})
			}
		}
		if len(caveats) > 0 {
			result["zero_impact_caveat"] = caveats
		} else {
			// Every input classified as "has real incoming usage edges", so no
			// per-symbol caveat fired. A zero blast radius must never reach an
			// agent unannotated — the classifier reasons about the edges the
			// graph holds, not about the ones extraction or resolution missed —
			// so state the residual uncertainty plainly.
			result["zero_impact_warning"] = "zero observed dependents is not proof of zero impact; extraction or resolution gaps may exist"
		}
	}

	// Cross-community warning
	if len(impact.AffectedCommunities) >= 2 {
		warning := s.computeCrossCommunityWarning(impact.AffectedCommunities, communities)
		result["cross_community_warning"] = warning
	} else {
		result["cross_community_warning"] = nil
		if len(impact.AffectedCommunities) == 1 && !impact.LowerBound {
			result["community_note"] = "change is community-local"
		} else if len(impact.AffectedCommunities) == 1 {
			result["community_scope"] = "incomplete — the bounded impact result cannot prove community locality"
		}
	}

	// Contract impact — if any of the changed symbols is referenced
	// as a request/response body by a declared contract, surface the
	// full list so the reviewer sees "this struct backs N routes"
	// before the edit lands. Live validate pass runs on the affected
	// contracts so existing breaking drift is reported alongside the
	// pending-change blast radius.
	if impactCtx.Err() == nil {
		if ci := s.computeContractImpactContext(impactCtx, ids); ci != nil {
			result["contract_impact"] = ci
			if impact.Risk == analysis.RiskLow && ci.Breaking > 0 {
				result["risk"] = analysis.RiskHigh
				result["contract_risk_upgrade"] = "risk raised to HIGH — type is a contract boundary with breaking drift"
			}
		}
	}

	if s.isGCX(ctx, req) {
		// encodeChangeImpact reads the same map shape we'd return as
		// JSON; routing through it keeps a single source of truth for
		// field names and avoids divergence on the next analyzer
		// addition.
		return s.gcxResponseWithBudget(req)(encodeChangeImpact(result))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

func impactComplete(impact *analysis.ImpactResult) bool {
	return impact != nil && !impact.LowerBound
}

// -----------------------------------------------------------------------------
// Contract impact helper
// -----------------------------------------------------------------------------

// contractImpact enumerates the contracts that reference one of the
// input type IDs as a request or response body, and rolls up the
// current validation issues for that subset so change-review sees
// breaking drift in the same payload as community / risk info.
type contractImpact struct {
	Affected     []contractImpactEntry     `json:"affected"`
	Breaking     int                       `json:"breaking"`
	Warning      int                       `json:"warning"`
	Info         int                       `json:"info"`
	SampleIssues []contracts.ContractIssue `json:"sample_issues,omitempty"`
}

type contractImpactEntry struct {
	ContractID string `json:"contract_id"`
	Position   string `json:"position"` // request | response
	Role       string `json:"role"`     // provider | consumer
	Repo       string `json:"repo"`
	TypeID     string `json:"type_id"`
}

// computeContractImpact walks every contract in the effective
// registry and returns the ones whose request_type or response_type
// matches any of the changed symbol IDs. Returns nil when nothing
// matches so the JSON payload stays compact.
type contractImpactNodeContextGetter interface {
	GetNodeContext(context.Context, string) (*graph.Node, error)
}

// computeContractImpact keeps non-interactive callers bounded while the MCP
// impact handler supplies its stricter request-scoped deadline below.
func (s *Server) computeContractImpact(changedIDs []string) *contractImpact {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.computeContractImpactContext(ctx, changedIDs)
}

func (s *Server) computeContractImpactContext(ctx context.Context, changedIDs []string) *contractImpact {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil
	}
	reg := s.effectiveContractRegistry()
	if reg == nil || ctx.Err() != nil {
		return nil
	}
	allContracts := reg.All()
	changed := make(map[string]struct{}, len(changedIDs))
	for _, id := range changedIDs {
		changed[id] = struct{}{}
	}

	var entries []contractImpactEntry
	affectedIDs := make(map[string]struct{})
	for _, c := range allContracts {
		if ctx.Err() != nil {
			return nil
		}
		reqType := impactMetaString(c.Meta, "request_type")
		respType := impactMetaString(c.Meta, "response_type")
		if _, hit := changed[reqType]; hit && reqType != "" {
			entries = append(entries, contractImpactEntry{
				ContractID: c.ID, Position: "request",
				Role: string(c.Role), Repo: c.RepoPrefix, TypeID: reqType,
			})
			affectedIDs[c.ID] = struct{}{}
		}
		if _, hit := changed[respType]; hit && respType != "" {
			entries = append(entries, contractImpactEntry{
				ContractID: c.ID, Position: "response",
				Role: string(c.Role), Repo: c.RepoPrefix, TypeID: respType,
			})
			affectedIDs[c.ID] = struct{}{}
		}
	}
	if len(entries) == 0 {
		return nil
	}

	// Validate the affected subset only — Validate on the full
	// registry would drown the payload in unrelated drift.
	sub := contracts.NewRegistry()
	for _, c := range allContracts {
		if ctx.Err() != nil {
			return nil
		}
		if _, ok := affectedIDs[c.ID]; ok {
			sub.Add(c)
		}
	}
	aborted := false
	reader := s.readerFor(ctx)
	lookup := contracts.ShapeLookup(func(id string) *contracts.Shape {
		if ctx.Err() != nil || reader == nil {
			aborted = true
			return nil
		}
		var n *graph.Node
		if getter, ok := reader.(contractImpactNodeContextGetter); ok {
			var err error
			n, err = getter.GetNodeContext(ctx, id)
			if err != nil {
				aborted = true
				return nil
			}
		} else {
			// Third-party and in-memory stores retain the existing Store
			// contract. Check cancellation immediately around the fallback;
			// production SQLite implements GetNodeContext above.
			if ctx.Err() != nil {
				aborted = true
				return nil
			}
			n = reader.GetNode(id)
			if ctx.Err() != nil {
				aborted = true
				return nil
			}
		}
		if n == nil || n.Meta == nil {
			return nil
		}
		switch v := n.Meta["shape"].(type) {
		case *contracts.Shape:
			return v
		case contracts.Shape:
			return &v
		}
		return nil
	})
	issues := contracts.Validate(sub, lookup)
	if aborted || ctx.Err() != nil {
		return nil
	}

	out := &contractImpact{Affected: entries}
	for _, is := range issues {
		switch is.Severity {
		case contracts.SeverityBreaking:
			out.Breaking++
		case contracts.SeverityWarning:
			out.Warning++
		case contracts.SeverityInfo:
			out.Info++
		}
	}
	// Keep the first 10 issues inline; full list is always one
	// `contracts validate` call away.
	if len(issues) > 10 {
		out.SampleIssues = issues[:10]
	} else {
		out.SampleIssues = issues
	}
	return out
}

func impactMetaString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// CrossCommunityWarning describes cross-community impact.
//
// It carries the affected community names and nothing else on purpose: the
// mandatory impact path must never perform a graph-wide coupling scan, so
// there is deliberately no field here for per-pair coupling scores.
type CrossCommunityWarning struct {
	AffectedCommunities []string `json:"affected_communities"`
}

// computeCrossCommunityWarning names the communities a change reaches.
//
// It deliberately reports no per-pair coupling score: scoring a pair requires
// the whole edge table, which is far too much work for an interactive safety
// gate that runs before every edit. Callers who want the numbers ask for the
// dedicated coupling analysis, which is scoped and budgeted for it.
func (s *Server) computeCrossCommunityWarning(affectedCommunities []string, _ *analysis.CommunityResult) *CrossCommunityWarning {
	return &CrossCommunityWarning{AffectedCommunities: affectedCommunities}
}
