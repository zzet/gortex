package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

// trackAcceptedHeadroom is how long before the MCP request deadline the
// track handler stops waiting for the first index and answers `accepted`.
// Without the headroom the daemon aborts the request first and the caller
// sees a bare timeout instead of a usable answer.
const trackAcceptedHeadroom = 5 * time.Second

// registerMultiRepoTools registers MCP tools for multi-repo management:
// track_repository, untrack_repository, set_active_project, get_active_project.
func (s *Server) registerMultiRepoTools() {
	s.addTool(
		mcp.NewTool("track_repository",
			mcp.WithDescription("Add a repository to the tracked workspace at runtime. Indexes immediately and persists to config."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to repository")),
			mcp.WithString("name", mcp.Description("Optional repo prefix override")),
			mcp.WithBoolean("as_worktree", mcp.Description("Track a linked git worktree as an independent instance (derived `<base>@<workspace>` prefix) even when its repo is already tracked elsewhere. Auto-detected when the worktree's .gortex.yaml declares a different workspace; set this to force it.")),
			mcp.WithBoolean("force", mcp.Description("Track even when the path is the home directory or filesystem root (refused by default to avoid an unbounded crawl). Operator/CLI channel only — refused for agent sessions, since the tracked-root set is the file-access boundary.")),
		),
		s.handleTrackRepository,
	)

	s.addTool(
		mcp.NewTool("untrack_repository",
			mcp.WithDescription("Remove a repository from the tracked workspace at runtime. Evicts nodes/edges and persists to config. "+
				"A checkout the family can still serve from another corpus is demoted into the automatic lane and the call runs "+
				"outright; a plan that removes rows — a primary corpus with its closure, or a checkout with nowhere to be demoted "+
				"to — is previewed instead and needs confirm:true."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path or repo prefix to remove")),
			mcp.WithBoolean("confirm", mcp.Description("Run a plan that removes rows. Without it such a plan is only previewed.")),
		),
		s.handleUntrackRepository,
	)

	s.addTool(
		mcp.NewTool("set_active_project",
			mcp.WithDescription("Switch the active project scope. Persists to config and re-scopes all subsequent queries."),
			mcp.WithString("project", mcp.Required(), mcp.Description("Project name to activate")),
		),
		s.handleSetActiveProject,
	)

	s.addTool(
		mcp.NewTool("get_active_project",
			mcp.WithDescription("Return the current active project name and its list of member repositories."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetActiveProject,
	)

	s.addTool(
		mcp.NewTool("query_project",
			mcp.WithDescription("Search symbols in another project or repository without switching the "+
				"active project. A read-only, one-shot cross-project lookup: it resolves the named "+
				"project (or a bare tracked-repo prefix), searches it, and returns — the active project "+
				"and the session scope are left unchanged. Use this instead of set_active_project for a "+
				"quick look into another project."),
			mcp.WithString("project", mcp.Required(),
				mcp.Description("Project name, per-repo project tag, or tracked-repo prefix to search")),
			mcp.WithString("query", mcp.Required(), mcp.Description("Symbol search query")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
		),
		s.handleQueryProject,
	)
}

// handleTrackRepository validates the path, indexes the repo, and persists to GlobalConfig.
func (s *Server) handleTrackRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Validate path exists and is a directory.
	info, statErr := os.Stat(path)
	if statErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid path: %s", path)), nil
	}
	if !info.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("invalid path: %s (not a directory)", path)), nil
	}

	if s.multiIndexer == nil {
		return mcp.NewToolResultError("multi-repo indexing is not enabled"), nil
	}

	entry := config.RepoEntry{Path: path}
	if name, ok := req.GetArguments()["name"].(string); ok && name != "" {
		entry.Name = name
	}
	if asWT, ok := req.GetArguments()["as_worktree"].(bool); ok {
		entry.AsWorktree = asWT
	}
	if force, ok := req.GetArguments()["force"].(bool); ok && force {
		// force skips unsafeRootBlocked, the guard that refuses `/`, a
		// Windows drive root and $HOME. That guard is not only a crawl
		// bound: the tracked-root set IS the confinement boundary for
		// every file-path tool (guardRepoRoots), so tracking `/` would
		// widen read and write access to the whole filesystem — and the
		// change persists to the global config. An agent session must not
		// be able to do that to itself; the operator CLI still can.
		if s.confineCallerPaths(ctx) {
			return mcp.NewToolResultError(
				"force is not available to an agent session: it would track a root such as / or " +
					"your home directory, widening file access for every tool and persisting that to " +
					"your global config. Track the specific repository you need instead."), nil
		}
		entry.Force = true
	}

	// A fresh repo's first index routinely outruns the MCP request deadline,
	// and registration only lands *after* TrackRepoCtx returns — so the
	// cancelled index left the repo untracked and a retry just repeated the
	// cycle (#326). Run the index on a context detached from the request and
	// answer `accepted` before the deadline, so the work continues
	// server-side the way boot indexing does.
	absPath, absErr := filepath.Abs(path)
	if absErr != nil {
		absPath = path
	}
	if _, busy := s.trackInFlight.LoadOrStore(absPath, struct{}{}); busy {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status": "accepted",
			"path":   path,
			"detail": "an initial index for this path is already running; call track again later to read its result",
		})
	}

	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}

	type trackOutcome struct {
		result indexer.RegisterResult
		err    error
	}
	done := make(chan trackOutcome, 1)
	go func() {
		defer s.trackInFlight.Delete(absPath)
		// WithoutCancel keeps the request's values (progress token, session)
		// while dropping its cancellation, so the daemon's request lifetime
		// can no longer kill a half-written first index.
		//
		// Registration also persists the config, attaches the watcher and
		// invalidates every session's cached workspace binding — the last of
		// which is what stops the session that ran `track` to repair its own
		// uncovered cwd from staying blind to the repo it just added. It
		// happens inside the goroutine because the caller may already have
		// answered `accepted` and returned.
		res, trackErr := s.lifecycle.Register(
			s.progressCtx(context.WithoutCancel(ctx), req), entry, indexer.TrackSourceMCP)
		if trackErr == nil && res.CatalogErr != nil {
			s.logger.Warn("track: recording the checkout identity failed",
				zap.String("path", path), zap.Error(res.CatalogErr))
		}
		done <- trackOutcome{result: res, err: trackErr}
	}()

	// A nil channel blocks forever, so a request with no deadline (CLI, tests)
	// keeps today's fully synchronous contract.
	var answerBy <-chan time.Time
	if deadline, ok := ctx.Deadline(); ok {
		wait := max(time.Until(deadline)-trackAcceptedHeadroom, 0)
		timer := time.NewTimer(wait)
		defer timer.Stop()
		answerBy = timer.C
	}

	select {
	case out := <-done:
		if out.err != nil {
			return mcp.NewToolResultError(out.err.Error()), nil
		}
		// Already tracked — the corpus held the repo, so only its identity
		// and side effects were brought up to date.
		if out.result.AlreadyTracked {
			return mcp.NewToolResultText("repository already tracked"), nil
		}
		prefix := out.result.Prefix
		if prefix == "" {
			prefix = config.ResolvePrefix(entry)
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":     "tracked",
			"path":       path,
			"prefix":     prefix,
			"file_count": out.result.Index.FileCount,
			"node_count": out.result.Index.NodeCount,
			"edge_count": out.result.Index.EdgeCount,
		})
	case <-answerBy:
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status": "accepted",
			"path":   path,
			"detail": "initial index is still running server-side and is no longer bound to this request; call track again to read the tracked result",
		})
	}
}

// handleUntrackRepository removes a repo from the workspace and persists to GlobalConfig.
func (s *Server) handleUntrackRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	if s.multiIndexer == nil {
		return mcp.NewToolResultError("multi-repo indexing is not enabled"), nil
	}
	if s.lifecycle == nil {
		return mcp.NewToolResultError("checkout lifecycle is not wired"), nil
	}

	// What an untrack of this path does is a property of the catalog, so it is
	// read before anything is torn down. The lifecycle uses the fail-closed
	// destructive resolver; an unknown token is guidance, never a path relative
	// to the daemon's working directory. A plan that removes rows is shown and
	// not run; a plan that keeps the checkout is the ordinary untrack.
	preview, err := s.lifecycle.PreviewUntrack(ctx, path)
	if errors.Is(err, indexer.ErrCheckoutNotTracked) {
		return repoNotTrackedGuidance(path), nil
	}
	if err != nil {
		return untrackFailure(path, err), nil
	}
	if destructiveUntrackPlan(preview.Plan) && !req.GetBool("confirm", false) {
		return s.respondJSONOrTOON(ctx, req, untrackPreviewPayload("untrack", preview,
			"nothing was written; call untrack_repository again with confirm:true to run this plan"))
	}

	// The lifecycle revokes the tracking intents, runs the plan's saga and
	// drives every side effect from it: watcher detach, graph eviction,
	// config persist, session invalidation, analysis rerun.
	result, err := s.lifecycle.ApplyUntrack(ctx, preview)
	if err != nil {
		return untrackFailure(path, err), nil
	}
	status := "untracked"
	if result.Demoted {
		status = "demoted"
	}
	return s.respondJSONOrTOON(ctx, req, untrackResultPayload(status, result))
}

// handleSetActiveProject validates the project name, updates the active project,
// persists to GlobalConfig, and re-scopes queries.
func (s *Server) handleSetActiveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, err := req.RequireString("project")
	if err != nil {
		return mcp.NewToolResultError("project is required"), nil
	}

	if s.configManager == nil {
		return mcp.NewToolResultError("configuration manager is not available"), nil
	}

	gc := s.configManager.Global()

	// Validate project exists.
	repos, resolveErr := gc.ResolveRepos(project)
	if resolveErr != nil {
		// Build list of available projects for the error message.
		available := make([]string, 0, len(gc.Projects))
		for name := range gc.Projects {
			available = append(available, name)
		}
		return mcp.NewToolResultError(fmt.Sprintf(
			"project not found: %s (available: %s)", project, strings.Join(available, ", "),
		)), nil
	}

	// Update active project in config and on server.
	gc.ActiveProject = project
	s.activeProject = project

	// Persist to disk.
	if saveErr := gc.Save(); saveErr != nil {
		s.logger.Warn("failed to persist active project change",
			zap.String("project", project), zap.Error(saveErr))
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":  "active",
		"project": project,
		"repos":   buildRepoList(repos),
	})
}

// handleGetActiveProject returns the current active project name and its repo list.
func (s *Server) handleGetActiveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.respondJSONOrTOON(ctx, req, s.buildActiveProjectPayload(ctx))
}

// buildActiveProjectPayload returns the same data the `get_active_project`
// tool emits. Shared with the `gortex://active-project` resource.
//
// For a workspace-bound session it reports the session's own resolved
// scope — the boundary the query tools actually enforce — rather than
// the process-global config default, which would mask whether scoping
// is in effect.
func (s *Server) buildActiveProjectPayload(ctx context.Context) map[string]any {
	if sessWS, sessProj, bound := s.sessionScope(ctx); bound {
		return map[string]any{
			"workspace": sessWS,
			"project":   sessProj,
			"bound":     true,
			"repos":     s.sessionWorkspaceRepos(ctx),
		}
	}

	if s.configManager == nil {
		return map[string]any{
			"project": "",
			"repos":   []any{},
		}
	}

	gc := s.configManager.Global()
	project := s.activeProject
	if project == "" {
		project = gc.ActiveProject
	}

	result := map[string]any{
		"project": project,
	}

	if project == "" {
		result["repos"] = buildRepoList(gc.Repos)
		return result
	}

	repos, resolveErr := gc.ResolveRepos(project)
	if resolveErr != nil {
		// Common after the workspace bind drops to "unbound"
		// while a stale active_project still points at a project
		// the workspace no longer discovers. Fall back to the
		// workspace-level repo list and record the drift in `note`.
		result["project"] = ""
		result["repos"] = buildRepoList(gc.Repos)
		result["note"] = fmt.Sprintf("active_project %q not found in current workspace; returning top-level repos", project)
		return result
	}

	result["repos"] = buildRepoList(repos)
	return result
}

// resolveRepoPrefix resolves a path-or-prefix string to a repo prefix by
// consulting only the in-memory MultiIndexer state. Use
// resolveRepoPrefixOrReconcile when drift between persisted config and
// in-memory state could produce a false miss.
func (s *Server) resolveRepoPrefix(pathOrPrefix string) string {
	if s.multiIndexer == nil || pathOrPrefix == "" {
		return ""
	}

	// Check if it is a known prefix directly.
	if meta := s.multiIndexer.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix
	}

	// Files and working directories inside a tracked repository are valid
	// selectors too. RepoForFile owns the longest-root and canonical-alias
	// rules shared with every request path; duplicating them here previously
	// made administrative selectors disagree with graph routing.
	absInput, err := filepath.Abs(pathOrPrefix)
	if err != nil {
		return ""
	}
	return s.multiIndexer.RepoForFile(absInput)
}

// diffJoinPrefix resolves the graph repo prefix used to join repo-relative
// diff / forge file paths to indexed nodes: multi-repo daemons key file
// paths as "<prefix>/<rel>" while git and forge APIs emit repo-relative
// paths. repoRoot is the already-resolved working-tree root. Returns "" in
// single-repo / unprefixed mode, where the raw lookup already matches.
func (s *Server) diffJoinPrefix(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	if p := s.resolveRepoPrefix(repoRoot); p != "" {
		return p
	}
	if s.indexer != nil && s.indexer.RootPath() == repoRoot {
		return s.indexer.RepoPrefix()
	}
	return ""
}

// diffRepoScope resolves the working-tree root and graph repo prefix a
// diff-driven handler operates on. An explicit selector (a repo prefix or a
// filesystem path — the CLI defaults to the caller's working directory) is
// normalized and honoured exclusively: when it names nothing tracked the
// result is empty so the caller errors instead of silently diffing another
// repo. With no selector the lone tracked repo wins, then the session's
// cwd-bound repo (clients dial the daemon with their working directory).
// Both empty means no resolvable working tree.
func (s *Server) diffRepoScope(ctx context.Context, repo string) (repoRoot, repoPrefix string) {
	if repo != "" {
		if p := s.resolveRepoPrefix(repo); p != "" {
			repo = p
		}
		// A selector still has to stay inside the session. Every other
		// scope path intersects an explicit repo with the session's
		// ceiling; this one resolved against the whole tracked set, so a
		// session bound to one repo could name another and diff its
		// working tree.
		if ceiling := s.sessionRepoCeiling(ctx); len(ceiling) > 0 && !ceiling[repo] {
			return "", ""
		}
		root := pickRepoRoot(s.collectRepoRoots(repo), repo)
		if root == "" {
			return "", ""
		}
		return root, s.diffJoinPrefix(root)
	}
	if root := pickRepoRoot(s.collectRepoRoots(""), ""); root != "" {
		return root, s.diffJoinPrefix(root)
	}
	if cwd := SessionCWDFromContext(ctx); cwd != "" && s.multiIndexer != nil {
		if _, _, prefix, ok := s.multiIndexer.ScopeForCWD(cwd); ok && prefix != "" {
			if root, ok := s.multiIndexer.RepoRoot(prefix); ok && root != "" {
				return root, prefix
			}
		}
	}
	// A session bound to the repos its cwd CONTAINS has no home repo, so
	// none of the branches above resolve. When the ceiling names exactly
	// one repo that is still an unambiguous answer; several is genuinely
	// ambiguous and must stay unresolved so the caller asks rather than
	// guesses.
	if ceiling := s.sessionRepoCeiling(ctx); len(ceiling) == 1 {
		for prefix := range ceiling {
			if root, ok := s.multiIndexer.RepoRoot(prefix); ok && root != "" {
				return root, prefix
			}
		}
	}
	return "", ""
}

// resolveDiffRoot is diffRepoScope plus the standalone-server fallback,
// with a single rule for when "." is a legitimate working tree.
//
// "." is the daemon PROCESS's cwd. That is the right answer only for a
// standalone, indexer-less server started inside the tree it serves.
// Inside the daemon it is wherever `gortex daemon start` happened to run
// — unrelated to the caller, and not necessarily a tree the session is
// scoped to at all. Falling back to it there answers a question nobody
// asked, from a directory the session may not be entitled to see.
//
// The session shapes that reach here are the ones with no home repo: a
// session bound to several contained repos, or one whose cwd resolves to
// no repo. Both want an actionable error naming the candidates, not a
// silent diff of the daemon's launch directory.
func (s *Server) resolveDiffRoot(ctx context.Context, repo string) (repoRoot, repoPrefix string, err error) {
	repoRoot, repoPrefix = s.diffRepoScope(ctx, repo)
	if repoRoot != "" {
		return repoRoot, repoPrefix, nil
	}
	if s.multiIndexer == nil {
		return ".", repoPrefix, nil
	}
	if repo != "" {
		return "", "", fmt.Errorf("repo %q names no tracked repository with a working tree", repo)
	}
	if candidates := sortedRepoNames(s.sessionRepoCeiling(ctx)); len(candidates) > 0 {
		return "", "", fmt.Errorf(
			"this session spans %d repositories (%s) — pass repo:<name> to say which working tree to diff",
			len(candidates), strings.Join(candidates, ", "))
	}
	return "", "", fmt.Errorf("no working tree resolved for this session — pass repo:<name>")
}

// sortedRepoNames renders a repo allow-set deterministically for an error
// message.
func sortedRepoNames(repos map[string]bool) []string {
	if len(repos) == 0 {
		return nil
	}
	out := make([]string, 0, len(repos))
	for p := range repos {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// resolveRepoPrefixOrReconcile resolves a path-or-prefix to a repo prefix
// and reconciles persisted-config state into the in-memory MultiIndexer on
// miss. Warmup can silently drop a repo (transient index failure, daemon
// restart with a stale snapshot, crash mid-warmup) and leave it listed
// under get_active_project but absent from mi.repos; the user's next
// operation then errors with "not a tracked repository" for something
// they can plainly see in the project list. Here, if the input matches a
// persisted config entry, we auto-track it before returning the prefix.
func (s *Server) resolveRepoPrefixOrReconcile(ctx context.Context, pathOrPrefix string) string {
	if prefix := s.resolveRepoPrefix(pathOrPrefix); prefix != "" {
		return prefix
	}
	if s.multiIndexer == nil || s.configManager == nil {
		return ""
	}

	absInput, _ := filepath.Abs(pathOrPrefix)
	for _, entry := range s.configManager.Global().Repos {
		entryAbs, _ := filepath.Abs(entry.Path)
		if entry.Path != pathOrPrefix && entryAbs != absInput &&
			config.ResolvePrefix(entry) != pathOrPrefix {
			continue
		}
		if _, err := s.multiIndexer.TrackRepoCtx(ctx, entry); err != nil {
			s.logger.Warn("auto-track from config failed",
				zap.String("path", entry.Path), zap.Error(err))
			return ""
		}
		return s.resolveRepoPrefix(pathOrPrefix)
	}
	return ""
}

// buildRepoList converts a slice of RepoEntry to a JSON-friendly list.
func buildRepoList(repos []config.RepoEntry) []map[string]string {
	list := make([]map[string]string, 0, len(repos))
	for _, r := range repos {
		entry := map[string]string{
			"path":   r.Path,
			"prefix": config.ResolvePrefix(r),
		}
		if r.Ref != "" {
			entry["ref"] = r.Ref
		}
		list = append(list, entry)
	}
	return list
}
