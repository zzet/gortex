package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/churn"
)

// registerEnrichChurnTool exposes the churn enricher as an MCP tool so
// agents (and the post-commit / post-merge git hook driving `gortex
// enrich churn`) can refresh per-symbol churn data without going
// through the daemon control socket. The handler runs the enricher
// in-process against s.graph, so it inherits whatever backend the
// daemon was launched with — the on-disk backend for persistence, in-memory for
// CI / one-off invocations.
//
// The accompanying `get_churn_rate` tool reads from the same
// meta.churn fields this tool writes; pre-computation here is what
// makes the read path a sub-second graph scan.
func (s *Server) registerEnrichChurnTool() {
	s.addTool(
		mcp.NewTool("enrich_churn",
			mcp.WithDescription("Pre-compute per-file and per-symbol git churn data and stamp it on graph nodes so `get_churn_rate` can answer without a git subprocess. Walks `git log <branch>` and `git blame <branch>` once per file, then projects line-range commit counts onto every function/method node. The branch is the repository's default branch (origin/main, then origin/master, then local main/master/trunk) unless `branch` overrides. Idempotent: re-running updates the same Meta fields in place. Disk-backed daemons (sqlite) persist the result across restarts; in-memory daemons recompute on next call."),
			mcp.WithString("branch", mcp.Description("Branch / tag / SHA to compute churn against. Empty means resolve the repository's default branch.")),
			mcp.WithString("path", mcp.Description("Optional path or repo prefix to scope the enrichment. Multi-repo daemons enrich every tracked repo when empty.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleEnrichChurn,
	)
}

func (s *Server) handleEnrichChurn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("graph not initialized"), nil
	}
	branch := strings.TrimSpace(req.GetString("branch", ""))
	pathArg := strings.TrimSpace(req.GetString("path", ""))

	// Resolve targets: one repo root per tracked repo, optionally
	// filtered by path (matched as either prefix or absolute root).
	type target struct {
		prefix string
		root   string
	}
	var targets []target
	// A routed request enriches exactly one target — its own checkout — and an
	// identity with no writable generation enriches none. enrichmentTargets is
	// the single place that decision is made; the multi-repo sweep below is the
	// unrouted shape it falls back to.
	for prefix, root := range s.enrichmentTargets(ctx, "") {
		if pathArg != "" && pathArg != prefix && pathArg != root {
			continue
		}
		targets = append(targets, target{prefix: prefix, root: root})
	}
	if len(targets) == 0 {
		if requestViewFromContext(ctx).readsOwnCheckout() {
			return mcp.NewToolResultError(ErrEnrichmentSnapshotNotWritable.Error() + ": churn enrichment"), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("no tracked repo matches %q", pathArg)), nil
	}

	started := time.Now()
	type perRepo struct {
		Prefix  string `json:"prefix"`
		Branch  string `json:"branch"`
		HeadSHA string `json:"head_sha"`
		Files   int    `json:"files"`
		Symbols int    `json:"symbols"`
		Skipped string `json:"skipped,omitempty"`
		// Superseded reports that a newer churn run for this output took the
		// authority over while this one ran. The stamps this run wrote are on
		// the graph — the producer finished before it settled — so the counts
		// above are real and this is an ordering statement, not a skip.
		Superseded bool `json:"superseded,omitempty"`
	}
	var per []perRepo
	totalFiles, totalSymbols := 0, 0
	// One answer-level generation for the whole run: every target a corpus
	// enrichment admits names generation zero, so it is set from the first
	// admitted output rather than re-assigned per repository (which would make
	// the field read "whatever the last repository named").
	generation := int64(0)
	generationNamed := false
	for _, t := range targets {
		b := branch
		if b == "" {
			b = churn.DefaultBranch(t.root)
		}
		if b == "" {
			per = append(per, perRepo{Prefix: t.prefix, Skipped: "no default branch resolvable"})
			continue
		}
		out, err := s.beginEnrichmentOutput(ctx, EnrichProducerChurn, t.prefix, t.root)
		if err != nil {
			return mcp.NewToolResultError("churn enrichment: " + err.Error()), nil
		}
		res, err := churn.EnrichGraph(ctx, out.Store, out.Root, churn.Options{Branch: b})
		if err != nil {
			out.Abandon()
			per = append(per, perRepo{Prefix: t.prefix, Branch: b, Skipped: err.Error()})
			continue
		}
		superseded, serr := out.Settle()
		if serr != nil {
			per = append(per, perRepo{Prefix: t.prefix, Branch: b, Skipped: serr.Error()})
			continue
		}
		if !generationNamed {
			generation, generationNamed = out.Generation, true
		}
		per = append(per, perRepo{
			Prefix: t.prefix, Branch: res.Branch, HeadSHA: res.HeadSHA,
			Files: res.Files, Symbols: res.Symbols, Superseded: superseded,
		})
		totalFiles += res.Files
		totalSymbols += res.Symbols
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"repos":       per,
		"files":       totalFiles,
		"symbols":     totalSymbols,
		"generation":  generation,
		"duration_ms": time.Since(started).Milliseconds(),
	})
}
