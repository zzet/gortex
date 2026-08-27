package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/codeowners"
	"github.com/zzet/gortex/internal/forge"
)

// Signal weights for the reviewer blend. CODEOWNERS is an explicit,
// maintainer-declared "who must look at this" mapping, so it outranks
// recent authorship, which in turn outranks a co-change expert (a
// person who has merely touched files that historically change
// alongside the changeset).
const (
	reviewerWeightCodeowner = 3
	reviewerWeightAuthor    = 2
	reviewerWeightCoChange  = 1
)

// ReviewerSuggestion is one ranked reviewer recommendation. Score is the
// weighted blend of the signals the reviewer appeared in; Reasons names
// each contributing signal in human-readable form; MatchedFiles lists the
// changed files a CODEOWNERS rule mapped to this reviewer (empty for a
// reviewer surfaced only by authorship / co-change).
type ReviewerSuggestion struct {
	Reviewer     string   `json:"reviewer"`
	Kind         string   `json:"kind"`
	Score        int      `json:"score"`
	Reasons      []string `json:"reasons"`
	MatchedFiles []string `json:"matched_files"`
}

// registerSuggestReviewersTool registers the suggest_reviewers MCP tool — a
// reviewer recommender that blends CODEOWNERS matches, recent authorship of
// the changed symbols, and co-change experts into one ranked list. Deferred:
// it goes into the lazy catalog unless lazy tools are disabled.
func (s *Server) registerSuggestReviewersTool() {
	s.addTool(
		mcp.NewTool("suggest_reviewers",
			mcp.WithDescription("Rank the people / teams best placed to review a changeset. Blends three signals — CODEOWNERS matches (strongest), recent authorship of the changed symbols, and co-change experts (authors of files that historically change alongside the changeset) — into one ranked reviewer list with per-reviewer reasons and matched files. Pass `ids` (comma-separated changed symbol IDs), `base` (a git ref — the changed set is derived from the diff against it), or `number` (a GitHub PR number — files fetched via the forge). Use to pick reviewers before opening or assigning a PR."),
			mcp.WithString("ids", mcp.Description("Comma-separated changed symbol IDs (already mapped). One of ids|base|number is required.")),
			mcp.WithString("base", mcp.Description("Base git ref (e.g. main). Derives the changed files/symbols from `git diff base...HEAD` against the indexed repo.")),
			mcp.WithNumber("number", mcp.Description("GitHub PR number. The changed file set is fetched via the forge (needs GH_TOKEN / GITHUB_TOKEN in the daemon environment).")),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of ranked reviewers returned (default 10).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleSuggestReviewers,
	)
}

// handleSuggestReviewers resolves the changeset (from ids / base / number),
// gathers the three reviewer signals against the changed files, ranks them,
// and projects the result to the suggest_reviewers wire shape.
func (s *Server) handleSuggestReviewers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	limit := req.GetInt("limit", 10)
	if limit < 1 {
		limit = 10
	}

	repo := strings.TrimSpace(req.GetString("repo", ""))
	repoRoot, repoPrefix := s.diffRepoScope(ctx, repo)

	changedFiles, fileDomain, _, err := s.resolveReviewerChangeset(ctx, req, repoRoot)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// CODEOWNERS signal — declared owners for each changed file.
	coRules, _, coFound := codeowners.LoadFromRepo(repoRoot)
	codeownerCounts := map[string]int{}
	codeownerKinds := map[string]string{}
	codeownerFiles := map[string][]string{}
	if coFound {
		for _, f := range changedFiles {
			// CODEOWNERS rules are written against repo-relative paths, and a
			// root-anchored rule (`/pkg/auth/`) only matches that spelling. In
			// ids mode the changed files are graph keys, so relForRepo — which
			// only strips an absolute repo root — would leave the repo prefix
			// on and silently match nothing.
			rel := analysis.RepoRelPath(repoPrefix, f, fileDomain)
			owners := codeowners.MatchFile(relForRepo(rel, repoRoot), coRules)
			for _, owner := range owners {
				name := normalizeReviewer(owner)
				if name == "" {
					continue
				}
				codeownerCounts[name]++
				codeownerKinds[name] = classifyReviewer(owner)
				// matched_files is reported to the caller, so it carries the
				// repo-relative spelling too, not the graph key.
				codeownerFiles[name] = appendUnique(codeownerFiles[name], rel)
			}
		}
	}

	// Ownership signal — recent authors of the changed symbols / files.
	// The changed-file paths are repo-relative (git / forge), so the node
	// join is prefix-aware in multi-repo mode.
	blame := blameRowsByID(s.graph)
	authorCounts := map[string]int{}
	for _, f := range changedFiles {
		for _, n := range analysis.JoinFileNodes(s.graph, repoPrefix, f, fileDomain) {
			if la, ok := lastAuthoredFrom(blame, n); ok && la.Email != "" {
				authorCounts[normalizeReviewer(la.Email)]++
			}
		}
	}

	// Co-change signal — recent authors of files that historically change
	// alongside the changed files. Each co-changing file's authors become
	// candidate experts; the count is the number of co-change links.
	coChangeCounts := map[string]int{}
	for _, f := range changedFiles {
		for partner := range s.coChangeScores(analysis.GraphKey(repoPrefix, f, fileDomain)) {
			for _, n := range s.graph.GetFileNodes(partner) {
				if la, ok := lastAuthoredFrom(blame, n); ok && la.Email != "" {
					coChangeCounts[normalizeReviewer(la.Email)]++
				}
			}
		}
	}

	reviewers := rankReviewers(codeownerCounts, authorCounts, coChangeCounts, codeownerKinds, codeownerFiles)
	if len(reviewers) > limit {
		reviewers = reviewers[:limit]
	}

	payload := suggestReviewersPayload(reviewers, changedFiles, coFound)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeSuggestReviewers(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// resolveReviewerChangeset turns the ids / base / number input into a set of
// changed file paths (and, where available, the changed symbol IDs). Exactly
// one input source is honoured, checked in ids → base → number order.
//
// The three sources do NOT share a path vocabulary, so the domain is returned
// with the files rather than assumed by the caller: ids yields graph node
// FilePaths, while base (git) and number (forge) yield repo-relative paths.
// Forcing one domain on all three double-prefixes the ids branch on a prefixed
// graph and silently drops the ownership and co-change signals.
func (s *Server) resolveReviewerChangeset(ctx context.Context, req mcp.CallToolRequest, repoRoot string) (files []string, domain analysis.PathDomain, symbolIDs []string, err error) {
	idsStr := strings.TrimSpace(req.GetString("ids", ""))
	base := strings.TrimSpace(req.GetString("base", ""))
	number := req.GetInt("number", 0)

	switch {
	case idsStr != "":
		fileSeen := map[string]bool{}
		for _, id := range strings.Split(idsStr, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			symbolIDs = append(symbolIDs, id)
			if n := s.graph.GetNode(id); n != nil && n.FilePath != "" && !fileSeen[n.FilePath] {
				fileSeen[n.FilePath] = true
				files = append(files, n.FilePath)
			}
		}
		return files, analysis.GraphKeyedPath, symbolIDs, nil

	case base != "":
		if repoRoot == "" {
			return nil, analysis.RepoRelativePath, nil, fmt.Errorf("could not resolve a repository root for the base diff")
		}
		diff, derr := analysis.MapGitDiff(s.graph, repoRoot, s.diffJoinPrefix(repoRoot), "compare", base)
		if derr != nil {
			return nil, analysis.RepoRelativePath, nil, fmt.Errorf("git diff against %q failed: %v", base, derr)
		}
		for _, cs := range diff.ChangedSymbols {
			symbolIDs = append(symbolIDs, cs.ID)
		}
		return diff.ChangedFiles, analysis.RepoRelativePath, symbolIDs, nil

	case number > 0:
		if !forge.Available(ctx) {
			return nil, analysis.RepoRelativePath, nil, fmt.Errorf("forge unavailable: set GH_TOKEN (or GITHUB_TOKEN) in the daemon environment to resolve PR files")
		}
		if repoRoot == "" {
			return nil, analysis.RepoRelativePath, nil, fmt.Errorf("could not resolve a repository root for the PR file fetch")
		}
		prFiles, ferr := forge.PRFiles(ctx, repoRoot, number)
		if ferr != nil {
			return nil, analysis.RepoRelativePath, nil, fmt.Errorf("fetching files for PR #%d failed: %v", number, ferr)
		}
		// A forge file list is repo-relative.
		return prFiles, analysis.RepoRelativePath, nil, nil

	default:
		return nil, analysis.RepoRelativePath, nil, fmt.Errorf("one of ids, base, or number is required")
	}
}

// rankReviewers blends the three reviewer signals into a deterministic ranked
// list. Each signal contributes its weight times the per-reviewer count; the
// reasons name each contributing signal. kinds and matchedFiles are
// presentation metadata keyed by reviewer (a CODEOWNERS-derived classification
// and the changed files a rule mapped to the reviewer) — they never affect the
// score. The function is pure: it takes plain maps and is fully table-testable
// without a graph. Ties on score break on reviewer name ascending.
func rankReviewers(codeowners, authors, coChangeExperts map[string]int, kinds map[string]string, matchedFiles map[string][]string) []ReviewerSuggestion {
	type acc struct {
		score   int
		reasons []string
	}
	merged := map[string]*acc{}
	get := func(name string) *acc {
		a, ok := merged[name]
		if !ok {
			a = &acc{}
			merged[name] = a
		}
		return a
	}

	for name, n := range codeowners {
		if name == "" || n <= 0 {
			continue
		}
		a := get(name)
		a.score += reviewerWeightCodeowner * n
		a.reasons = append(a.reasons, fmt.Sprintf("CODEOWNERS for %s", plural(n, "file")))
	}
	for name, n := range authors {
		if name == "" || n <= 0 {
			continue
		}
		a := get(name)
		a.score += reviewerWeightAuthor * n
		a.reasons = append(a.reasons, fmt.Sprintf("recent author of %s", plural(n, "changed symbol")))
	}
	for name, n := range coChangeExperts {
		if name == "" || n <= 0 {
			continue
		}
		a := get(name)
		a.score += reviewerWeightCoChange * n
		a.reasons = append(a.reasons, fmt.Sprintf("co-change expert across %s", plural(n, "related symbol")))
	}

	out := make([]ReviewerSuggestion, 0, len(merged))
	for name, a := range merged {
		kind := kinds[name]
		if kind == "" {
			kind = classifyReviewer(name)
		}
		mf := matchedFiles[name]
		if mf == nil {
			mf = []string{}
		} else {
			mf = append([]string(nil), mf...)
			sort.Strings(mf)
		}
		out = append(out, ReviewerSuggestion{
			Reviewer:     name,
			Kind:         kind,
			Score:        a.score,
			Reasons:      a.reasons,
			MatchedFiles: mf,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Reviewer < out[j].Reviewer
	})
	return out
}

// suggestReviewersPayload projects the ranked reviewers onto the
// suggest_reviewers wire shape.
func suggestReviewersPayload(reviewers []ReviewerSuggestion, changedFiles []string, codeownersFound bool) map[string]any {
	rows := make([]map[string]any, 0, len(reviewers))
	for _, r := range reviewers {
		reasons := r.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		matched := r.MatchedFiles
		if matched == nil {
			matched = []string{}
		}
		rows = append(rows, map[string]any{
			"reviewer":      r.Reviewer,
			"kind":          r.Kind,
			"score":         r.Score,
			"reasons":       reasons,
			"matched_files": matched,
		})
	}
	files := changedFiles
	if files == nil {
		files = []string{}
	}
	return map[string]any{
		"reviewers":        rows,
		"total":            len(rows),
		"changed_files":    len(files),
		"codeowners_found": codeownersFound,
	}
}

// normalizeReviewer canonicalises an owner handle / author email for use as a
// dedup key: trims whitespace and a leading "@" (CODEOWNERS syntax, not part of
// the identity) so `@core` and `core` collapse together, matching
// codeowners.TeamNodeID.
func normalizeReviewer(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "@")
}

// classifyReviewer mirrors the codeowners person/team split: a handle with a
// slash ("@org/team") is a team; an email or bare handle is a person.
func classifyReviewer(owner string) string {
	owner = strings.TrimPrefix(strings.TrimSpace(owner), "@")
	if strings.Contains(owner, "/") {
		return "team"
	}
	return "person"
}

// relForRepo returns a repo-root-relative, forward-slash path for CODEOWNERS
// matching. Graph file paths are already repo-relative; this only strips a
// leading repoRoot prefix on the off chance an absolute path slips through.
func relForRepo(file, repoRoot string) string {
	file = strings.TrimSpace(file)
	if repoRoot != "" {
		if rel := strings.TrimPrefix(file, strings.TrimRight(repoRoot, "/")+"/"); rel != file {
			return rel
		}
	}
	return file
}

// plural renders "1 file" / "3 files" — a tiny helper so reasons read
// naturally without an external dependency.
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
