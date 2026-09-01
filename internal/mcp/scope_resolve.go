package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type ResolvedScope struct {
	WorkspaceID string
	ProjectID   string
	RepoAllow   map[string]bool
	Applied     string
}

type ToolIntent string

const (
	IntentLocate  ToolIntent = "locate"
	IntentReach   ToolIntent = "reach"
	IntentAnalyze ToolIntent = "analyze"
)

func (s *Server) resolveScope(ctx context.Context, req mcp.CallToolRequest, intent ToolIntent) (ResolvedScope, *mcp.CallToolResult) {
	scope, err := s.resolveScopeForRequest(ctx, req, intent)
	if err != nil {
		return ResolvedScope{}, mcp.NewToolResultError(err.Error())
	}
	return scope, nil
}

func (s *Server) resolveScopeForRequest(ctx context.Context, req mcp.CallToolRequest, intent ToolIntent) (ResolvedScope, error) {
	intent = normalizeToolIntent(intent, req.Params.Name)

	repo := strings.TrimSpace(req.GetString("repo", ""))
	// The selector may be a filesystem path (the CLI defaults to the
	// caller's working directory) -- normalize to the tracked prefix so
	// the filter matches what the workspace knows the repo as.
	if repo != "*" {
		if p := s.resolveRepoPrefix(repo); p != "" {
			repo = p
		}
	}
	project := strings.TrimSpace(req.GetString("project", ""))
	ref := strings.TrimSpace(req.GetString("ref", ""))
	workspaceArg := strings.TrimSpace(req.GetString("workspace", ""))

	scopeArg := strings.TrimSpace(req.GetString("scope", ""))
	if gitDiffScopes[scopeArg] {
		scopeArg = ""
	}
	explicitNarrowing := repo != "" || project != "" || ref != "" || workspaceArg != "" || scopeArg != ""

	var scopeRepos map[string]bool
	if scopeArg != "" && repo == "" && project == "" && ref == "" {
		sc, ok := s.lookupScope(scopeArg)
		if !ok {
			return ResolvedScope{}, fmt.Errorf("unknown scope %q — run list_scopes to see saved scopes, or create one with save_scope", scopeArg)
		}
		scopeRepos = s.scopeRepoSet(sc)
		if len(scopeRepos) == 0 {
			return ResolvedScope{}, fmt.Errorf("saved scope %q names no repositories", scopeArg)
		}
	}

	if sessWS, sessProj, bound := s.sessionScope(ctx); bound {
		return s.resolveBoundSessionScope(ctx, sessWS, sessProj, workspaceArg, project, repo, ref, scopeArg, scopeRepos, intent, explicitNarrowing)
	}
	return s.resolveUnboundScope(ctx, workspaceArg, project, repo, ref, scopeArg, scopeRepos, intent, explicitNarrowing)
}

func normalizeToolIntent(intent ToolIntent, toolName string) ToolIntent {
	switch intent {
	case IntentLocate, IntentReach, IntentAnalyze:
		return intent
	default:
		return toolIntentForName(toolName)
	}
}

func (s *Server) resolveBoundSessionScope(ctx context.Context, sessWS, sessProj, workspaceArg, project, repo, ref, scopeArg string, scopeRepos map[string]bool, intent ToolIntent, explicitNarrowing bool) (ResolvedScope, error) {
	resolved := ResolvedScope{
		WorkspaceID: sessWS,
		ProjectID:   sessProj,
		Applied:     appliedProjectOrWorkspace(sessProj),
	}
	if project != "" {
		resolved.ProjectID = project
		resolved.Applied = "project:" + project
	}

	// ceiling is non-nil only for a session bound to the repos its cwd
	// CONTAINS: that shape has no single workspace slug, so the repo set
	// is the boundary and every branch that would otherwise leave
	// RepoAllow nil must carry it instead. Nil for every other shape,
	// where the slug in resolved.WorkspaceID is the whole boundary and
	// nil keeps today's behaviour byte-for-byte.
	ceiling := s.sessionRepoCeiling(ctx)

	// A `workspace` arg may only name a workspace the session is already
	// inside. Any other value is a cross-workspace escape attempt --
	// reject it outright rather than silently honouring the boundary and
	// returning a confusing empty result. A contained-repo session spans
	// several, so it accepts any of them and narrows to that one.
	if workspaceArg != "" && workspaceArg != sessWS {
		spanned := s.sessionScopeWorkspaces(ctx)
		if !spanned[workspaceArg] {
			return ResolvedScope{}, fmt.Errorf(
				"workspace %q is outside the active workspace %s; cross-workspace queries are not permitted",
				workspaceArg, describeSessionWorkspaces(sessWS, spanned))
		}
		// Narrow to the named workspace's slice of the ceiling. The
		// intersection keeps containment authoritative: a repo declaring
		// this slug from outside the session's cwd stays invisible.
		narrowed := intersectRepoSets(s.multiIndexer.ReposInWorkspace(workspaceArg), ceiling)
		if len(narrowed) == 0 {
			return ResolvedScope{}, fmt.Errorf(
				"workspace %q names no repository inside this session", workspaceArg)
		}
		resolved.WorkspaceID = workspaceArg
		resolved.ProjectID = project
		resolved.RepoAllow = narrowed
		resolved.Applied = "workspace:" + workspaceArg
		if project == "" && repo == "" && ref == "" && scopeRepos == nil {
			return resolved, nil
		}
		ceiling = narrowed
	}
	if workspaceArg != "" && workspaceArg == sessWS && project == "" && repo == "" && ref == "" && scopeRepos == nil {
		resolved.ProjectID = ""
		resolved.RepoAllow = ceiling
		resolved.Applied = "workspace"
		return resolved, nil
	}

	wsRepos := ceiling
	if wsRepos == nil {
		wsRepos = map[string]bool{}
		if s.multiIndexer != nil {
			wsRepos = s.multiIndexer.ReposInWorkspace(sessWS)
		}
	}

	if !explicitNarrowing {
		if s.scopeIntentDefaultsEnabled() {
			return s.applyIntentDefault(ctx, resolved, intent), nil
		}
		// Layer-A compatibility: no explicit narrowing in a bound session
		// stays on the session project, with the repo allow-set clamped to
		// the whole workspace for handlers that filter by repo prefix.
		resolved.RepoAllow = wsRepos
		resolved.Applied = appliedProjectOrWorkspace(resolved.ProjectID)
		return resolved, nil
	}

	// A named scope, intersected with the workspace so it can only ever
	// narrow -- a scope is a convenience, never a clamp escape.
	if scopeRepos != nil {
		intersected := intersectRepoSets(scopeRepos, wsRepos)
		if len(intersected) == 0 {
			return ResolvedScope{}, fmt.Errorf(
				"saved scope %q resolves to nothing inside the active workspace %s",
				scopeArg, describeSessionWorkspaces(sessWS, s.sessionScopeWorkspaces(ctx)))
		}
		resolved.ProjectID = ""
		resolved.RepoAllow = intersected
		resolved.Applied = "scope:" + scopeArg
		return resolved, nil
	}

	if repo == "*" && project == "" && ref == "" {
		resolved.ProjectID = ""
		resolved.RepoAllow = ceiling
		resolved.Applied = "workspace"
		return resolved, nil
	}
	if repo == "*" {
		repo = ""
	}

	// Explicit narrowing: resolve the args, then intersect with the
	// workspace so a repo/project/ref arg can never escape it.
	narrowed, err := s.resolveRepoFilterArgs(repo, project, ref, false)
	if err != nil {
		return ResolvedScope{}, err
	}
	if narrowed == nil {
		resolved.ProjectID = project
		resolved.RepoAllow = ceiling
		resolved.Applied = appliedForExplicit(project, repo, ref, ceiling)
		return resolved, nil
	}
	intersected := intersectRepoSets(narrowed, wsRepos)
	if len(intersected) == 0 {
		return ResolvedScope{}, fmt.Errorf(
			"repo/project/ref filter resolves to nothing inside the active workspace %s; cross-workspace queries are not permitted",
			describeSessionWorkspaces(sessWS, s.sessionScopeWorkspaces(ctx)))
	}
	resolved.ProjectID = ""
	if project != "" {
		resolved.ProjectID = project
	}
	resolved.RepoAllow = intersected
	resolved.Applied = appliedForExplicit(project, repo, ref, intersected)
	return resolved, nil
}

func (s *Server) resolveUnboundScope(ctx context.Context, workspaceArg, project, repo, ref, scopeArg string, scopeRepos map[string]bool, intent ToolIntent, explicitNarrowing bool) (ResolvedScope, error) {
	resolved := ResolvedScope{
		WorkspaceID: s.scopeWorkspace,
		ProjectID:   s.scopeProject,
		Applied:     appliedProjectOrWorkspace(s.scopeProject),
	}
	if workspaceArg != "" {
		resolved.WorkspaceID = workspaceArg
		if project == "" {
			resolved.ProjectID = ""
			resolved.Applied = "workspace"
		}
	}
	if project != "" {
		resolved.ProjectID = project
		resolved.Applied = "project:" + project
	}

	if !explicitNarrowing && s.scopeIntentDefaultsEnabled() {
		return s.applyIntentDefault(ctx, resolved, intent), nil
	}

	if scopeRepos != nil {
		resolved.ProjectID = ""
		resolved.RepoAllow = scopeRepos
		resolved.Applied = "scope:" + scopeArg
		return resolved, nil
	}
	if repo == "*" && project == "" && ref == "" {
		resolved.ProjectID = ""
		resolved.RepoAllow = nil
		resolved.Applied = "workspace"
		return resolved, nil
	}
	if repo == "*" {
		repo = ""
	}

	allowed, err := s.resolveRepoFilterArgs(repo, project, ref, !explicitNarrowing)
	if err != nil {
		return ResolvedScope{}, err
	}
	resolved.RepoAllow = allowed
	if repo != "" || project != "" || ref != "" {
		resolved.Applied = appliedForExplicit(project, repo, ref, allowed)
	} else if allowed != nil {
		resolved.Applied = appliedForExplicit(s.activeProject, "", "", allowed)
	}
	return resolved, nil
}

func (s *Server) scopeIntentDefaultsEnabled() bool {
	if s == nil {
		return true
	}
	return s.scopeIntentDefaults
}

func (s *Server) applyIntentDefault(ctx context.Context, resolved ResolvedScope, intent ToolIntent) ResolvedScope {
	resolved.ProjectID = ""
	// The session repo ceiling is the floor of every intent default, not
	// an optional extra: for a session bound to the repos its cwd
	// contains, resolved.WorkspaceID is empty, so leaving RepoAllow nil
	// here would make every reach / analyze call a full-graph pass.
	ceiling := s.sessionRepoCeiling(ctx)
	resolved.RepoAllow = ceiling
	resolved.Applied = "workspace"
	if len(ceiling) > 0 {
		resolved.Applied = appliedForRepoSet(ceiling)
	}
	if intent != IntentLocate {
		return resolved
	}
	repo, _ := s.sessionLocality(ctx)
	if repo == "" || (ceiling != nil && !ceiling[repo]) {
		return resolved
	}
	resolved.RepoAllow = map[string]bool{repo: true}
	resolved.Applied = "repo:" + repo
	return resolved
}

// appliedForRepoSet renders a repo allow-set for the `_meta.scope_applied`
// provenance line: the repo name when it is a single repo, a count
// otherwise. Display only — it carries no enforcement.
func appliedForRepoSet(repos map[string]bool) string {
	if len(repos) == 1 {
		for p := range repos {
			return "repo:" + p
		}
	}
	return fmt.Sprintf("repos:%d", len(repos))
}

// describeSessionWorkspaces renders a session's workspace boundary for an
// error message. A session bound to the repos its cwd contains has no
// single slug, so quoting the empty sessWS would read as `workspace ""`.
func describeSessionWorkspaces(sessWS string, spanned map[string]bool) string {
	if sessWS != "" || len(spanned) == 0 {
		return strconv.Quote(sessWS)
	}
	names := make([]string, 0, len(spanned))
	for w := range spanned {
		names = append(names, strconv.Quote(w))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func appliedProjectOrWorkspace(project string) string {
	if project != "" {
		return "project:" + project
	}
	return "workspace"
}

func appliedForExplicit(project, repo, ref string, allowed map[string]bool) string {
	switch {
	case repo != "" && repo != "*":
		return "repo:" + repo
	case project != "":
		return "project:" + project
	case ref != "":
		return "ref:" + ref
	case len(allowed) == 1:
		for p := range allowed {
			return "repo:" + p
		}
	case len(allowed) > 1:
		return fmt.Sprintf("repos:%d", len(allowed))
	}
	return "workspace"
}

func scopeApplied(scope ResolvedScope) string {
	if scope.Applied != "" {
		return scope.Applied
	}
	if len(scope.RepoAllow) == 1 {
		for repo := range scope.RepoAllow {
			return "repo:" + repo
		}
	}
	if len(scope.RepoAllow) > 1 {
		return fmt.Sprintf("repos:%d", len(scope.RepoAllow))
	}
	return appliedProjectOrWorkspace(scope.ProjectID)
}

func decorateResultWithScope(res *mcp.CallToolResult, scope ResolvedScope) *mcp.CallToolResult {
	if res == nil {
		return nil
	}
	fields := map[string]any{
		"scope_applied":    scopeApplied(scope),
		"scope_widen_hint": `to widen: pass repo:"*" or project:<name> or scope:<name>`,
	}
	return mergeResultMeta(res, fields)
}

// scopeZeroNote is the body-visible variant of the scope disclosure for a
// Locate result that came back EMPTY while repo narrowing was active. The
// scope_applied / scope_widen_hint fields ride in _meta, which CLI output
// and most MCP clients never render — without this note a scope-narrowed
// zero is indistinguishable from "not in the graph". outside is the number
// of candidates a workspace-wide recheck found beyond the narrowed scope;
// pass -1 when no recheck ran.
func scopeZeroNote(scope ResolvedScope, outside int) string {
	label := scopeApplied(scope)
	switch {
	case outside > 0:
		return fmt.Sprintf("0 results within the active scope (%s); %d candidate(s) match outside it — widen with repo:\"*\" or pass an explicit repo:/project:", label, outside)
	case outside == 0:
		return fmt.Sprintf("0 results within the active scope (%s); a workspace-wide recheck also found nothing", label)
	default:
		return fmt.Sprintf("0 results within the active scope (%s) — widen with repo:\"*\" or pass an explicit repo:/project:", label)
	}
}

func withScopeResult(res *mcp.CallToolResult, err error, scope ResolvedScope) (*mcp.CallToolResult, error) {
	if err != nil {
		return res, err
	}
	return decorateResultWithScope(res, scope), nil
}

func (s *Server) respondScopedJSONOrTOON(ctx context.Context, req mcp.CallToolRequest, payload any, scope ResolvedScope) (*mcp.CallToolResult, error) {
	res, err := s.respondJSONOrTOON(ctx, req, payload)
	return withScopeResult(res, err, scope)
}

func (s *Server) returnScopedSubGraph(ctx context.Context, req mcp.CallToolRequest, sg *query.SubGraph, scope ResolvedScope) (*mcp.CallToolResult, error) {
	res, err := s.returnSubGraph(ctx, req, sg)
	return withScopeResult(res, err, scope)
}

func intersectRepoSets(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for p := range a {
		if b[p] {
			out[p] = true
		}
	}
	return out
}

// sessionWorkspaceRepoSet returns the set of repo prefixes inside the
// current session's workspace and whether the session is workspace-
// bound. An unbound session (no cwd / no multi-repo indexer) returns
// (nil, false) — its callers must not clamp. This is the workspace
// ceiling for the analyze kinds that run a global graph algorithm and
// cannot honour the repo allow-set in v1.
func (s *Server) sessionWorkspaceRepoSet(ctx context.Context) (map[string]bool, bool) {
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound || s.multiIndexer == nil {
		return nil, false
	}
	// A session bound to the repos its cwd contains carries the ceiling
	// directly. A bound empty result is deliberately distinct from unbound:
	// it is a deny-all ceiling for an unresolved cwd, never permission to
	// widen back to the whole graph.
	if ceiling := s.sessionRepoCeiling(ctx); len(ceiling) > 0 {
		return ceiling, true
	}
	return s.multiIndexer.ReposInWorkspace(sessWS), true
}

// communitiesInSessionScope returns a workspace-clamped copy of cr for a
// workspace-bound session: each community keeps only the members / files
// inside the session workspace, and communities left with no in-workspace
// member are dropped. Community detection runs over the whole index (one
// shared partition), so without this the community analyze kinds
// (clusters / concepts / suggest_boundaries) would surface clusters from
// sibling workspaces — a breach of the workspace isolation boundary. The
// cached partition is never mutated: the copy gets fresh member / file
// slices. An unbound session gets cr back unchanged.
func (s *Server) communitiesInSessionScope(ctx context.Context, cr *analysis.CommunityResult) *analysis.CommunityResult {
	wsRepos, bound := s.sessionWorkspaceRepoSet(ctx)
	if !bound || cr == nil {
		return cr
	}
	out := *cr
	out.Communities = make([]analysis.Community, 0, len(cr.Communities))
	out.NodeToComm = make(map[string]string)
	if len(wsRepos) == 0 {
		out.Modularity = 0
	}
	for _, c := range cr.Communities {
		members := make([]string, 0, len(c.Members))
		for _, m := range c.Members {
			if wsRepos[graph.RepoPrefixOfID(m)] {
				members = append(members, m)
			}
		}
		if len(members) == 0 {
			continue
		}
		files := make([]string, 0, len(c.Files))
		for _, f := range c.Files {
			if wsRepos[graph.RepoPrefixOfID(f)] {
				files = append(files, f)
			}
		}
		// A cell that straddled the boundary keeps display fields derived
		// from the members that were just removed: Label is the modal
		// directory across the whole cell (so it can name a sibling repo
		// outright), Hub is the highest-degree member's symbol name, and
		// ParentID is a grouping key built from that Label. Recompute the
		// label over what survives and drop the two fields that cannot be
		// recomputed without the members that are gone.
		if len(members) != len(c.Members) {
			c.Label = analysis.RelabelNarrowedCommunity(members, files)
			c.Hub = ""
			c.ParentID = ""
		}
		c.Members = members
		c.Files = files
		c.Size = len(members)
		out.Communities = append(out.Communities, c)
		for _, member := range members {
			out.NodeToComm[member] = c.ID
		}
	}
	return &out
}

// repoPrefixVisible is the prefix-level analogue of analyzeNodeVisible for
// handlers whose rows carry a repo prefix instead of a graph node — a
// watcher event, a contract record. It applies the same two gates in the
// same order: the session-workspace ceiling first, then the resolved repo
// allow-set.
//
// Except for a bound-empty deny-all session, an unattributed row — one whose
// prefix is empty — is admitted by both
// gates. The allow-set half routes through repoNarrowAdmits, the single
// definition of what a repo narrow does to something carrying no prefix;
// the workspace half follows it for the same reason. Single-repo daemons
// emit unprefixed rows, so rejecting would not narrow a result, it would
// empty one. In multi-repo mode every row of both kinds this gates is
// attributed at its source, so the admitted case does not arise there.
func (s *Server) repoPrefixVisible(ctx context.Context, prefix string) bool {
	if wsRepos, bound := s.sessionWorkspaceRepoSet(ctx); bound {
		if len(wsRepos) == 0 {
			return false
		}
		if prefix != "" && !wsRepos[prefix] {
			return false
		}
	}
	return repoNarrowAdmits(repoAllowFromContext(ctx), prefix)
}

// processesInSessionScope returns a scope-clamped copy of pr: processes
// whose entry point the caller may not see are dropped, and each surviving
// chain has its out-of-scope steps excised. Process discovery runs over the
// whole index, so without this the process surface would hand a
// workspace-bound caller a sibling workspace's entry points, call chains and
// file lists. Strict no-op for an unbound session with no repo allow-set;
// the cached snapshot is never mutated.
//
// Excision is by subtree, not by element. Steps are a DFS preorder whose
// parent relation is implicit — the parent of a step is the nearest
// preceding step with a smaller depth (see analysis.Step). Dropping a step
// on its own would re-parent its children onto an unrelated ancestor and
// assert a call that does not exist. Removing an out-of-scope step at depth
// d together with the following run of steps at depth > d cannot do that:
// any survivor's parent has a strictly smaller depth than the survivor, so
// a parent inside the removed run would put the survivor inside it too.
// Depths are therefore left untouched and the surviving tree is exactly the
// pre-filter tree minus whole subtrees.
func (s *Server) processesInSessionScope(ctx context.Context, pr *analysis.ProcessResult) *analysis.ProcessResult {
	// The step / entry-point scope test is a syntactic split of the node id
	// (graph.RepoPrefixOfID), which only names a repository in multi-repo
	// mode — on an unprefixed single-repo id it returns the first directory
	// name. Bail out before that misreads a directory as a repo.
	if pr == nil || s.multiIndexer == nil {
		return pr
	}
	wsRepos, bound := s.sessionWorkspaceRepoSet(ctx)
	clampWorkspace := bound
	repoAllow := repoAllowFromContext(ctx)
	if !clampWorkspace && len(repoAllow) == 0 {
		return pr
	}
	inScope := func(id string) bool {
		prefix := graph.RepoPrefixOfID(id)
		if clampWorkspace && !wsRepos[prefix] {
			return false
		}
		return len(repoAllow) == 0 || repoAllow[prefix]
	}

	out := *pr
	out.Processes = make([]analysis.Process, 0, len(pr.Processes))
	out.NodeToProcs = make(map[string][]string, len(pr.NodeToProcs))
	for _, p := range pr.Processes {
		if !inScope(p.EntryPoint) {
			continue
		}
		steps := make([]analysis.Step, 0, len(p.Steps))
		excised := false
		for i := 0; i < len(p.Steps); i++ {
			step := p.Steps[i]
			if inScope(step.ID) {
				steps = append(steps, step)
				continue
			}
			// Skip this step and the whole subtree rooted at it: every
			// following step deeper than it is one of its descendants.
			excised = true
			for i+1 < len(p.Steps) && p.Steps[i+1].Depth > step.Depth {
				i++
			}
		}
		// A process is a chain; discovery itself discards anything
		// shorter than two steps (analysis.discoverProcesses).
		if len(steps) < 2 {
			continue
		}
		files := make([]string, 0, len(p.Files))
		for _, f := range p.Files {
			if inScope(f) {
				files = append(files, f)
			}
		}
		p.Steps = steps
		p.StepCount = len(steps)
		p.Files = files
		p.Truncated = p.Truncated || excised
		out.Processes = append(out.Processes, p)
		for _, step := range steps {
			out.NodeToProcs[step.ID] = append(out.NodeToProcs[step.ID], p.ID)
		}
	}
	return &out
}

// scopeNarrowingRequested reports whether the caller passed an explicit
// repo / project / scope / ref narrowing arg (other than the "*"
// all-repos escape hatch).
func scopeNarrowingRequested(req mcp.CallToolRequest) bool {
	if repo := strings.TrimSpace(req.GetString("repo", "")); repo != "" && repo != "*" {
		return true
	}
	return strings.TrimSpace(req.GetString("project", "")) != "" ||
		strings.TrimSpace(req.GetString("scope", "")) != "" ||
		strings.TrimSpace(req.GetString("ref", "")) != ""
}

// workspaceScopeBlock returns a body-visible disclosure of the scope a
// not-repo-narrowed analyze kind actually applied: it honours the session
// workspace boundary but cannot apply a repo / project narrow in v1.
// Unlike the _meta scope_note (which several clients, Claude Code among
// them, do not surface), this rides in the response payload so an agent
// always sees it. Returns nil for an unbound session — nothing to
// disclose, and the result was never clamped.
func (s *Server) workspaceScopeBlock(ctx context.Context, req mcp.CallToolRequest, kind string) map[string]any {
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		return nil
	}
	applied := "workspace"
	if sessWS != "" {
		applied = "workspace:" + sessWS
	}
	blk := map[string]any{"applied": applied}
	if scopeNarrowingRequested(req) {
		blk["note"] = "kind '" + kind + "' is clamped to the session workspace but is not repo/project-narrowed in v1; the requested narrowing was widened to the whole workspace"
	}
	return blk
}
