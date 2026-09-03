package mcp

// MCP entrypoint for the Temporal dispatch LLM verification pass.
//
// PURPOSE — make resolver.VerifyTemporalEdges reachable from the MCP `analyze`
// surface (the CLI host that originally drove it was removed; analyze is now
// MCP-only). `analyze kind=temporal_verify` runs the precision backstop over
// the active graph's low-confidence Temporal dispatch edges, promoting LLM-
// confirmed edges and suppressing rejected ones, and returns the canonical
// verify report.
// RATIONALE — the verification core (resolver) and its LLM/source adapters
// (analyzer) are injected interfaces; this handler is the only place that wires
// them to the server's live state — the shared LLM provider and the on-disk
// source resolved through the server's own multi-repo/worktree path logic.
// Reusing s.llmService.Provider() avoids constructing a second provider, and
// the source provider delegates to resolveNodePath so paths are correct in
// single-repo, multi-repo, and worktree layouts. With no LLM configured the
// handler returns a clear error instead of running (or panicking).
// KEYWORDS — analyze, temporal, verify, llm, mcp, precision

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analyzer"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// maxTemporalNodeSourceBytes caps the per-node source handed to the LLM so a
// giant function body can't blow the prompt budget. Source reading lives here
// because this handler resolves paths through the server's multi-repository and
// worktree-aware resolveNodePath logic.
const maxTemporalNodeSourceBytes = 6000

// serverSourceProvider implements resolver.TemporalSourceProvider by reading a
// node's source slice from disk via the server's own path resolution
// (resolveNodePath), so the verifier gets correct on-disk source under
// single-repo, multi-repo prefix, and linked-worktree layouts alike. Read file
// bodies are cached for the run.
type serverSourceProvider struct {
	s *Server
	// ctx is the request the provider was built for. The resolver interface
	// carries none, and path resolution needs one to place a path in the
	// checkout the request reads.
	ctx   context.Context
	cache map[string]string // absolute path -> file body ("" = unreadable)
}

func newServerSourceProvider(ctx context.Context, s *Server) *serverSourceProvider {
	return &serverSourceProvider{s: s, ctx: ctx, cache: map[string]string{}}
}

// NodeSource returns the source text of n's declaration, or ("", false).
func (p *serverSourceProvider) NodeSource(n *graph.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	abs, err := p.s.resolveNodePath(p.ctx, n)
	if err != nil || abs == "" {
		return "", false
	}
	body, ok := p.fileBody(abs)
	if !ok {
		return "", false
	}
	lines := strings.Split(body, "\n")
	start, end := n.StartLine, n.EndLine
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		return "", false
	}
	if end < start || end > len(lines) {
		end = len(lines)
	}
	src := strings.Join(lines[start-1:end], "\n")
	if len(src) > maxTemporalNodeSourceBytes {
		src = src[:maxTemporalNodeSourceBytes] + "\n// …truncated"
	}
	return src, true
}

func (p *serverSourceProvider) fileBody(abs string) (string, bool) {
	if b, ok := p.cache[abs]; ok {
		return b, b != ""
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		p.cache[abs] = ""
		return "", false
	}
	p.cache[abs] = string(raw)
	return string(raw), true
}

// handleAnalyzeTemporalVerify runs the LLM cleaning pass over the active
// graph's verifiable Temporal dispatch edges and returns the verify report
// (checked/confirmed/rejected/uncertain/errors/details/totals). It requires a
// configured LLM provider — with none, it returns a clear "LLM not configured"
// error result rather than silently no-op'ing or panicking.
func (s *Server) handleAnalyzeTemporalVerify(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.llmService == nil || !s.llmService.Enabled() {
		return mcp.NewToolResultError(
			"temporal_verify requires a configured LLM provider — set llm.provider " +
				"in .gortex.yaml (or GORTEX_LLM_PROVIDER) with a valid model / API key",
		), nil
	}
	provider := s.llmService.Provider()
	if provider == nil {
		return mcp.NewToolResultError("temporal_verify: LLM provider unavailable"), nil
	}

	inner := analyzer.NewLLMTemporalVerifierFromProvider(provider)
	// Disk-backed verdict cache so re-runs (e.g. in CI) don't re-pay the LLM.
	// The key includes the model and the actual caller / target source, so it
	// invalidates automatically when the model or the code changes.
	cachePath := ""
	roots := s.collectRepoRoots("")
	rootKeys := make([]string, 0, len(roots))
	for k := range roots {
		rootKeys = append(rootKeys, k)
	}
	sort.Strings(rootKeys)
	for _, k := range rootKeys {
		if roots[k] != "" {
			cachePath = filepath.Join(roots[k], ".gortex", "temporal-verify-cache.json")
			break
		}
	}
	verifier := analyzer.NewCachingVerifier(inner, provider.Name(), cachePath)
	src := newServerSourceProvider(ctx, s)
	report := resolver.VerifyTemporalEdges(ctx, s.graph, src, verifier)
	if err := verifier.Flush(); err != nil && s.logger != nil {
		s.logger.Warn("temporal_verify: verdict cache flush failed", zap.Error(err))
	}

	out := analyzer.VerifyReportToMap(report)

	if isCompact(req) {
		var b strings.Builder
		b.WriteString("checked: ")
		b.WriteString(strconv.Itoa(report.Checked))
		b.WriteString(", confirmed: ")
		b.WriteString(strconv.Itoa(report.Confirmed))
		b.WriteString(", rejected: ")
		b.WriteString(strconv.Itoa(report.Rejected))
		b.WriteString(", uncertain: ")
		b.WriteString(strconv.Itoa(report.Uncertain))
		b.WriteString(", errors: ")
		b.WriteString(strconv.Itoa(report.Errors))
		b.WriteByte('\n')
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, out)
}
