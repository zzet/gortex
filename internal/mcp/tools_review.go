package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/review"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/testpath"
)

// registerReviewTools registers the review-engine tool group. Unlike most
// specialised tool groups, these tools are EAGER — their names live in
// hotEagerTools so they are published in the initial tools/list rather than
// hidden behind tools_search. A reviewing agent reaches for them on the first
// turn of a review task, so paying a discovery round-trip for them would be a
// regression. This group grows: it is the single registration site for the
// whole review surface, so later review tools append their addTool block here.
func (s *Server) registerReviewTools() {
	s.addTool(
		mcp.NewTool("sibling_diff_context",
			mcp.WithDescription("Return the raw unified diff of the OTHER changed files in a changeset — the sibling changes a per-symbol or per-file review view filters out — prebuilt in one call. Enumerates the whole changeset (via the git diff against `base`/`scope`), drops the focus files, and returns each remaining file's raw diff ranked by relatedness to the focus (shared community/process → co-change → directory proximity). Pass `focus_files` (comma-separated changed file paths to exclude) and/or `focus_symbol_id` (a changed symbol whose file is the focus). Use to pull in the cross-file context a narrow review needs without issuing a diff call per file."),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Selects the changeset as `git diff base...HEAD`. Alias for scope=compare + base_ref=base.")),
			mcp.WithString("base_ref", mcp.Description("Base ref for scope=compare (default: main). `base` takes precedence when both are set.")),
			mcp.WithString("scope", mcp.Description("Changeset scope: unstaged (default), staged, all, or compare. Ignored when `base` is set (forces compare).")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithString("focus_files", mcp.Description("Comma-separated changed file paths that are the focus — excluded from the returned siblings.")),
			mcp.WithString("focus_file", mcp.Description("Single focus file path — excluded from the siblings (alias for focus_files with one entry).")),
			mcp.WithString("focus_symbol_id", mcp.Description("A changed symbol's ID; its file becomes a focus file and is excluded from the siblings.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The lowest-ranked siblings are trimmed first; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description("Token budget for the response — the lowest-ranked siblings are dropped first to fit.")),
		),
		s.handleSiblingDiffContext,
	)

	s.addTool(
		mcp.NewTool("review",
			mcp.WithDescription("Review a changeset and return line-anchored inline review comments plus a BLOCK/REVIEW/APPROVE verdict. Enumerates the changeset (git diff against `base`/`scope`, or a pasted unified `diff`), runs the deterministic correctness review rulepack over the changed files (graph-grounded to drop false positives), and — when `use_llm` is set and an LLM provider is configured — folds in LLM-found findings relocated to exact lines. Each finding is anchored to a `{file,line,severity,message,rule,category}` so it can be posted as an inline comment. Returns the verdict envelope (verdict + summary + per-file risk + the line-anchored comments)."),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Selects the changeset as `git diff base...HEAD`. Alias for scope=compare + base_ref=base.")),
			mcp.WithString("base_ref", mcp.Description("Base ref for scope=compare (default: main). `base` takes precedence when both are set.")),
			mcp.WithString("scope", mcp.Description("Changeset scope: unstaged (default), staged, all, or compare. Ignored when `base` or `diff` is set.")),
			mcp.WithString("diff", mcp.Description("Raw unified-diff text to review off-disk (the pasted-diff path). When set, no git command runs and `scope`/`base` are ignored.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithBoolean("use_llm", mcp.Description("Engage the LLM review phase (graph-grounded rulepack findings always run). Requires a configured LLM provider; ignored when none is available. Default: false.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list (comments) is trimmed first; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description("Token budget for the response and the internal review pack handed to the LLM.")),
		),
		s.handleReview,
	)

	s.addTool(
		mcp.NewTool("review_pack",
			mcp.WithDescription("Run the whole PR-review gate set over a changeset and fold the result into ONE packaged envelope. Composes the deterministic graph-grounded review (verdict + line-anchored findings), per-symbol semantic classification (feature/fix/refactor/test/config), per-file risk ranking, contract-impact + guard/architecture checks, and the impacted test targets — then derives a concrete `verification_command` to run and a privacy-safe risk receipt. The verdict is the worst-of the review report, upgraded to BLOCK when a contract breaks or a guard/architecture rule is violated. Pass `base`/`scope` to select the changeset (or a pasted `diff`); `include_pack` adds the tiered diff-hunk pack; `scrub` strips any path/symbol/email from the receipt. Use as the single entrypoint for an AST-grounded PR review."),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Selects the changeset as `git diff base...HEAD`. Alias for scope=compare + base_ref=base.")),
			mcp.WithString("base_ref", mcp.Description("Base ref for scope=compare (default: main). `base` takes precedence when both are set.")),
			mcp.WithString("scope", mcp.Description("Changeset scope: unstaged (default), staged, all, or compare. Ignored when `base` or `diff` is set.")),
			mcp.WithString("diff", mcp.Description("Raw unified-diff text to review off-disk (the pasted-diff path). When set, no git command runs and `scope`/`base` are ignored. Contract/guard/test gates that need indexed symbols are skipped for a pasted diff.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithBoolean("use_llm", mcp.Description("Engage the LLM review phase (graph-grounded rulepack findings always run). Requires a configured LLM provider; ignored when none is available. Default: false.")),
			mcp.WithBoolean("include_pack", mcp.Description("Include the tiered review pack (changed symbols as diff hunks, direct callers as full source, the rest as an outline). Default: false.")),
			mcp.WithBoolean("scrub", mcp.Description("Sanitize the risk receipt so no path-like, symbol-ID-like, or email-like value can leak — safe to share cross-org. Default: false.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed first; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description("Token budget for the response and the internal review pack handed to the LLM.")),
		),
		s.handleReviewPack,
	)

	s.addTool(
		mcp.NewTool("suppress_finding",
			mcp.WithDescription("Durably silence a review finding as a false positive — or list / un-suppress existing suppressions — for the current repository. A suppressed finding is identified by a stable key over its rule, category, symbol, file, and (when supplied) the flagged line's source text, so it stays suppressed after the file shifts the finding to a different line. Every subsequent `review` / `review_pack` run drops a suppressed finding (counted in the gate's `identity_suppressed` stat). This is a permanent, per-repo never-flag-again list (sidecar-backed, survives restarts) — distinct from a development memory or a feedback signal. Pass `action:add` with the finding's `identity_key` (preferred) or the `rule`/`category`/`symbol_id`/`file`/`source_line` fields to derive it; `action:list` to see what is suppressed; `action:remove` with an `identity_key` to un-suppress."),
			mcp.WithString("action", mcp.Description("add (default), list, or remove.")),
			mcp.WithString("identity_key", mcp.Description("The finding's stable identity key (from a prior review's gate or a list call). Required for remove; preferred for add. When absent on add, the key is derived from rule/category/symbol_id/file/source_line.")),
			mcp.WithString("rule", mcp.Description("Detector / rule name of the finding (used to derive identity_key on add).")),
			mcp.WithString("category", mcp.Description("Finding category (used to derive identity_key on add).")),
			mcp.WithString("symbol_id", mcp.Description("The flagged symbol's ID (used to derive identity_key on add).")),
			mcp.WithString("file", mcp.Description("The flagged file path (used to derive identity_key on add).")),
			mcp.WithNumber("line", mcp.Description("The flagged line number. Recorded for context; NOT part of the identity, so the suppression survives line drift.")),
			mcp.WithString("source_line", mcp.Description("The flagged line's source text. Folded (trimmed) into the identity so the suppression is drift-stable and does not over-suppress sibling findings on the same symbol.")),
			mcp.WithString("reason", mcp.Description("Why this finding is a false positive (stored alongside the suppression).")),
			mcp.WithString("author", mcp.Description("Who suppressed it (stored alongside the suppression).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes (list action). Omit for no cap.")),
		),
		s.handleSuppressFinding,
	)

	s.addTool(
		mcp.NewTool("post_review",
			mcp.WithDescription("Post review findings as inline comments on a GitHub PR / GitLab MR. Each finding is mapped to a RIGHT-side (new-code) inline comment anchored to its file + line (multi-line findings carry a start_line < line range), batched into one review. Every comment body is run through a secret-redaction pass BEFORE any payload is built or any request is sent — a body that quotes an inline credential (API key, token, PEM block, password assignment) is redacted (or, by default, the whole finding is skipped) so a secret never egresses. Posting to a public or fork PR is opt-in: pass confirm_public:true (or set review.post.allow_public) or the post is refused. Pass findings as a JSON array (the gated findings from a prior `review` / `review_pack` call); when omitted the deterministic review rulepack runs over the changeset / pasted diff. dry_run:true returns the would-post (already-redacted) payloads without any network call."),
			mcp.WithNumber("number", mcp.Required(), mcp.Description("The PR / MR number to post comments on.")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree + token (multi-repo mode).")),
			mcp.WithString("findings", mcp.Description("JSON array of review findings to post (from a prior review / review_pack call). When omitted, the review rulepack runs over the changeset to produce findings.")),
			mcp.WithString("diff", mcp.Description("Raw unified-diff text to review off-disk when deriving findings (the pasted-diff path). Ignored when `findings` is supplied.")),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main) selecting the changeset when deriving findings. Alias for scope=compare + base_ref=base.")),
			mcp.WithString("base_ref", mcp.Description("Base ref for scope=compare (default: main) when deriving findings.")),
			mcp.WithString("scope", mcp.Description("Changeset scope when deriving findings: unstaged (default), staged, all, or compare.")),
			mcp.WithString("summary", mcp.Description("Top-level review summary body posted alongside the inline comments.")),
			mcp.WithString("owner", mcp.Description("Repository owner (for the posted review URL).")),
			mcp.WithString("repo_name", mcp.Description("Repository name (for the posted review URL).")),
			mcp.WithString("provider", mcp.Description("Forge backend: github (default).")),
			mcp.WithBoolean("public", mcp.Description("The target PR is on a public or fork repo (world-readable). When true, posting requires confirm_public / review.post.allow_public. Default: false.")),
			mcp.WithBoolean("confirm_public", mcp.Description("Confirm posting to a public / fork PR (world-readable comments). Default: false — without it a public target is refused.")),
			mcp.WithBoolean("refuse_on_secret", mcp.Description("Skip a finding whose body still quoted a secret rather than posting a redacted version. Default: true.")),
			mcp.WithBoolean("dry_run", mcp.Description("Build and return the would-post (already-redacted) payloads without any network call. Default: false.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. Omit for no cap.")),
		),
		s.handlePostReview,
	)
}

// reviewSuppressions returns the active repo's durable suppression store and its
// per-repo key for the review gate. Both are zero-valued (nil store, empty key)
// when InitSuppressions has not run — the review flow tolerates that and
// suppresses nothing.
func (s *Server) reviewSuppressions() (*review.SuppressionStore, string) {
	if s.suppressions == nil {
		return nil, ""
	}
	return s.suppressions.Store(), s.suppressions.RepoKey()
}

// handleSuppressFinding records, lists, or removes a durable false-positive
// suppression for the current repository. The add action derives the finding's
// stable identity key from the supplied fields (or takes an explicit
// identity_key); list returns every suppression most-recently-hit first; remove
// deletes one by identity key. The suppression is honoured by every subsequent
// review / review_pack run via the gate.
func (s *Server) handleSuppressFinding(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.suppressions == nil {
		return mcp.NewToolResultError("suppression store not initialised"), nil
	}
	store := s.suppressions.Store()
	repoKey := s.suppressions.RepoKey()

	action := strings.ToLower(strings.TrimSpace(req.GetString("action", "add")))
	switch action {
	case "list":
		rows, err := store.List(repoKey)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list suppressions: %v", err)), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"suppressions": rows,
			"total":        len(rows),
		})

	case "remove", "delete", "unsuppress":
		key := strings.TrimSpace(req.GetString("identity_key", ""))
		if key == "" {
			return mcp.NewToolResultError("suppress_finding remove requires identity_key"), nil
		}
		if err := store.Unsuppress(repoKey, key); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("remove suppression: %v", err)), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":       "removed",
			"identity_key": key,
		})

	case "add", "suppress", "":
		f := review.Finding{
			IdentityKey: strings.TrimSpace(req.GetString("identity_key", "")),
			Rule:        strings.TrimSpace(req.GetString("rule", "")),
			Category:    strings.TrimSpace(req.GetString("category", "")),
			SymbolID:    strings.TrimSpace(req.GetString("symbol_id", "")),
			File:        strings.TrimSpace(req.GetString("file", "")),
			Line:        req.GetInt("line", 0),
			SourceLine:  req.GetString("source_line", ""),
		}
		// Require enough to derive a meaningful identity: either an explicit
		// key, or at least a rule plus a symbol/file to anchor it.
		if f.IdentityKey == "" && f.Rule == "" && f.SymbolID == "" && f.File == "" {
			return mcp.NewToolResultError("suppress_finding add requires identity_key, or at least rule + symbol_id/file to derive one"), nil
		}
		if f.IdentityKey == "" {
			f.IdentityKey = review.IdentityKey(f)
		}
		reason := strings.TrimSpace(req.GetString("reason", ""))
		author := strings.TrimSpace(req.GetString("author", ""))
		if err := store.Suppress(repoKey, f, reason, author); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("suppress finding: %v", err)), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":       "suppressed",
			"identity_key": f.IdentityKey,
			"rule":         f.Rule,
			"category":     f.Category,
			"symbol_id":    f.SymbolID,
			"file":         f.File,
		})

	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown action %q (want add, list, or remove)", action)), nil
	}
}

// inlineComment is one line-anchored review finding projected onto the inline
// review-comment shape: the file + new-side line it anchors to, its severity,
// the short message, the rule/detector that produced it, and its category. It is
// the unit a reviewing agent (or a forge poster one layer up) attaches to a PR.
type inlineComment struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Rule     string `json:"rule"`
	Category string `json:"category"`
	Source   string `json:"source,omitempty"`
}

// siblingDiffRow is one related-but-filtered-out changed file: its repo-relative
// path, the relation that ranks it against the focus, the relatedness score, and
// the raw unified diff text of just that file's hunks.
type siblingDiffRow struct {
	File     string  `json:"file"`
	Relation string  `json:"relation"`
	Score    float64 `json:"score"`
	Diff     string  `json:"diff"`
}

// handleSiblingDiffContext enumerates the changeset, drops the focus files, and
// returns each remaining changed file's raw diff ranked by relatedness to the
// focus. Relatedness is community/process sharing → co-change → directory
// proximity; budget trims the lowest-ranked rows first.
func (s *Server) handleSiblingDiffContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	scope, baseRef := siblingDiffScope(req)

	repo := strings.TrimSpace(req.GetString("repo", ""))
	repoRoot, repoPrefix := s.diffRepoScope(ctx, repo)
	if repoRoot == "" {
		return mcp.NewToolResultError("could not resolve a repository root for the changeset diff"), nil
	}

	// Enumerate the whole changeset.
	diff, err := analysis.MapGitDiff(s.graph, repoRoot, repoPrefix, scope, baseRef)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Resolve the focus set: explicit focus_files / focus_file plus the file
	// of any focus_symbol_id.
	focus := s.resolveFocusFiles(req)
	focusList := make([]string, 0, len(focus))
	for f := range focus {
		focusList = append(focusList, f)
	}
	sort.Strings(focusList)

	// Build a deduplicated, focus-excluded sibling set out of the changed files.
	seen := map[string]bool{}
	var siblings []siblingDiffRow
	for _, f := range diff.ChangedFiles {
		f = filepath.Clean(f)
		if f == "" || f == "." || focus[f] || seen[f] {
			continue
		}
		seen[f] = true

		raw, derr := s.rawFileDiff(ctx, repoRoot, scope, baseRef, f)
		if derr != nil || strings.TrimSpace(raw) == "" {
			continue
		}
		relation, score := s.siblingRelation(f, focusList)
		siblings = append(siblings, siblingDiffRow{
			File:     f,
			Relation: relation,
			Score:    score,
			Diff:     raw,
		})
	}

	// Rank: highest relatedness first, ties broken by path for determinism.
	sort.SliceStable(siblings, func(i, j int) bool {
		if siblings[i].Score != siblings[j].Score {
			return siblings[i].Score > siblings[j].Score
		}
		return siblings[i].File < siblings[j].File
	})

	payload := siblingDiffPayload(focusList, siblings)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeSiblingDiffContext(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// siblingDiffScope resolves the (scope, baseRef) pair from the request. `base`
// is a convenience alias that forces compare scope against that ref.
func siblingDiffScope(req mcp.CallToolRequest) (scope, baseRef string) {
	base := strings.TrimSpace(req.GetString("base", ""))
	if base != "" {
		return "compare", base
	}
	scope = req.GetString("scope", "unstaged")
	baseRef = req.GetString("base_ref", "main")
	return scope, baseRef
}

// resolveFocusFiles collects the focus file set from focus_files / focus_file
// (paths) and focus_symbol_id (the symbol's file). Paths are cleaned so they
// join the MapGitDiff ChangedFiles keys.
func (s *Server) resolveFocusFiles(req mcp.CallToolRequest) map[string]bool {
	focus := map[string]bool{}
	add := func(p string) {
		p = filepath.Clean(strings.TrimSpace(p))
		if p != "" && p != "." {
			focus[p] = true
		}
	}
	for _, p := range strings.Split(req.GetString("focus_files", ""), ",") {
		add(p)
	}
	if ff := req.GetString("focus_file", ""); ff != "" {
		add(ff)
	}
	if id := strings.TrimSpace(req.GetString("focus_symbol_id", "")); id != "" {
		if n := s.graph.GetNode(id); n != nil && n.FilePath != "" {
			add(n.FilePath)
		}
	}
	return focus
}

// siblingRelation classifies and scores how a candidate sibling file relates to
// the focus set. The strongest applicable relation wins:
//
//	community → the sibling shares a graph community with a focus symbol
//	process   → the sibling shares a process with a focus symbol
//	cochange  → the sibling historically changes alongside a focus file
//	directory → the sibling lives in (or near) a focus file's directory
//	none      → unrelated by any signal
//
// Score is a coarse band per relation (plus a co-change magnitude bump) so the
// ranking is deterministic and the bands stay separable.
func (s *Server) siblingRelation(file string, focusFiles []string) (string, float64) {
	if len(focusFiles) == 0 {
		return "none", 0
	}

	// community / process sharing, evaluated symbol-wise across both sides.
	communities := s.getCommunities()
	processes := s.getProcesses()
	focusCommunities := map[string]bool{}
	focusProcesses := map[string]bool{}
	for _, ff := range focusFiles {
		for _, n := range s.graph.GetFileNodes(ff) {
			if communities != nil {
				if c, ok := communities.NodeToComm[n.ID]; ok && c != "" {
					focusCommunities[c] = true
				}
			}
			if processes != nil {
				for _, p := range processes.NodeToProcs[n.ID] {
					if p != "" {
						focusProcesses[p] = true
					}
				}
			}
		}
	}
	if communities != nil && len(focusCommunities) > 0 {
		for _, n := range s.graph.GetFileNodes(file) {
			if c, ok := communities.NodeToComm[n.ID]; ok && focusCommunities[c] {
				return "community", 100
			}
		}
	}
	if processes != nil && len(focusProcesses) > 0 {
		for _, n := range s.graph.GetFileNodes(file) {
			for _, p := range processes.NodeToProcs[n.ID] {
				if focusProcesses[p] {
					return "process", 80
				}
			}
		}
	}

	// co-change: historical commit overlap between the sibling and a focus file.
	bestCo := 0.0
	for _, ff := range focusFiles {
		if sc, ok := s.coChangeScores(ff)[file]; ok && sc > bestCo {
			bestCo = sc
		}
	}
	if bestCo > 0 {
		// Keep co-change strictly below process so the bands don't collide.
		return "cochange", 40 + clampScore(bestCo, 39)
	}

	// directory proximity: same directory (or an ancestor of one) as a focus
	// file. The deeper the shared prefix the higher the score.
	bestDir := 0.0
	for _, ff := range focusFiles {
		if d := dirProximity(file, ff); d > bestDir {
			bestDir = d
		}
	}
	if bestDir > 0 {
		return "directory", bestDir
	}

	return "none", 0
}

// dirProximity scores directory closeness in (0,20]. Same directory scores
// highest; a shared parent directory scores by the number of shared leading
// path segments. Returns 0 when the files share no directory prefix.
func dirProximity(a, b string) float64 {
	da := strings.Split(filepath.ToSlash(filepath.Dir(a)), "/")
	db := strings.Split(filepath.ToSlash(filepath.Dir(b)), "/")
	shared := 0
	for shared < len(da) && shared < len(db) && da[shared] == db[shared] {
		if da[shared] == "." || da[shared] == "" {
			break
		}
		shared++
	}
	if shared == 0 {
		return 0
	}
	if len(da) == len(db) && shared == len(da) {
		// Exact same directory.
		return 20
	}
	score := float64(shared)
	if score > 19 {
		score = 19
	}
	return score
}

// clampScore caps v into [0, max].
func clampScore(v, max float64) float64 {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// rawFileDiff returns the raw unified diff text (context-bearing) for a single
// changed file within the changeset. It runs the same git-diff selection as
// MapGitDiff narrowed to one pathspec, so the per-file diff joins the enumerated
// changeset exactly.
func (s *Server) rawFileDiff(ctx context.Context, repoRoot, scope, baseRef, file string) (string, error) {
	args := siblingDiffArgs(scope, baseRef)
	args = append(args, "--", file)
	// Route through gitcmd (the concurrency-gated chokepoint). Use the handler
	// ctx so the subprocess is cancelled on session teardown; if it carries no
	// deadline, bound it at 30s. Run returns raw stdout (no trailing trim) so
	// the parsed diff is byte-identical to the pre-gitcmd output.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	out, err := gitcmd.Run(ctx, repoRoot, args...)
	if err != nil {
		// An empty diff for a path is not an error (e.g. mode-only change).
		if len(out) == 0 {
			return "", nil
		}
		return "", fmt.Errorf("git diff for %s failed: %w", file, err)
	}
	return string(out), nil
}

// siblingDiffArgs mirrors the analysis diff-arg selection but emits a context
// window (unified=3) so the raw sibling diff carries readable surrounding lines.
func siblingDiffArgs(scope, baseRef string) []string {
	// analysis.GitDiffArgs pins the a/ b/ header prefixes the diff parsers
	// anchor on (diff.mnemonicPrefix / diff.noprefix would zero them out).
	return analysis.GitDiffArgs(scope, baseRef, 3)
}

// siblingDiffPayload projects the ranked siblings onto the wire shape.
// truncated is always false here — the byte/token budget applied downstream
// stamps its own truncation flag when it trims rows.
func siblingDiffPayload(focus []string, siblings []siblingDiffRow) map[string]any {
	if focus == nil {
		focus = []string{}
	}
	rows := make([]map[string]any, 0, len(siblings))
	for _, sib := range siblings {
		rows = append(rows, map[string]any{
			"file":     sib.File,
			"relation": sib.Relation,
			"score":    sib.Score,
			"diff":     sib.Diff,
		})
	}
	return map[string]any{
		"focus":     focus,
		"siblings":  rows,
		"total":     len(rows),
		"truncated": false,
	}
}

// handleReview enumerates a changeset, runs the graph-grounded review rulepack
// over the changed files, optionally folds in LLM findings, and returns the
// resulting ReviewReport projected onto line-anchored inline comments plus the
// verdict envelope.
func (s *Server) handleReview(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	diffText := strings.TrimSpace(req.GetString("diff", ""))
	scope, baseRef := siblingDiffScope(req)

	repo := strings.TrimSpace(req.GetString("repo", ""))
	repoRoot, repoPrefix := s.diffRepoScope(ctx, repo)
	// An on-disk review needs a working tree; a pasted-diff review does not.
	if repoRoot == "" && diffText == "" {
		return mcp.NewToolResultError("could not resolve a repository root for the changeset diff"), nil
	}

	// Compute the deterministic rulepack matches over the changed files, and the
	// per-changed-symbol impact map, from the on-disk changeset. For a pasted
	// diff there is no git changeset to scan, so both stay empty and review.Run
	// degrades to the diff-window substrate.
	var (
		rulepack     []astquery.Match
		impact       map[string]*analysis.ImpactResult
		changedFiles []string
	)
	if diffText == "" {
		diff, err := analysis.MapGitDiff(s.graph, repoRoot, repoPrefix, scope, baseRef)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		allowedRepos, err := s.resolveRepoFilter(ctx, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		rulepack = s.reviewRulepackMatches(ctx, diff.ChangedFiles, analysis.RepoRelativePath, repoPrefix, allowedRepos)
		impact = s.reviewImpact(diff.ChangedSymbols)
		changedFiles = diff.ChangedFiles
	}

	// LLM seam: a closure over the optional LLM service's Generate, gated on the
	// caller's use_llm and the service actually being enabled. nil disables the
	// LLM phases entirely — review.Run then carries only rulepack findings.
	useLLM := requestBoolDefault(req, "use_llm", false)
	gen := s.reviewLLMGenWithUsage(useLLM)

	suppStore, suppRepoKey := s.reviewSuppressions()
	report, err := review.RunWithUsage(ctx, s.graph, gen, s.reviewPricing(), review.Options{
		RepoRoot:        repoRoot,
		RepoPrefix:      repoPrefix,
		CoverageKnown:   s.coverageKnownForDiff(repoPrefix, changedFiles),
		Scope:           scope,
		BaseRef:         baseRef,
		Diff:            diffText,
		RulepackMatches: rulepack,
		Impact:          impact,
		Rules:           s.reviewRuleResolver(repoRoot),
		Config:          s.reviewConfig(repo),
		UseLLM:          useLLM && gen != nil,
		TokenBudget:     intArg(req.GetArguments(), "max_tokens", 0),
		Suppressions:    suppStore,
		RepoKey:         suppRepoKey,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	payload := reviewPayload(report)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeReview(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// reviewRulepackMatches runs the graph-grounded review detector bundle over just
// the changed files and returns the surviving matches. It mirrors the analyze
// review path (DetectorsByCategory("review") + GroundReviewMatches) but narrows
// the AST targets to the changeset so the review tool only flags changed code.
//
// repoPrefix bridges the two path vocabularies the narrowing spans: git names
// changed files relative to the working tree while the graph keys every file
// node "<prefix>/<rel>". Matches come back repo-relative — the spelling that
// rule resolution, the per-file risk ranking, and the forge comment API speak.
//
// domain declares which vocabulary changedFiles is spelled in, because the two
// overlap and cannot be recovered by inspection — see reviewChangedGraphPaths.
func (s *Server) reviewRulepackMatches(ctx context.Context, changedFiles []string, domain analysis.PathDomain, repoPrefix string, allowedRepos map[string]bool) []astquery.Match {
	bundle := astquery.DetectorsByCategory("review")
	if len(bundle) == 0 {
		return nil
	}

	allTargets, err := s.buildASTTargets("", "", allowedRepos)
	if err != nil || len(allTargets) == 0 {
		return nil
	}

	// Narrow to the changed-file set so the rulepack only scans the changeset,
	// not the whole repository.
	changed := reviewChangedGraphPaths(changedFiles, domain, repoPrefix)
	if len(changed) == 0 {
		return nil
	}
	targets := make([]astquery.Target, 0, len(allTargets))
	for _, t := range allTargets {
		// Normalize, do not Clean. A graph path is "<prefix>/" + the rest in
		// native separators, so filepath.Clean rewrites the prefix slash on
		// Windows ("repo-a/pkg\widget.go" -> "repo-a\pkg\widget.go") and the
		// key the changeset built no longer matches. graphpath.Norm is the
		// canonical comparison form and is the identity on POSIX.
		if changed[graphpath.Norm(t.GraphPath)] {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	var collected []astquery.Match
	for _, d := range bundle {
		res, runErr := astquery.Run(ctx, astquery.Options{
			Detector:     d.Name,
			Targets:      targets,
			Resolver:     astquery.DefaultLanguageResolver,
			Limit:        5000,
			ExcludeTests: true,
		})
		if runErr != nil {
			continue
		}
		collected = append(collected, res.Matches...)
	}

	// Graph-grounding post-pass: drop the N+1 / check-then-act rows the resolved
	// call / loop metadata refutes. This is the same FP-reduction the analyze
	// review path applies. Grounding keys off the match's symbol id, so bounded
	// post-match enrichment must precede it; path rewriting still happens only
	// after the surviving rows are known.
	s.enrichASTMatchesContext(ctx, collected)
	kept := review.GroundReviewMatches(s.graph, collected)
	for i := range kept {
		kept[i].File = reviewRepoRelPath(kept[i].File, repoPrefix)
	}
	return kept
}

// reviewChangedGraphPaths maps a changeset's file paths onto the vocabulary the
// graph keys file nodes in. `git diff` names files relative to the working tree
// while a multi-repo daemon keys every node "<prefix>/<rel>", so intersecting
// the two spellings raw matched nothing: `review` reported zero findings on the
// very code its own detector bundle flags under `analyze --kind review`.
//
// Only the prefixed spelling is admitted once a prefix applies — a bare
// relative path would also match a same-named file in a sibling tracked repo.
//
// The caller states which vocabulary it holds; this function never guesses.
// The two domains are not distinguishable by inspection: in a repo whose tree
// carries a top-level directory named like the repo prefix,
// `repo-a/pkg/widget.go` is a valid path in *both*. Inferring "already
// prefixed" from that spelling skips the real key
// `repo-a/repo-a/pkg/widget.go`, so the changed target is missed — and when a
// same-named `pkg/widget.go` also exists, that unchanged shadow is scanned in
// its place. Admitting both candidate keys is not a fix either: it still lets
// unchanged code be selected.
func reviewChangedGraphPaths(changedFiles []string, domain analysis.PathDomain, repoPrefix string) map[string]bool {
	changed := make(map[string]bool, len(changedFiles))
	for _, f := range changedFiles {
		// Clean collapses "./" and "..", then Norm puts both vocabularies in
		// the one comparison spelling — git already speaks '/', and a caller
		// handing in a graph-keyed path carries native separators after the
		// prefix. Identity on POSIX.
		f = graphpath.Norm(filepath.Clean(strings.TrimSpace(f)))
		if f == "" || f == "." {
			continue
		}
		if domain == analysis.RepoRelativePath && repoPrefix != "" {
			f = repoPrefix + "/" + f
		}
		changed[f] = true
	}
	return changed
}

// reviewRepoRelPath strips the graph repo prefix off a file path, returning the
// repo-relative spelling the rest of the review pipeline speaks: the rule
// resolver matches `.gortex.yaml` globs against it, rankFileRisk keys its rows
// on it, and post_review hands it to the forge's comment API.
// Every one of those consumers speaks '/', so the result is normalized: a
// graph path carries native separators after the prefix, and handing
// `pkg\widget.go` to a `.gortex.yaml` glob or the forge comment API would
// match nothing and anchor a comment nowhere. Identity on POSIX.
func reviewRepoRelPath(path, repoPrefix string) string {
	path = graphpath.Norm(path)
	if repoPrefix == "" {
		return path
	}
	return strings.TrimPrefix(path, repoPrefix+"/")
}

// testLangByExt maps a source-file extension onto the language family its
// covering tests share. The family is the probe's unit because a test file
// carries the same extension family as the code it exercises — a `.tsx`
// component is covered by a `.test.ts` or a `.test.tsx` — so asking "does the
// index carry tests for this language?" is one lookup per changed file.
// Extensions absent from the table have no convention to attest and are
// skipped.
var testLangByExt = map[string]string{
	".go":    "go",
	".ts":    "ts",
	".tsx":   "ts",
	".mts":   "ts",
	".cts":   "ts",
	".js":    "js",
	".jsx":   "js",
	".mjs":   "js",
	".cjs":   "js",
	".py":    "py",
	".rb":    "rb",
	".rs":    "rs",
	".dart":  "dart",
	".java":  "java",
	".kt":    "kt",
	".cs":    "cs",
	".swift": "swift",
	".php":   "php",
}

// testLangsIndexed returns the language families this repo's graph carries
// indexed test symbols for. One scan per analysis epoch, cached per repo
// prefix and invalidated by RunAnalysis — the answer changes with a reindex,
// and pinning it for the daemon's lifetime meant a probe that ran during
// warmup (before the test-edge pass stamps its symbols) reported "no tests"
// until the daemon was restarted.
//
// A node counts as evidence when the indexer stamped it a test symbol or its
// path follows a recognised convention. The stamp is what makes this
// runner-agnostic: an annotation-driven test (Rust #[test], JUnit @Test) or
// a co-located vitest probe carries no distinguishing path fragment, and
// probing for hardcoded fragments instead reported "the index carries no
// test symbols" over a graph full of them. Every `tests` edge originates at
// a stamped symbol, so scanning nodes covers the edge evidence too.
func (s *Server) testLangsIndexed(repoPrefix string) map[string]bool {
	if v, ok := s.testIndexProbe.Load(repoPrefix); ok {
		return v.(map[string]bool)
	}
	var nodes []*graph.Node
	if repoPrefix != "" {
		nodes = s.graph.GetRepoNodes(repoPrefix)
	} else {
		nodes = s.graph.AllNodes()
	}
	langs := map[string]bool{}
	for _, n := range nodes {
		if n == nil || n.Kind == graph.KindFile || !nodeIsTestSymbol(n) {
			continue
		}
		if lang, ok := testLangByExt[strings.ToLower(filepath.Ext(n.FilePath))]; ok {
			langs[lang] = true
		}
	}
	s.testIndexProbe.Store(repoPrefix, langs)
	return langs
}

// nodeIsTestSymbol reports whether a node is test code, preferring the
// indexer's own stamp over the path convention so a test the runner
// discovers by attribute rather than by filename still counts.
func nodeIsTestSymbol(n *graph.Node) bool {
	if v, ok := n.Meta["is_test"].(bool); ok && v {
		return true
	}
	return testpath.IsTestFile(n.FilePath)
}

// coverageKnownForDiff reports whether the graph can attest test coverage
// for this changeset: every changed file whose language has a test
// convention must have test symbols for that language present in the index.
// An index config that excludes a language's test files (e.g. "**/*_test.go")
// makes "no covering test" blindness, not a finding — the review then says
// "coverage unknown" instead of "untested".
func (s *Server) coverageKnownForDiff(repoPrefix string, changedFiles []string) bool {
	langs := s.testLangsIndexed(repoPrefix)
	sawCode := false
	for _, f := range changedFiles {
		lang, ok := testLangByExt[strings.ToLower(filepath.Ext(f))]
		if !ok {
			continue
		}
		sawCode = true
		if !langs[lang] {
			return false
		}
	}
	return sawCode
}

// reviewImpact builds the per-changed-symbol blast-radius map review.Run uses to
// rank per-file risk. A symbol whose impact analysis is empty is omitted.
func (s *Server) reviewImpact(changed []analysis.ChangedSymbol) map[string]*analysis.ImpactResult {
	if len(changed) == 0 {
		return nil
	}
	communities := s.getCommunities()
	processes := s.getProcesses()
	out := make(map[string]*analysis.ImpactResult, len(changed))
	for _, cs := range changed {
		if cs.ID == "" {
			continue
		}
		if ir := analysis.AnalyzeImpact(s.graph, []string{cs.ID}, communities, processes); ir != nil {
			out[cs.ID] = ir
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reviewLLMGenWithUsage returns the usage-aware LLM seam the review flow runs
// through so the report carries a per-review CostBreakdown: a closure over the
// optional LLM service's usage-aware Generate, or nil when the LLM phase is not
// engaged (caller opted out, no service, or the service is disabled). A nil
// seam disables the MAIN/RELOCATE phases, and still yields a zero
// (un-Estimated) cost block from RunWithUsage.
func (s *Server) reviewLLMGenWithUsage(useLLM bool) review.LLMGenWithUsage {
	if !useLLM {
		return nil
	}
	// Test-only seam: a non-nil usage-aware override stands in for the real
	// provider so the cost-bearing review path can be driven without a backend.
	if s.reviewLLMGenWithUsageOverride != nil {
		return s.reviewLLMGenWithUsageOverride()
	}
	// Backward-compatible test seam: a test that only set the plain
	// reviewLLMGenOverride (no usage) still drives the LLM phase — adapt it up
	// to the usage-aware shape, reporting zero usage (so the cost block is a
	// zero, un-Estimated breakdown, exactly as a no-usage provider would).
	if s.reviewLLMGenOverride != nil {
		plain := s.reviewLLMGenOverride()
		if plain == nil {
			return nil
		}
		return func(ctx context.Context, prompt string, maxTokens int) (string, llm.TokenUsage, error) {
			text, err := plain(ctx, prompt, maxTokens)
			return text, llm.TokenUsage{}, err
		}
	}
	if s.llmService == nil || !s.llmService.Enabled() {
		return nil
	}
	return func(ctx context.Context, prompt string, maxTokens int) (string, llm.TokenUsage, error) {
		return s.llmService.GenerateWithUsage(ctx, prompt, maxTokens)
	}
}

// reviewPricing resolves the rate card the review cost block prices token usage
// against: the test override when set, else the active LLM provider's pricing,
// else a zero card (a zero USD estimate, never an omitted cost block).
func (s *Server) reviewPricing() llm.ProviderPricing {
	if s.reviewPricingOverride != nil {
		return *s.reviewPricingOverride
	}
	if s.llmService != nil {
		return s.llmService.Pricing()
	}
	return llm.ProviderPricing{}
}

// reviewConfig loads the repo's `review:` block (the gate / depth / rule
// knobs). A nil config manager — the legacy / test-without-config path —
// yields a zero ReviewConfig, which is a pass-through: the gate drops nothing
// and the depth classifier falls back to its built-in default ladder, so
// today's behaviour is preserved exactly.
func (s *Server) reviewConfig(repo string) config.ReviewConfig {
	if s.configManager == nil {
		return config.ReviewConfig{}
	}
	cfg := s.configManager.GetRepoConfig(repo)
	if cfg == nil {
		return config.ReviewConfig{}
	}
	return cfg.Review
}

// reviewRuleResolver builds the 4-layer review rule resolver rooted at the
// repo. An empty repoRoot skips the repo-local / project layers, leaving the
// global + embedded layers — so the resolver always resolves (the embedded
// `**` catch-all guarantees it). A construction error (a malformed rule file)
// degrades to nil: the flow then carries no per-file rule grounding rather than
// failing the whole review.
func (s *Server) reviewRuleResolver(repoRoot string) *review.RuleResolver {
	resolver, err := review.NewRuleResolver("", repoRoot)
	if err != nil {
		return nil
	}
	return resolver
}

// reviewPayload projects a ReviewReport onto the review tool's wire shape: the
// verdict envelope plus the line-anchored inline comments derived from the
// report's findings.
func reviewPayload(report *review.ReviewReport) map[string]any {
	commentRows := make([]map[string]any, 0)
	fileRisk := make([]map[string]any, 0)
	verdict := ""
	summary := ""
	depth := ""
	var stats any = map[string]any{}
	var cost map[string]any
	if report != nil {
		verdict = string(report.Verdict)
		summary = report.Summary
		stats = report.Stats
		depth = report.Depth
		cost = reviewCostMap(report.Cost)
		for _, f := range report.Findings {
			line := f.Line
			if line == 0 {
				line = f.StartLine
			}
			row := map[string]any{
				"file":     f.File,
				"line":     line,
				"severity": string(f.Severity),
				"message":  f.Message,
				"rule":     f.Rule,
				"category": f.Category,
				"source":   f.Source,
			}
			// Expose the stable identity key so an agent can suppress this exact
			// finding via suppress_finding without re-deriving it.
			if f.IdentityKey != "" {
				row["identity_key"] = f.IdentityKey
			}
			commentRows = append(commentRows, row)
		}
		for _, fr := range report.FileRisk {
			fileRisk = append(fileRisk, map[string]any{
				"file":      fr.File,
				"risk":      fr.Risk,
				"findings":  fr.Findings,
				"affected":  fr.Affected,
				"symbols":   fr.Symbols,
				"uncovered": fr.Uncovered,
			})
		}
	}

	payload := map[string]any{
		"verdict":   verdict,
		"summary":   summary,
		"comments":  commentRows,
		"file_risk": fileRisk,
		"total":     len(commentRows),
		"stats":     stats,
		"depth":     depth,
	}
	// The gate suppression summary rides on stats.Gate; lift it to the top
	// level too so an agent can read "N findings suppressed" without decoding
	// the whole stats block.
	if report != nil {
		payload["gate"] = report.Stats.Gate
	}
	// The per-review cost block is present only when the review ran through the
	// usage-aware seam (it always does now), so include it whenever non-nil.
	if cost != nil {
		payload["cost"] = cost
	}
	return payload
}

// reviewCostMap projects a CostBreakdown onto the review response's wire shape:
// the token split, the USD estimate, whether the estimate is grounded in a real
// usage report, and the LLM wall-clock. A nil cost (the deterministic-only Run
// path) yields nil so the cost key is simply omitted.
func reviewCostMap(cost *review.CostBreakdown) map[string]any {
	if cost == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":       cost.InputTokens,
		"output_tokens":      cost.OutputTokens,
		"cache_read_tokens":  cost.CacheReadTokens,
		"cache_write_tokens": cost.CacheWriteTokens,
		"usd":                cost.USD,
		"estimated":          cost.Estimated,
		"elapsed_ms":         cost.ElapsedMs,
	}
}

// classifiedSymbol is one changed symbol with its coarse change class
// (feature/fix/refactor/test/config) and its impact-derived risk tier.
type classifiedSymbol struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Class string `json:"class"`
	Risk  string `json:"risk"`
}

// reviewEnvelope is the single packaged PR-review result: the worst-of verdict
// (review report, upgraded by contract/guard breaks), the per-symbol semantic
// classification, the per-file risk ranking, the line-anchored findings, the
// contract + guard gate results, the impacted test targets, the concrete
// verification command, the privacy-safe risk receipt, and an optional tiered
// review pack.
type reviewEnvelope struct {
	Verdict             string                    `json:"verdict"`
	Summary             string                    `json:"summary"`
	ChangedSymbols      []classifiedSymbol        `json:"changed_symbols"`
	FileRisk            []review.FileRisk         `json:"file_risk"`
	Findings            []inlineComment           `json:"findings"`
	Contracts           *contractImpact           `json:"contracts,omitempty"`
	Guards              []analysis.GuardViolation `json:"guards"`
	HighRiskPreviews    []reviewPreview           `json:"high_risk_previews,omitempty"`
	TestTargets         []string                  `json:"test_targets"`
	VerificationCommand string                    `json:"verification_command"`
	Receipt             analysis.ReviewReceipt    `json:"receipt"`
	Pack                *review.ReviewPack        `json:"pack,omitempty"`
	// Depth is the adaptive review depth the changeset classified into
	// (quick | standard | deep) — set from the review report.
	Depth string `json:"depth"`
	// Gate is the confidence / severity / category / cap suppression summary
	// the gate produced over the merged findings.
	Gate review.GateStats `json:"gate"`
	// Cost is the per-review token + USD accounting. Nil only when no
	// usage-aware seam ran (cannot happen on the live path now).
	Cost *review.CostBreakdown `json:"cost,omitempty"`
}

// reviewPreview is the cost-bounded speculative-edit preview run for a single
// high-risk (depth-1-heavy) changed symbol: the broken callers/implementors and
// the impact rollup the simulation produced from a no-op rewrite of the symbol's
// own range.
type reviewPreview struct {
	SymbolID           string `json:"symbol_id"`
	BrokenCallers      int    `json:"broken_callers"`
	BrokenImplementors int    `json:"broken_implementors"`
	Summary            string `json:"summary"`
}

// highRiskD1Threshold is the direct-dependent (depth-1) count at or above which a
// changed symbol is "high-risk" and earns a cost-bounded preview_edit simulation.
// Keeping the bound high means the speculative pass runs only for the few symbols
// a heavily-depended-on change touches, never the whole changeset.
const highRiskD1Threshold = 5

// handleReviewPack runs the full PR-review gate set over a changeset and folds
// the result into one packaged envelope. It composes the existing handlers /
// analysis — it never re-implements a gate:
//
//	MapGitDiff          → the changed symbols + files
//	AnalyzeImpact       → per-symbol blast radius (risk ranking + d=1 high-risk set)
//	review.Run          → the verdict report (findings + per-file risk + summary)
//	computeContractImpact → contract-boundary breaks
//	EvaluateGuards/Architecture → guard + layering violations
//	get-test-targets walk → the impacted test files → verification_command
//	buildSimulation     → a cost-bounded preview for the high-risk d=1 symbols only
//	ScorePRRisk/BuildReviewReceipt → the privacy-safe receipt (scrub passthrough)
//
// The verdict is the review report's worst-of, upgraded to BLOCK when a contract
// breaks or any guard/architecture rule is violated (mirroring the contract-risk
// upgrade the enhanced change-impact handler applies).
func (s *Server) handleReviewPack(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	diffText := strings.TrimSpace(req.GetString("diff", ""))
	scope, baseRef := siblingDiffScope(req)

	repo := strings.TrimSpace(req.GetString("repo", ""))
	repoRoot, repoPrefix := s.diffRepoScope(ctx, repo)
	if repoRoot == "" && diffText == "" {
		return mcp.NewToolResultError("could not resolve a repository root for the changeset diff"), nil
	}

	// Enumerate the changeset (on-disk path only — a pasted diff has no graph
	// changeset, so the gates that need indexed symbols are skipped).
	var (
		diff     *analysis.DiffResult
		rulepack []astquery.Match
		impact   map[string]*analysis.ImpactResult
		ids      []string
	)
	if diffText == "" {
		d, err := analysis.MapGitDiff(s.graph, repoRoot, repoPrefix, scope, baseRef)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		diff = d
		allowedRepos, err := s.resolveRepoFilter(ctx, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		rulepack = s.reviewRulepackMatches(ctx, diff.ChangedFiles, analysis.RepoRelativePath, repoPrefix, allowedRepos)
		impact = s.reviewImpact(diff.ChangedSymbols)
		for _, cs := range diff.ChangedSymbols {
			if cs.ID != "" {
				ids = append(ids, cs.ID)
			}
		}
	}

	// Impacted test targets → the concrete verification command, and the
	// changeset's own coverage evidence. Resolved before the review runs
	// because a non-empty target list is proof the index carries test
	// symbols for this change: whatever the repo-wide probe concludes, the
	// envelope must never pair these files with a summary claiming there
	// are none.
	testTargets := s.reviewTestTargets(ctx, ids)

	// The review report (deterministic rulepack always; LLM phase gated).
	useLLM := requestBoolDefault(req, "use_llm", false)
	gen := s.reviewLLMGenWithUsage(useLLM)
	suppStore, suppRepoKey := s.reviewSuppressions()
	report, err := review.RunWithUsage(ctx, s.graph, gen, s.reviewPricing(), review.Options{
		RepoRoot:        repoRoot,
		RepoPrefix:      repoPrefix,
		CoverageKnown:   len(testTargets) > 0 || (diff != nil && s.coverageKnownForDiff(repoPrefix, diff.ChangedFiles)),
		Scope:           scope,
		BaseRef:         baseRef,
		Diff:            diffText,
		RulepackMatches: rulepack,
		Impact:          impact,
		Rules:           s.reviewRuleResolver(repoRoot),
		Config:          s.reviewConfig(repo),
		UseLLM:          useLLM && gen != nil,
		TokenBudget:     intArg(req.GetArguments(), "max_tokens", 0),
		Suppressions:    suppStore,
		RepoKey:         suppRepoKey,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Gate: contract-boundary impact.
	contracts := s.computeContractImpact(ids)

	// Gate: guard + architecture rules.
	var guards []analysis.GuardViolation
	if len(ids) > 0 && (s.hasGuardRules(ids) || !s.architecture.IsEmpty()) {
		guards = s.evaluateGuards(ids)
		guards = append(guards, analysis.EvaluateArchitecture(s.graph, s.architecture, ids)...)
	}

	// Verdict = the review report's worst-of, upgraded to BLOCK on any contract
	// break or guard/architecture violation.
	verdict := report.Verdict
	if (contracts != nil && contracts.Breaking > 0) || len(guards) > 0 {
		verdict = review.VerdictBlock
	}

	// Per-symbol semantic classification, graph-grounded on the diff-hunk text.
	changedSyms := s.classifyChangedSymbols(diff, impact)

	verCmd := review.VerificationCommand(testTargets, reviewPackLang(diff))

	// Cost bound: run a speculative preview_edit for the high-risk (d=1-heavy)
	// changed symbols only — never the whole changeset.
	previews, previewErr := s.highRiskPreviews(ctx, diff, impact)
	if previewErr != nil {
		return nil, previewErr
	}

	// Privacy-safe risk receipt over the whole changeset.
	scrub := requestBoolDefault(req, "scrub", false)
	receipt := s.reviewReceipt(ids, diff, contracts, guards, scrub)

	env := reviewEnvelope{
		Verdict:             string(verdict),
		Summary:             report.Summary,
		ChangedSymbols:      changedSyms,
		FileRisk:            report.FileRisk,
		Findings:            reviewFindingsToComments(report),
		Contracts:           contracts,
		Guards:              guards,
		HighRiskPreviews:    previews,
		TestTargets:         testTargets,
		VerificationCommand: verCmd,
		Receipt:             receipt,
		Depth:               report.Depth,
		Gate:                report.Stats.Gate,
		Cost:                report.Cost,
	}
	if requestBoolDefault(req, "include_pack", false) {
		env.Pack = s.buildReviewPack(diff, impact, intArg(req.GetArguments(), "max_tokens", 0))
	}

	payload := reviewPackPayload(env)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeReviewPack(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// classifyChangedSymbols stamps a change class + impact-risk tier on every
// changed symbol. The class is graph-grounded on the symbol's diff-hunk text;
// the risk tier is the symbol's AnalyzeImpact tier (LOW when no impact entry).
func (s *Server) classifyChangedSymbols(diff *analysis.DiffResult, impact map[string]*analysis.ImpactResult) []classifiedSymbol {
	if diff == nil {
		return []classifiedSymbol{}
	}
	view, _ := s.reviewChangeView(diff)
	out := make([]classifiedSymbol, 0, len(diff.ChangedSymbols))
	for _, cs := range diff.ChangedSymbols {
		hunk := review.SymbolHunk(s.graph, view, cs)
		risk := string(analysis.RiskLow)
		if impact != nil {
			if ir := impact[cs.ID]; ir != nil && ir.Risk != "" {
				risk = string(ir.Risk)
			}
		}
		out = append(out, classifiedSymbol{
			ID:    cs.ID,
			Name:  cs.Name,
			Class: review.ClassifyChange(s.graph, cs, hunk),
			Risk:  risk,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// reviewChangeView builds the ChangeView the classify/pack rendering reads its
// diff-hunk text from. Errors degrade to a nil view (the renderers tolerate it).
func (s *Server) reviewChangeView(diff *analysis.DiffResult) (*review.ChangeView, *analysis.DiffResult) {
	repoRoot := ""
	if s.indexer != nil {
		repoRoot = s.indexer.RootPath()
	}
	view, err := review.BuildChangeView(s.graph, repoRoot, s.diffJoinPrefix(repoRoot), "", "")
	if err != nil {
		return nil, diff
	}
	return view, diff
}

// reviewTestTargets walks the impacted symbols to their covering test files,
// reusing the same EdgeTests / caller walk get_test_targets uses, and returns
// the distinct test-file paths.
func (s *Server) reviewTestTargets(ctx context.Context, ids []string) []string {
	files := map[string]bool{}
	eng := s.engineFor(ctx)
	for _, id := range ids {
		node := eng.GetSymbol(id)
		if node == nil {
			continue
		}
		// Fast path: the persistent test-edge inverse walk.
		if testers := eng.GetTesters(id); len(testers) > 0 {
			for _, tn := range testers {
				if tn != nil && tn.FilePath != "" {
					files[cleanPathMCP(tn.FilePath)] = true
				}
			}
			continue
		}
		// Fallback: BFS callers, keep the ones in test files.
		callers := eng.GetCallers(id, query.QueryOptions{Depth: 3, Limit: 100, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if isTestFile(cn.FilePath) {
				files[cleanPathMCP(cn.FilePath)] = true
			}
		}
		if isTestFile(node.FilePath) {
			files[cleanPathMCP(node.FilePath)] = true
		}
	}
	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// highRiskPreviews runs a cost-bounded speculative preview for each high-risk
// changed symbol — one whose direct-dependent (depth-1) count meets the
// threshold. A no-op rewrite of the symbol's own range is fed to buildSimulation
// so the broken-callers / impact rollup is computed without touching disk. Only
// the high-risk subset is simulated, so the pass never scales with the whole
// changeset.
func (s *Server) highRiskPreviews(ctx context.Context, diff *analysis.DiffResult, impact map[string]*analysis.ImpactResult) ([]reviewPreview, error) {
	if diff == nil || impact == nil {
		return nil, nil
	}
	var previews []reviewPreview
	for _, cs := range diff.ChangedSymbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ir := impact[cs.ID]
		if ir == nil || len(ir.ByDepth[1]) < highRiskD1Threshold {
			continue
		}
		edit, ok := s.identityEditForSymbol(cs.ID)
		if !ok {
			continue
		}
		sim, err := s.buildSimulation(ctx, []lsp.WorkspaceEdit{edit}, false)
		if err != nil {
			if ctxErr := requestContextError(ctx, err); ctxErr != nil {
				return nil, ctxErr
			}
			continue
		}
		if len(sim.steps) == 0 {
			continue
		}
		step := sim.steps[0]
		previews = append(previews, reviewPreview{
			SymbolID:           cs.ID,
			BrokenCallers:      len(step.brokenCallers),
			BrokenImplementors: len(step.brokenImplementors),
			Summary:            step.summary,
		})
	}
	sort.SliceStable(previews, func(i, j int) bool { return previews[i].SymbolID < previews[j].SymbolID })
	return previews, nil
}

// identityEditForSymbol builds a no-op WorkspaceEdit that rewrites a symbol's own
// [StartLine,EndLine] range with its current source. Fed to buildSimulation it
// yields the symbol's broken-callers / impact preview without changing disk. Only
// nodes with a known range under the indexed root are eligible.
func (s *Server) identityEditForSymbol(id string) (lsp.WorkspaceEdit, bool) {
	n := s.graph.GetNode(id)
	if n == nil || n.StartLine <= 0 || n.FilePath == "" {
		return lsp.WorkspaceEdit{}, false
	}
	abs, err := s.resolveOverlayAbsPath(n.FilePath)
	if err != nil || abs == "" {
		return lsp.WorkspaceEdit{}, false
	}
	data, rerr := os.ReadFile(abs)
	if rerr != nil {
		return lsp.WorkspaceEdit{}, false
	}
	lines := strings.Split(string(data), "\n")
	end := n.EndLine
	if end < n.StartLine {
		end = n.StartLine
	}
	if n.StartLine > len(lines) {
		return lsp.WorkspaceEdit{}, false
	}
	if end > len(lines) {
		end = len(lines)
	}
	src := strings.Join(lines[n.StartLine-1:end], "\n") + "\n"
	edit := lsp.WorkspaceEdit{
		Changes: map[string][]lsp.TextEdit{
			n.FilePath: {{
				Range: lsp.Range{
					Start: lsp.Position{Line: n.StartLine - 1, Character: 0},
					End:   lsp.Position{Line: end, Character: 0},
				},
				NewText: src,
			}},
		},
	}
	return edit, true
}

// reviewReceipt scores PR-level risk over the changeset and projects it to the
// privacy-safe receipt. A contract break or guard violation flags the
// out-of-band hard blocker so the receipt's merge_blocker reflects the gates.
func (s *Server) reviewReceipt(ids []string, diff *analysis.DiffResult, ci *contractImpact, guards []analysis.GuardViolation, scrub bool) analysis.ReviewReceipt {
	var changedFiles []string
	if diff != nil {
		changedFiles = diff.ChangedFiles
	}
	communities := s.getCommunities()
	var nodeToComm map[string]string
	if communities != nil {
		nodeToComm = communities.NodeToComm
	}
	result := analysis.ScorePRRisk(s.graph, analysis.PRRiskInput{
		SymbolIDs:    ids,
		ChangedFiles: changedFiles,
		NodeToComm:   nodeToComm,
		Communities:  communities,
		Processes:    s.getProcesses(),
	})
	blocker := (ci != nil && ci.Breaking > 0) || len(guards) > 0
	return analysis.BuildReviewReceipt(result, "NONE", blocker, scrub)
}

// buildReviewPack renders the FG9 tiered review pack for the envelope: changed
// symbols as diff hunks, direct callers as full source, the rest as an outline.
func (s *Server) buildReviewPack(diff *analysis.DiffResult, impact map[string]*analysis.ImpactResult, budget int) *review.ReviewPack {
	if diff == nil {
		return nil
	}
	view, _ := s.reviewChangeView(diff)
	return review.BuildReviewPack(s.graph, view, diff, review.MergeImpact(impact), budget)
}

// reviewFindingsToComments projects the report's findings onto the line-anchored
// inline-comment shape the envelope carries.
func reviewFindingsToComments(report *review.ReviewReport) []inlineComment {
	out := make([]inlineComment, 0)
	if report == nil {
		return out
	}
	for _, f := range report.Findings {
		line := f.Line
		if line == 0 {
			line = f.StartLine
		}
		out = append(out, inlineComment{
			File:     f.File,
			Line:     line,
			Severity: string(f.Severity),
			Message:  f.Message,
			Rule:     f.Rule,
			Category: f.Category,
			Source:   f.Source,
		})
	}
	return out
}

// reviewPackLang infers the dominant language of the changeset from the changed
// file extensions so the verification command targets the right toolchain.
func reviewPackLang(diff *analysis.DiffResult) string {
	if diff == nil {
		return ""
	}
	counts := map[string]int{}
	for _, f := range diff.ChangedFiles {
		switch strings.ToLower(filepath.Ext(f)) {
		case ".go":
			counts["go"]++
		case ".py":
			counts["python"]++
		case ".ts", ".tsx":
			counts["typescript"]++
		case ".js", ".jsx":
			counts["javascript"]++
		case ".rs":
			counts["rust"]++
		}
	}
	best, bestN := "", 0
	for lang, n := range counts {
		if n > bestN || (n == bestN && lang < best) {
			best, bestN = lang, n
		}
	}
	return best
}

// cleanPathMCP cleans a graph-relative path to slash form for the test-target set.
func cleanPathMCP(p string) string {
	return filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
}

// reviewPackPayload projects the envelope onto the wire map shape. The map form
// keeps the GCX / TOON encoders and the byte/token budget machinery on the same
// path as the rest of the review surface.
func reviewPackPayload(env reviewEnvelope) map[string]any {
	changed := make([]map[string]any, 0, len(env.ChangedSymbols))
	for _, c := range env.ChangedSymbols {
		changed = append(changed, map[string]any{
			"id":    c.ID,
			"name":  c.Name,
			"class": c.Class,
			"risk":  c.Risk,
		})
	}
	fileRisk := make([]map[string]any, 0, len(env.FileRisk))
	for _, fr := range env.FileRisk {
		fileRisk = append(fileRisk, map[string]any{
			"file":      fr.File,
			"risk":      fr.Risk,
			"findings":  fr.Findings,
			"affected":  fr.Affected,
			"symbols":   fr.Symbols,
			"uncovered": fr.Uncovered,
		})
	}
	findings := make([]map[string]any, 0, len(env.Findings))
	for _, f := range env.Findings {
		findings = append(findings, map[string]any{
			"file":     f.File,
			"line":     f.Line,
			"severity": f.Severity,
			"message":  f.Message,
			"rule":     f.Rule,
			"category": f.Category,
			"source":   f.Source,
		})
	}
	previews := make([]map[string]any, 0, len(env.HighRiskPreviews))
	for _, p := range env.HighRiskPreviews {
		previews = append(previews, map[string]any{
			"symbol_id":           p.SymbolID,
			"broken_callers":      p.BrokenCallers,
			"broken_implementors": p.BrokenImplementors,
			"summary":             p.Summary,
		})
	}
	payload := map[string]any{
		"verdict":              env.Verdict,
		"summary":              env.Summary,
		"changed_symbols":      changed,
		"file_risk":            fileRisk,
		"findings":             findings,
		"guards":               env.Guards,
		"test_targets":         env.TestTargets,
		"verification_command": env.VerificationCommand,
		"receipt":              env.Receipt,
		"total":                len(findings),
		"depth":                env.Depth,
		"gate":                 env.Gate,
	}
	if cost := reviewCostMap(env.Cost); cost != nil {
		payload["cost"] = cost
	}
	if env.Contracts != nil {
		payload["contracts"] = env.Contracts
	}
	if len(previews) > 0 {
		payload["high_risk_previews"] = previews
	}
	if env.Pack != nil {
		payload["pack"] = env.Pack
	}
	return payload
}
