package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/gitcmd"
)

// registerCitationTools wires verify_citation — the substring-grounded
// citation gate. Pairs with the gortex-pr-review skill's "did the
// agent's quoted spans actually exist at the SHA they claim?" step;
// can also run as a Stop-hook validator that catches hallucinated
// quotes before they reach the user.
func (s *Server) registerCitationTools() {
	s.addTool(
		mcp.NewTool("verify_citation",
			mcp.WithDescription("Verifies that a cited code span actually exists in `file_path` at the given git `sha`. Runs `git show <sha>:<file_path>` and checks for the verbatim substring (whitespace-significant). Returns {verified, sha_resolved, file_path, span_first_line, match_count, error?}. Use as a Stop-hook auto-validator or before quoting code into PR comments / postmortems."),
			mcp.WithString("span", mcp.Description("Verbatim code span the caller claims is in the file. Whitespace-sensitive — exactly as it should appear.")),
			mcp.WithString("file_path", mcp.Description("Repo-relative or absolute path to the file at the cited SHA.")),
			mcp.WithString("sha", mcp.Description("Git revision: full or abbreviated commit hash, branch, or tag. Defaults to HEAD.")),
			mcp.WithString("repo", mcp.Description("(Multi-repo only) repo prefix. Resolves which working tree to run git in. Inferred from `file_path` when the path is repo-prefixed.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleVerifyCitation,
	)
}

// handleVerifyCitation runs the substring check. Errors from git or
// missing files are reported in the `error` field with verified=false
// rather than as MCP errors — the caller wants the verdict, not an
// exception they have to catch.
func (s *Server) handleVerifyCitation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	span := req.GetString("span", "")
	if span == "" {
		return mcp.NewToolResultError("verify_citation requires `span`"), nil
	}
	rawPath := strings.TrimSpace(req.GetString("file_path", ""))
	if rawPath == "" {
		return mcp.NewToolResultError("verify_citation requires `file_path`"), nil
	}
	sha := strings.TrimSpace(req.GetString("sha", ""))
	if sha == "" {
		sha = "HEAD"
	}

	// Resolve the working tree this file lives in — via the repo prefix
	// on `file_path`, or the sole tracked repo's root for an unqualified
	// path. The shared resolveFilePath already encodes both rules and
	// rejects escapes.
	absPath, relPath, err := s.resolveFilePath(ctx, rawPath)
	if err != nil {
		return s.respondJSONOrTOON(ctx, req, citationFailure(sha, rawPath, fmt.Sprintf("resolve path: %v", err)))
	}

	workTree := repoRootContaining(absPath)
	if workTree == "" {
		return s.respondJSONOrTOON(ctx, req, citationFailure(sha, rawPath, "file is not inside a git working tree"))
	}

	// `git show` uses tree-relative paths. We rewrite the relPath to
	// be relative to the resolved git work tree — the indexer's relPath
	// is relative to the indexer root, which may differ from the git
	// root when the indexer is anchored on a subdirectory.
	gitRelPath, gitRelErr := filepath.Rel(workTree, absPath)
	if gitRelErr != nil {
		return s.respondJSONOrTOON(ctx, req, citationFailure(sha, relPath, fmt.Sprintf("rel(%s,%s): %v", workTree, absPath, gitRelErr)))
	}

	// Expand abbreviated / symbolic refs to a full hex SHA so the
	// caller can persist a stable citation. `git rev-parse --verify`
	// fails loudly when the ref doesn't resolve, which we surface as
	// verified=false rather than ambiguous output.
	fullSHA, shaErr := runGit(ctx, workTree, "rev-parse", "--verify", sha+"^{commit}")
	if shaErr != nil {
		return s.respondJSONOrTOON(ctx, req, citationFailure(sha, relPath, fmt.Sprintf("rev-parse %s: %v", sha, shaErr)))
	}
	fullSHA = strings.TrimSpace(fullSHA)

	content, showErr := runGit(ctx, workTree, "show", fullSHA+":"+filepath.ToSlash(gitRelPath))
	if showErr != nil {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"verified":         false,
			"sha":              sha,
			"sha_resolved":     fullSHA,
			"file_path":        relPath,
			"span_first_line":  0,
			"match_count":      0,
			"error":            fmt.Sprintf("git show: %v", showErr),
		})
	}

	first, count := substringMatchInfo(content, span)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"verified":         count > 0,
		"sha":              sha,
		"sha_resolved":     fullSHA,
		"file_path":        relPath,
		"span_first_line":  first,
		"match_count":      count,
	})
}

// citationFailure builds the result shape used when we can't even
// execute the substring check (path resolution failed, not a git
// tree, sha doesn't parse). Keeps the wire shape stable for callers.
func citationFailure(sha, filePath, msg string) map[string]any {
	return map[string]any{
		"verified":         false,
		"sha":              sha,
		"sha_resolved":     "",
		"file_path":        filePath,
		"span_first_line":  0,
		"match_count":      0,
		"error":            msg,
	}
}

// repoRootContaining walks up from path until it finds a directory
// holding a `.git` entry (file or dir — submodules use a file).
// Returns "" if no enclosing git tree exists.
func repoRootContaining(path string) string {
	dir := path
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		dir = filepath.Dir(path)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// substringMatchInfo returns the 1-based line of the first match and
// the total number of matches. Whitespace and newline-sensitive — a
// cited span that spans multiple lines must match exactly.
func substringMatchInfo(haystack, needle string) (firstLine, count int) {
	if needle == "" {
		return 0, 0
	}
	idx := strings.Index(haystack, needle)
	if idx < 0 {
		return 0, 0
	}
	firstLine = 1 + strings.Count(haystack[:idx], "\n")
	count = strings.Count(haystack, needle)
	return firstLine, count
}

// runGit executes `git <args...>` with cwd=dir and returns raw stdout.
// Routed through gitcmd (the concurrency-gated chokepoint) so it shares
// the process-wide git limiter. Bounded by the caller's request context —
// git invocations are expected to complete in milliseconds, but we still
// cancel on session teardown; when the ctx carries no deadline we bound it
// at 30s. Run returns raw stdout (callers TrimSpace themselves).
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	out, err := gitcmd.Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
