package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm/svc"
)

// forgeList / forgeFiles are the package-level seam over the forge free
// functions. They default to the real network-backed forge calls; a test
// swaps them for closures returning canned data so the PR tools exercise
// the graph-join and ranking logic with no network. Keep this the only
// indirection — every PR tool routes its self-served fetch through these.
var (
	forgeList  = forge.ListPRs
	forgeFiles = forge.PRFiles
)

// llmRerank is the package-level seam over the optional LLM re-rank used
// by triage_prs. It reports whether the service is usable (a constructed,
// enabled provider) and, when usable, runs one freeform Generate call.
// It defaults to routing through the server's real *svc.Service; a test
// swaps it for a closure returning a fixed ordering so the re-rank path
// is exercised with no provider and no network. Keep this the only
// indirection over the LLM call so the deterministic path stays pure.
var llmRerank = func(ctx context.Context, service *svc.Service, prompt string, maxTokens int) (text string, usable bool, err error) {
	if service == nil || !service.Enabled() {
		return "", false, nil
	}
	out, gerr := service.Generate(ctx, prompt, maxTokens)
	return out, true, gerr
}

// prCacheTTL is how long a fetched forge.PR is reused before a refetch.
// A short TTL means a triage re-run within the window does not refetch
// the same PR, while a stale list is never served for long.
const prCacheTTL = 60 * time.Second

// prCacheEntry is one cached forge.PR keyed by (repo, number), stamped
// with the time it was fetched so the TTL can be evaluated on read.
type prCacheEntry struct {
	pr        forge.PR
	fetchedAt time.Time
}

// prCache is a small TTL cache of forge.PR values per (repo, number). It
// is shared across PR-tool calls on a server so a triage fan-out plus a
// follow-up get_pr_impact on the same PR reuse one fetch within the TTL.
type prCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]prCacheEntry
}

// newPRCache builds an empty PR cache with the given TTL.
func newPRCache(ttl time.Duration) *prCache {
	return &prCache{ttl: ttl, m: make(map[string]prCacheEntry)}
}

// prCacheKey is the cache key for a PR — the repo prefix plus the number.
func prCacheKey(repo string, number int) string {
	return repo + "\x1f" + fmt.Sprintf("%d", number)
}

// get returns the cached PR for (repo, number) when present and still
// within the TTL.
func (c *prCache) get(repo string, number int) (forge.PR, bool) {
	if c == nil {
		return forge.PR{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[prCacheKey(repo, number)]
	if !ok {
		return forge.PR{}, false
	}
	if c.ttl > 0 && time.Since(e.fetchedAt) > c.ttl {
		delete(c.m, prCacheKey(repo, number))
		return forge.PR{}, false
	}
	return e.pr, true
}

// put records a freshly-fetched PR under (repo, number).
func (c *prCache) put(repo string, number int, pr forge.PR) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[prCacheKey(repo, number)] = prCacheEntry{pr: pr, fetchedAt: time.Now()}
}

// putList records a list-level PR (one with no hydrated Files) without
// clobbering an unexpired entry that already carries hydrated Files. A
// fresh list fan-out runs on every triage / conflicts call, but the PRs it
// carries have empty Files; overwriting a previously-hydrated entry with one
// would defeat the cache and force a refetch on the next run. So when a live
// hydrated entry exists, putList only refreshes its timestamp.
func (c *prCache) putList(repo string, number int, pr forge.PR) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := prCacheKey(repo, number)
	if e, ok := c.m[key]; ok && len(e.pr.Files) > 0 && len(pr.Files) == 0 {
		if c.ttl <= 0 || time.Since(e.fetchedAt) <= c.ttl {
			// Keep the hydrated PR; just slide the freshness window forward.
			e.fetchedAt = time.Now()
			c.m[key] = e
			return
		}
	}
	c.m[key] = prCacheEntry{pr: pr, fetchedAt: time.Now()}
}

// registerPRTools registers the data-only forge MCP surface — list_prs,
// get_pr_impact, triage_prs. All three are deferred (they land in the
// lazy catalog unless GORTEX_LAZY_TOOLS is off), read-only, and forge-
// self-serving: each fetches PR data via the daemon's own forge client
// (GH_TOKEN + the indexed repo identity), with an optional caller-
// supplied-data path to skip the network. None of them edits.
func (s *Server) registerPRTools() {
	s.addTool(
		mcp.NewTool("list_prs",
			mcp.WithDescription("List a repository's pull requests with a one-shot review-state classification. Each PR is reduced to a state label (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED / STALE / READY), a normalized CI rollup (NONE / FAILURE / PENDING / SUCCESS), and its merge blockers. The daemon self-serves the data via its own forge client (needs GH_TOKEN / GITHUB_TOKEN in the daemon environment); pass `prs` to classify an already-fetched set with no network call. Use to triage a review queue before opening any PR."),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithString("state", mcp.Description("PR state filter passed to the forge: open (default), closed, or all.")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of PRs fetched / returned (default 30).")),
			mcp.WithString("prs", mcp.Description("JSON array of already-fetched forge.PR objects to classify instead of calling the forge. Skips the network entirely.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleListPRs,
	)
	s.addTool(
		mcp.NewTool("get_pr_impact",
			mcp.WithDescription("Graph-joined blast radius and risk score for a pull request. Maps the PR's changed files to the symbols they define (whole-file granularity), scores PR-level risk across five axes (blast-radius flow, caller fan-in, coverage gap, security keywords, community span), and groups the affected surface by community and by caller/test file. The daemon self-serves the changed-file set via its forge client (needs GH_TOKEN / GITHUB_TOKEN); pass `files` to skip the fetch. Set `receipt:true` to additionally emit a small privacy-safe review receipt. Use to gauge how carefully a PR must be reviewed before reading the diff."),
			mcp.WithNumber("number", mcp.Required(), mcp.Description("GitHub PR number.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithString("files", mcp.Description("JSON array of already-fetched changed file paths to score instead of calling the forge. Skips the network entirely.")),
			mcp.WithBoolean("receipt", mcp.Description("When true, also emit a small machine-readable review receipt (counts, tier, next safe action, merge-blocker verdict) — no file paths or symbol IDs.")),
			mcp.WithBoolean("scrub", mcp.Description("When emitting a receipt, strip any path-like / symbol-ID-like / email-like value so the receipt is safe to share cross-org. No effect unless receipt is true.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleGetPRImpact,
	)
	s.addTool(
		mcp.NewTool("triage_prs",
			mcp.WithDescription("Rank a repository's open pull requests by graph-derived review priority. Computes get_pr_impact per PR and orders them by composite risk score (highest first, deterministic). The daemon self-serves the PR list and per-PR files via its forge client (needs GH_TOKEN / GITHUB_TOKEN); pass `prs` and/or `files` to supply already-fetched data and skip the fan-out. Use to decide which PR to review first."),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of open PRs triaged / returned (default 20).")),
			mcp.WithString("prs", mcp.Description("JSON array of already-fetched forge.PR objects to triage instead of listing via the forge.")),
			mcp.WithString("files", mcp.Description("JSON object mapping a PR number (as a string key) to its already-fetched changed file paths, so a per-PR file fetch is skipped.")),
			mcp.WithBoolean("use_llm", mcp.Description("When true and an LLM provider is configured, re-rank the deterministic queue with one compact LLM pass and attach a short per-PR rationale (llm_used:true). Falls back to the deterministic score-descending order (llm_used:false) when no provider is configured or the model response is unparseable — never errors.")),
			mcp.WithNumber("max_tokens", mcp.Description("Generation cap for the LLM re-rank pass (default 512). No effect unless use_llm is true and a provider is configured.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleTriagePRs,
	)
}

// forgeUnavailablePayload is the typed degradation returned when no token
// is resolvable and the caller supplied no data. It names GH_TOKEN so the
// operator knows exactly what the daemon environment is missing.
func forgeUnavailablePayload() map[string]any {
	return map[string]any{
		"error": "forge unavailable",
		"hint":  "set GH_TOKEN (or GITHUB_TOKEN) in the daemon environment",
	}
}

// rateLimitedPayload maps a forge.ErrRateLimited error onto the typed
// rate-limit degradation, extracting the Retry-After hint the forge layer
// encoded in the wrapped message.
func rateLimitedPayload(err error) map[string]any {
	out := map[string]any{"error": "rate limited"}
	if s := retryAfterSeconds(err); s >= 0 {
		out["retry_after_s"] = s
	}
	return out
}

// retryAfterSeconds extracts the whole-second Retry-After hint the forge
// layer wraps into a rate-limit error's message ("(retry after 30s)").
// Returns -1 when no hint is present.
func retryAfterSeconds(err error) int {
	if err == nil {
		return -1
	}
	msg := err.Error()
	const marker = "retry after "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return -1
	}
	rest := msg[idx+len(marker):]
	// rest looks like "30s)" or "1m30s)" — parse the leading Go duration.
	end := strings.IndexByte(rest, ')')
	if end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	d, perr := time.ParseDuration(rest)
	if perr != nil {
		return -1
	}
	return int(d.Seconds())
}

// isForgeUnavailable reports whether err is a "no token" forge error.
func isForgeUnavailable(err error) bool {
	return errors.Is(err, forge.ErrNotAuthenticated)
}

// handleListPRs lists (or accepts) a repository's PRs and classifies each
// into a review-state label + CI rollup + blockers.
func (s *Server) handleListPRs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo := strings.TrimSpace(req.GetString("repo", ""))
	state := strings.TrimSpace(req.GetString("state", ""))
	if state == "" {
		state = "open"
	}
	limit := req.GetInt("limit", 30)
	if limit < 1 {
		limit = 30
	}

	prs, supplied, err := s.parseSuppliedPRs(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !supplied {
		if !forge.Available(ctx) {
			return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
		}
		repoRoot, _ := s.diffRepoScope(ctx, repo)
		fetched, ferr := forgeList(ctx, repoRoot, forge.ListOpts{State: state, Limit: limit, WithDecision: true, WithCI: true})
		if ferr != nil {
			if errors.Is(ferr, forge.ErrRateLimited) {
				return s.respondJSONOrTOON(ctx, req, rateLimitedPayload(ferr))
			}
			if isForgeUnavailable(ferr) {
				return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
			}
			return mcp.NewToolResultError(fmt.Sprintf("listing PRs failed: %v", ferr)), nil
		}
		prs = fetched
		for _, pr := range prs {
			s.prCache.putList(repo, pr.Number, pr)
		}
	}

	if limit > 0 && len(prs) > limit {
		prs = prs[:limit]
	}

	payload := listPRsPayload(prs)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeListPRs(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// listPRsPayload projects the classified PRs onto the list_prs wire shape.
func listPRsPayload(prs []forge.PR) map[string]any {
	rows := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		st := forge.ClassifyStatus(pr, pr.BaseRef)
		blockers := st.Blockers
		if blockers == nil {
			blockers = []string{}
		}
		rows = append(rows, map[string]any{
			"number":   pr.Number,
			"title":    pr.Title,
			"author":   pr.Author,
			"age_days": st.AgeDays,
			"ci":       forge.RollupCI(pr),
			"review":   pr.ReviewDecision,
			"state":    st.State,
			"blockers": blockers,
		})
	}
	return map[string]any{
		"prs":   rows,
		"total": len(rows),
	}
}

// handleGetPRImpact maps a PR's changed files to symbols, scores PR-level
// risk, and groups the affected surface by community and caller/test file.
func (s *Server) handleGetPRImpact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	number := req.GetInt("number", 0)
	if number <= 0 {
		return mcp.NewToolResultError("number is required"), nil
	}
	repo := strings.TrimSpace(req.GetString("repo", ""))
	receipt := req.GetBool("receipt", false)
	scrub := req.GetBool("scrub", false)

	files, supplied, err := s.parseSuppliedFiles(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !supplied {
		fetched, degraded, ferr := s.fetchPRFiles(ctx, repo, number)
		if degraded != nil {
			return s.respondJSONOrTOON(ctx, req, degraded)
		}
		if ferr != nil {
			return mcp.NewToolResultError(ferr.Error()), nil
		}
		files = fetched
	}

	payload := s.prImpactForNumber(ctx, number, s.prJoinPrefix(ctx, repo), files, receipt, scrub)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodePRImpact(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// prImpactForNumber builds the get_pr_impact payload for one PR from its
// changed file set: file→symbol join, PR-risk score, and community / blast
// grouping. receipt adds a privacy-safe review receipt projected from the
// same risk result. It performs no network I/O — the files are supplied.
// repoPrefix anchors the file→symbol join in multi-repo mode (see
// prJoinPrefix).
func (s *Server) prImpactForNumber(ctx context.Context, number int, repoPrefix string, files []string, receipt, scrub bool) map[string]any {
	changedFiles, changedSymbolNodes := s.changedSymbolsForFiles(ctx, repoPrefix, files)
	symbolIDs := make([]string, 0, len(changedSymbolNodes))
	for _, n := range changedSymbolNodes {
		symbolIDs = append(symbolIDs, n.ID)
	}

	communities := s.getCommunities()
	var nodeToComm map[string]string
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}

	result := analysis.ScorePRRisk(s.readerFor(ctx), analysis.PRRiskInput{
		SymbolIDs:    symbolIDs,
		ChangedFiles: changedFiles,
		NodeToComm:   nodeToComm,
		Communities:  communities,
		Processes:    s.getProcesses(),
	})

	priorities := make([]map[string]any, 0, len(result.Factors))
	for _, f := range result.Factors {
		priorities = append(priorities, map[string]any{
			"axis":   f.Axis,
			"score":  f.Score,
			"reason": f.Reason,
		})
	}

	// Community grouping: the distinct communities the changed symbols span.
	commSet := map[string]bool{}
	for _, id := range symbolIDs {
		if cid, ok := nodeToComm[id]; ok && cid != "" {
			commSet[cid] = true
		}
	}
	commList := make([]string, 0, len(commSet))
	for c := range commSet {
		commList = append(commList, c)
	}
	sort.Strings(commList)

	changedFilesOut := append([]string(nil), changedFiles...)
	sort.Strings(changedFilesOut)

	changedSymbolsOut := make([]map[string]any, 0, len(changedSymbolNodes))
	for _, n := range changedSymbolNodes {
		changedSymbolsOut = append(changedSymbolsOut, map[string]any{
			"id":   n.ID,
			"name": n.Name,
			"kind": string(n.Kind),
			"file": n.FilePath,
		})
	}

	payload := map[string]any{
		"number":            number,
		"risk":              string(result.Risk),
		"score":             result.Score,
		"review_priorities": priorities,
		"changed_files":     changedFilesOut,
		"changed_symbols":   changedSymbolsOut,
		"communities":       commList,
		"blast":             s.buildBlastRadius(ctx, changedSymbolNodes),
	}

	if receipt {
		rec := analysis.BuildReviewReceipt(result, "", false, scrub)
		payload["receipt"] = rec
	}
	return payload
}

// changedSymbolsForFiles maps a set of changed file paths to the code
// symbols those files define, via the prefix-aware file→node join
// (whole-file granularity — the coarse mapping a not-checked-out PR
// allows). Forge APIs return repo-relative paths while multi-repo daemons
// key file paths as "<prefix>/<rel>"; repoPrefix bridges the two. Returns
// the deduped non-empty file list and the deduped symbol nodes (file nodes
// excluded), both deterministically ordered.
func (s *Server) changedSymbolsForFiles(ctx context.Context, repoPrefix string, files []string) ([]string, []*graph.Node) {
	reader := s.readerFor(ctx)
	fileSeen := map[string]bool{}
	var changedFiles []string
	nodeSeen := map[string]bool{}
	var nodes []*graph.Node
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || fileSeen[f] {
			continue
		}
		fileSeen[f] = true
		changedFiles = append(changedFiles, f)
		for _, n := range analysis.JoinFileNodes(reader, repoPrefix, f, analysis.RepoRelativePath) {
			if n == nil || n.Kind == graph.KindFile {
				continue
			}
			if nodeSeen[n.ID] {
				continue
			}
			nodeSeen[n.ID] = true
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return changedFiles, nodes
}

// prJoinPrefix resolves the graph repo prefix for the forge-file → symbol
// join via the shared diff-handler scope resolution: the caller's repo
// selector (prefix or path), the lone tracked repo, or the session's
// cwd-bound repo (the CLI dials the daemon with its working directory, so
// `gortex prs <N>` run inside a tracked repo joins against that repo
// without an explicit selector). Empty in single-repo / unprefixed mode.
func (s *Server) prJoinPrefix(ctx context.Context, repo string) string {
	_, prefix := s.diffRepoScope(ctx, repo)
	return prefix
}

// handleTriagePRs ranks a repository's open PRs by get_pr_impact score
// (highest first, deterministic). The PR list and per-PR files come from
// supplied maps or a self-served forge fan-out.
func (s *Server) handleTriagePRs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	repo := strings.TrimSpace(req.GetString("repo", ""))
	limit := req.GetInt("limit", 20)
	if limit < 1 {
		limit = 20
	}

	prs, prsSupplied, err := s.parseSuppliedPRs(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	filesByNumber, err := s.parseSuppliedFilesByNumber(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !prsSupplied {
		if !forge.Available(ctx) && len(filesByNumber) == 0 {
			return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
		}
		repoRoot, _ := s.diffRepoScope(ctx, repo)
		fetched, ferr := forgeList(ctx, repoRoot, forge.ListOpts{State: "open", Limit: limit, WithCI: true})
		if ferr != nil {
			if errors.Is(ferr, forge.ErrRateLimited) {
				return s.respondJSONOrTOON(ctx, req, rateLimitedPayload(ferr))
			}
			if isForgeUnavailable(ferr) {
				return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
			}
			return mcp.NewToolResultError(fmt.Sprintf("listing PRs failed: %v", ferr)), nil
		}
		prs = fetched
		for _, pr := range prs {
			s.prCache.putList(repo, pr.Number, pr)
		}
	}

	if limit > 0 && len(prs) > limit {
		prs = prs[:limit]
	}

	joinPrefix := s.prJoinPrefix(ctx, repo)
	ranked := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		files, degraded, ferr := s.resolvePRFiles(ctx, repo, pr, filesByNumber)
		if degraded != nil {
			return s.respondJSONOrTOON(ctx, req, degraded)
		}
		if ferr != nil {
			return mcp.NewToolResultError(ferr.Error()), nil
		}
		impact := s.prImpactForNumber(ctx, pr.Number, joinPrefix, files, false, false)
		ranked = append(ranked, map[string]any{
			"number": pr.Number,
			"title":  pr.Title,
			"author": pr.Author,
			"risk":   impact["risk"],
			"score":  impact["score"],
		})
	}

	// Deterministic: score descending, PR number ascending on a tie. This
	// always runs first so the deterministic order is the baseline the LLM
	// re-rank refines and the stable fallback tail when the model omits or
	// mangles a PR.
	sort.SliceStable(ranked, func(i, j int) bool {
		si, _ := ranked[i]["score"].(float64)
		sj, _ := ranked[j]["score"].(float64)
		if si != sj {
			return si > sj
		}
		ni, _ := ranked[i]["number"].(int)
		nj, _ := ranked[j]["number"].(int)
		return ni < nj
	})

	// Opt-in LLM re-rank: reorder the deterministic queue and attach a
	// per-PR rationale when a provider is configured. A disabled service
	// or an unparseable response leaves the deterministic order untouched
	// and stamps llm_used:false — never an error.
	llmUsed := false
	if req.GetBool("use_llm", false) {
		ranked, llmUsed = llmRankPRs(ctx, s.llmService, ranked, req.GetInt("max_tokens", 512))
	}

	payload := map[string]any{
		"ranked":   ranked,
		"total":    len(ranked),
		"llm_used": llmUsed,
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeTriagePRs(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// llmRankPRs re-ranks an already-deterministically-ordered triage list
// with a single LLM pass. It builds one compact prompt from the per-PR
// rows, calls the service via the llmRerank seam, parses the model's
// ordered PR numbers + rationale, and reorders the rows accordingly,
// stamping a "rationale" field on each row the model annotated. It is
// total: a disabled service, a Generate error, or an unparseable response
// returns the input order unchanged with used=false. The returned slice
// is a fresh permutation of the input rows (the input is not mutated in
// place beyond the rationale stamp on shared row maps).
func llmRankPRs(ctx context.Context, service *svc.Service, ranked []map[string]any, maxTokens int) (out []map[string]any, used bool) {
	if len(ranked) == 0 {
		return ranked, false
	}
	prompt := buildTriagePrompt(ranked)
	text, usable, err := llmRerank(ctx, service, prompt, maxTokens)
	if !usable || err != nil || strings.TrimSpace(text) == "" {
		return ranked, false
	}

	order, rationales := parseTriageRanking(text)
	if len(order) == 0 {
		return ranked, false
	}

	// Index the deterministic rows by PR number so the parsed order can
	// pull them in the model's sequence; numbers the model omitted or
	// invented are handled by appending the unseen deterministic tail.
	byNumber := make(map[int]map[string]any, len(ranked))
	for _, r := range ranked {
		if n, ok := rowNumber(r); ok {
			byNumber[n] = r
		}
	}

	reordered := make([]map[string]any, 0, len(ranked))
	placed := make(map[int]bool, len(ranked))
	for _, n := range order {
		row, ok := byNumber[n]
		if !ok || placed[n] {
			// A number the model invented or repeated — skip it.
			continue
		}
		if rat := strings.TrimSpace(rationales[n]); rat != "" {
			row["rationale"] = rat
		}
		reordered = append(reordered, row)
		placed[n] = true
	}
	// Preserve the deterministic tail for any PR the model dropped, in the
	// original (score-descending) order, so the queue stays complete.
	for _, r := range ranked {
		n, ok := rowNumber(r)
		if !ok || placed[n] {
			continue
		}
		reordered = append(reordered, r)
		placed[n] = true
	}

	if len(reordered) != len(ranked) {
		// Defensive: a row without a usable number would be lost. Fall
		// back to the deterministic order rather than truncate the queue.
		return ranked, false
	}
	return reordered, true
}

// rowNumber extracts the PR number from a triage row, tolerating both the
// int form (built in-process) and the float64 form (a JSON round-trip).
func rowNumber(row map[string]any) (int, bool) {
	switch v := row["number"].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

// buildTriagePrompt renders one compact, deterministic prompt from the
// per-PR triage rows: number, title, author, risk, and deterministic
// score. It is pure — same rows in, same prompt out — so the prompt is
// stable across runs and trivially testable. The model is asked to return
// one "PR <n>: <rationale>" line per PR, most-important first.
func buildTriagePrompt(ranked []map[string]any) string {
	var b strings.Builder
	b.WriteString("You are triaging a code-review queue. Below are open pull requests, ")
	b.WriteString("each with a graph-derived risk tier and score (higher score = more blast radius). ")
	b.WriteString("Rank them from most to least important to review first, considering risk, ")
	b.WriteString("score, and title. Reply with one line per PR in your chosen order, formatted ")
	b.WriteString("exactly as `PR <number>: <one-sentence rationale>`. Include every PR exactly once.\n\n")
	for _, r := range ranked {
		n, _ := rowNumber(r)
		title, _ := r["title"].(string)
		author, _ := r["author"].(string)
		risk, _ := r["risk"].(string)
		score, _ := r["score"].(float64)
		fmt.Fprintf(&b, "PR %d | risk=%s score=%.1f | %s",
			n, strings.TrimSpace(risk), score, strings.TrimSpace(title))
		if a := strings.TrimSpace(author); a != "" {
			b.WriteString(" | by " + a)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// parseTriageRanking parses an LLM ranking response into an ordered list
// of PR numbers and a per-number rationale. It is pure and forgiving: it
// scans line by line for the first integer following an optional `PR`/`#`
// token, treats the remainder of the line (after a `:` or `-` separator)
// as that PR's rationale, ignores lines with no number, and drops a
// repeated number's later occurrences. A response with no parseable
// numbers yields an empty order, signalling the caller to fall back.
func parseTriageRanking(text string) (order []int, rationales map[int]string) {
	rationales = map[int]string{}
	seen := map[int]bool{}
	for _, line := range strings.Split(text, "\n") {
		n, rationale, ok := parseRankLine(line)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		order = append(order, n)
		if rationale != "" {
			rationales[n] = rationale
		}
	}
	return order, rationales
}

// parseRankLine pulls the leading PR number and trailing rationale out of
// one ranking line. It tolerates `PR 12: reason`, `#12 - reason`, `12.
// reason`, `1) PR 12 reason`, and bare `12`. Returns ok=false when the
// line carries no PR number.
func parseRankLine(line string) (number int, rationale string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" {
		return 0, "", false
	}
	// Strip a leading ordinal like "1." / "1)" so it is not mistaken for
	// the PR number — but only when a `PR`/`#` token follows, which marks
	// the real number.
	if lower := strings.ToLower(s); strings.Contains(lower, "pr ") || strings.Contains(lower, "pr#") || strings.Contains(s, "#") {
		s = stripLeadingOrdinal(s)
	}
	// Advance to the first digit, skipping an optional `PR` / `#` marker.
	i := 0
	for i < len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			break
		}
		i++
	}
	if i >= len(s) {
		return 0, "", false
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	num, err := strconv.Atoi(s[i:j])
	if err != nil {
		return 0, "", false
	}
	rest := strings.TrimSpace(s[j:])
	rest = strings.TrimLeft(rest, ":-—–.) \t")
	return num, strings.TrimSpace(rest), true
}

// stripLeadingOrdinal removes a leading "N." or "N)" list marker so the
// real PR number (which follows a PR/# token) is the one parsed.
func stripLeadingOrdinal(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(s) {
		return s
	}
	if s[i] == '.' || s[i] == ')' {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// resolvePRFiles returns the changed-file set for one PR for the triage /
// conflicts fan-out, preferring already-available data over a forge call:
//
//  1. caller-supplied files (the filesByNumber map) win outright;
//  2. the PR's own hydrated Files (a supplied `prs` array may carry them);
//  3. a cached forge.PR with hydrated Files within the TTL — this is what
//     lets a triage / conflicts re-run within the window skip the refetch;
//  4. otherwise a self-served forge fetch, whose result is stamped into the
//     cache (as a hydrated PR) so the NEXT run hits step 3.
//
// degraded is non-nil only when a forge-degradation payload should be
// returned to the caller; err is non-nil only for an unexpected failure.
func (s *Server) resolvePRFiles(ctx context.Context, repo string, pr forge.PR, filesByNumber map[int][]string) (files []string, degraded map[string]any, err error) {
	if supplied, ok := filesByNumber[pr.Number]; ok {
		return supplied, nil, nil
	}
	if len(pr.Files) > 0 {
		return pr.Files, nil, nil
	}
	// Cache hit: a prior fan-out stamped this PR's files within the TTL.
	if cached, ok := s.prCache.get(repo, pr.Number); ok && len(cached.Files) > 0 {
		return cached.Files, nil, nil
	}
	fetched, degraded, ferr := s.fetchPRFiles(ctx, repo, pr.Number)
	if degraded != nil || ferr != nil {
		return nil, degraded, ferr
	}
	// Stamp the hydrated PR so a subsequent run hits the cache above and
	// avoids this forge call. Preserve the PR's other fields.
	hydrated := pr
	hydrated.Files = fetched
	s.prCache.put(repo, pr.Number, hydrated)
	return fetched, nil, nil
}

// fetchPRFiles resolves the changed-file set for one PR via the forge,
// honoring the no-token and rate-limit degradations. The returned map is
// non-nil only when a degradation payload should be returned to the
// caller; the error is non-nil only for an unexpected failure.
func (s *Server) fetchPRFiles(ctx context.Context, repo string, number int) (files []string, degraded map[string]any, err error) {
	if !forge.Available(ctx) {
		return nil, forgeUnavailablePayload(), nil
	}
	repoRoot, _ := s.diffRepoScope(ctx, repo)
	fetched, ferr := forgeFiles(ctx, repoRoot, number)
	if ferr != nil {
		if errors.Is(ferr, forge.ErrRateLimited) {
			return nil, rateLimitedPayload(ferr), nil
		}
		if isForgeUnavailable(ferr) {
			return nil, forgeUnavailablePayload(), nil
		}
		return nil, nil, fmt.Errorf("fetching files for PR #%d failed: %v", number, ferr)
	}
	return fetched, nil, nil
}

// parseSuppliedPRs reads the optional caller-supplied `prs` JSON array of
// forge.PR objects. supplied reports whether the caller provided the arg
// at all (an empty-but-present array still counts, so the tool skips the
// network as the caller asked). A malformed value is an error.
func (s *Server) parseSuppliedPRs(req mcp.CallToolRequest) (prs []forge.PR, supplied bool, err error) {
	raw := strings.TrimSpace(req.GetString("prs", ""))
	if raw == "" {
		return nil, false, nil
	}
	if uerr := json.Unmarshal([]byte(raw), &prs); uerr != nil {
		return nil, false, fmt.Errorf("invalid prs JSON: %v", uerr)
	}
	return prs, true, nil
}

// parseSuppliedFiles reads the optional caller-supplied `files` JSON array
// of changed file paths. supplied reports whether the caller provided the
// arg, so the tool can skip the forge fetch even for an empty list.
func (s *Server) parseSuppliedFiles(req mcp.CallToolRequest) (files []string, supplied bool, err error) {
	raw := strings.TrimSpace(req.GetString("files", ""))
	if raw == "" {
		return nil, false, nil
	}
	if uerr := json.Unmarshal([]byte(raw), &files); uerr != nil {
		return nil, false, fmt.Errorf("invalid files JSON: %v", uerr)
	}
	return files, true, nil
}

// parseSuppliedFilesByNumber reads the optional caller-supplied `files`
// JSON object mapping a PR number (string key) to its changed file paths,
// used by triage_prs to skip a per-PR file fetch. An absent arg yields an
// empty map; a malformed value is an error.
func (s *Server) parseSuppliedFilesByNumber(req mcp.CallToolRequest) (map[int][]string, error) {
	raw := strings.TrimSpace(req.GetString("files", ""))
	if raw == "" {
		return map[int][]string{}, nil
	}
	var stringKeyed map[string][]string
	if uerr := json.Unmarshal([]byte(raw), &stringKeyed); uerr != nil {
		return nil, fmt.Errorf("invalid files JSON (want an object mapping PR number to file paths): %v", uerr)
	}
	out := make(map[int][]string, len(stringKeyed))
	for k, v := range stringKeyed {
		n, cerr := strconv.Atoi(strings.TrimSpace(k))
		if cerr != nil {
			return nil, fmt.Errorf("invalid PR number key %q in files map: %v", k, cerr)
		}
		out[n] = v
	}
	return out, nil
}
