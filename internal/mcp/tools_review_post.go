package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/review"
)

// postReviewFindings is the package-level seam over review.PostFindings. It
// defaults to the real (forge-network-backed) post; a test swaps it for a
// closure that records the call and returns a canned result, so the post_review
// handler is exercised end-to-end with no network. Keep this the only
// indirection over the post call.
var postReviewFindings = review.PostFindings

// handlePostReview runs (or accepts) a review for a pull request and posts the
// findings as inline review comments. It honours the secret-redaction and
// public-repo gates in review.PostFindings:
//
//   - every comment body is run through RedactSecrets before any payload is
//     built or any request is sent;
//   - posting to a public / fork PR requires allow_public (config or the flag);
//   - dry_run returns the would-post (redacted) payloads without a network call.
//
// Findings are sourced either from an explicit `findings` JSON array (the gated
// findings from a prior review / review_pack call) or, when absent, by running
// the deterministic review rulepack over the changeset / pasted diff.
func (s *Server) handlePostReview(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	number := req.GetInt("number", 0)
	if number <= 0 {
		return mcp.NewToolResultError("number is required (the PR / MR number to post on)"), nil
	}

	repo := strings.TrimSpace(req.GetString("repo", ""))
	repoRoot, _ := s.diffRepoScope(ctx, repo)

	findings, err := s.postReviewFindingsFor(ctx, req, repoRoot)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	target := review.PostTarget{
		Provider: strings.TrimSpace(req.GetString("provider", "github")),
		Owner:    strings.TrimSpace(req.GetString("owner", "")),
		Repo:     strings.TrimSpace(req.GetString("repo_name", "")),
		PRNumber: number,
		// Public / fork status is caller-supplied (the forge-identity lookup that
		// would derive it is a network call out of scope here). Default false →
		// the public-repo gate refuses unless the caller opts in.
		Public: req.GetBool("public", false),
	}

	opts := review.NewPostOptions()
	opts.DryRun = req.GetBool("dry_run", false)
	opts.Summary = strings.TrimSpace(req.GetString("summary", ""))
	opts.AsSingleReview = true
	// allow_public defaults to the configured review.post.allow_public, overridden
	// to true by an explicit confirm_public.
	opts.AllowPublic = s.reviewAllowPublic(repo) || req.GetBool("confirm_public", false)
	// refuse_on_secret defaults on (drop a finding that quoted a secret); a caller
	// can opt to post the redacted version instead.
	opts.RefuseOnSecret = req.GetBool("refuse_on_secret", true)

	res, err := postReviewFindings(ctx, repoRoot, target, findings, opts)
	if err != nil {
		// The public-repo gate is a structured refusal, not a tool failure.
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"error":  err.Error(),
			"posted": res.Posted,
		})
	}

	payload := map[string]any{
		"posted":   res.Posted,
		"skipped":  res.Skipped,
		"redacted": res.Redacted,
		"dry_run":  opts.DryRun,
		"total":    len(findings),
	}
	if res.ReviewURL != "" {
		payload["review_url"] = res.ReviewURL
	}
	if opts.DryRun {
		payload["payloads"] = res.Payloads
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// reviewAllowPublic reads the configured review.post.allow_public gate for the
// given repo prefix. No config manager (or no review block) reports false — the
// safe default so a misconfigured token never leaks comments to a public PR.
func (s *Server) reviewAllowPublic(repo string) bool {
	if s.configManager == nil {
		return false
	}
	cfg := s.configManager.GetRepoConfig(repo)
	if cfg == nil {
		return false
	}
	return cfg.Review.Post.AllowPublic
}

// postReviewFindingsFor resolves the findings to post: an explicit `findings`
// JSON array (gated findings from a prior review call) takes precedence;
// otherwise the deterministic review rulepack runs over the changeset / pasted
// diff and its report findings are used.
func (s *Server) postReviewFindingsFor(ctx context.Context, req mcp.CallToolRequest, repoRoot string) ([]review.Finding, error) {
	if raw := strings.TrimSpace(req.GetString("findings", "")); raw != "" {
		var findings []review.Finding
		if err := json.Unmarshal([]byte(raw), &findings); err != nil {
			return nil, fmt.Errorf("invalid findings JSON: %v", err)
		}
		return findings, nil
	}

	diffText := strings.TrimSpace(req.GetString("diff", ""))
	scope, baseRef := siblingDiffScope(req)
	if repoRoot == "" && diffText == "" {
		return nil, fmt.Errorf("no findings supplied and no changeset to review (set `findings` or a repo / diff)")
	}

	var (
		rulepack []astquery.Match
		impact   map[string]*analysis.ImpactResult
	)
	repoPrefix := s.diffJoinPrefix(repoRoot)
	var changedFiles []string
	if diffText == "" {
		diff, err := analysis.MapGitDiff(s.graph, repoRoot, repoPrefix, scope, baseRef)
		if err != nil {
			return nil, err
		}
		allowedRepos, err := s.resolveRepoFilter(ctx, req)
		if err != nil {
			return nil, err
		}
		rulepack = s.reviewRulepackMatches(ctx, diff.ChangedFiles, analysis.RepoRelativePath, repoPrefix, allowedRepos)
		impact = s.reviewImpact(diff.ChangedSymbols)
		changedFiles = diff.ChangedFiles
	}

	suppStore, suppRepoKey := s.reviewSuppressions()
	report, err := review.Run(ctx, s.graph, nil, review.Options{
		RepoRoot:        repoRoot,
		RepoPrefix:      repoPrefix,
		CoverageKnown:   s.coverageKnownForDiff(repoPrefix, changedFiles),
		Scope:           scope,
		BaseRef:         baseRef,
		Diff:            diffText,
		RulepackMatches: rulepack,
		Impact:          impact,
		Suppressions:    suppStore,
		RepoKey:         suppRepoKey,
	})
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, nil
	}
	return report.Findings, nil
}
