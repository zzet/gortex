package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/releases"
)

// registerEnrichReleasesTool exposes the releases enricher as an MCP
// tool. `analyze kind=releases` is now a pure read — populating the
// per-file meta.added_in and the KindRelease timeline is this tool's
// job (counterpart to enrich_churn).
//
// Branch constrains the considered tags to those reachable from the
// branch — typically the repo's default branch — so topic-branch tags
// don't pollute the timeline. Empty branch means "every tag", matching
// the legacy behaviour.
func (s *Server) registerEnrichReleasesTool() {
	s.addTool(
		mcp.NewTool("enrich_releases",
			mcp.WithDescription("Pre-compute the release timeline: list tags on the default branch (or `branch` override), stamp meta.added_in on every file present in each tag's tree, and materialise one KindRelease node per tag. The read tool `analyze kind=releases` then answers from this Meta without re-walking git. Idempotent; disk-backed daemons (sqlite) persist the result across restarts."),
			mcp.WithString("branch", mcp.Description("Branch / tag / SHA whose reachable tag set bounds the timeline. Empty resolves the repo's default branch; pass a value to override.")),
			mcp.WithString("path", mcp.Description("Optional path or repo prefix to scope the enrichment. Multi-repo daemons enrich every tracked repo when empty.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleEnrichReleases,
	)
}

func (s *Server) handleEnrichReleases(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("graph not initialized"), nil
	}
	branchArg := strings.TrimSpace(req.GetString("branch", ""))
	pathArg := strings.TrimSpace(req.GetString("path", ""))

	type target struct {
		prefix string
		root   string
	}
	var targets []target
	// A request that reads a checkout of its own covers NO targets: its
	// generation is published and has no writable output, so it is refused
	// below rather than sweeping the tracked repositories on its behalf. Every
	// other request covers the corpus. See enrichmentTargets.
	for prefix, root := range s.enrichmentTargets(ctx, "") {
		if pathArg != "" && pathArg != prefix && pathArg != root {
			continue
		}
		targets = append(targets, target{prefix: prefix, root: root})
	}
	if len(targets) == 0 {
		if requestViewFromContext(ctx).readsOwnCheckout() {
			return mcp.NewToolResultError(ErrEnrichmentSnapshotNotWritable.Error() + ": release enrichment"), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("no tracked repo matches %q", pathArg)), nil
	}

	started := time.Now()
	type perRepo struct {
		Prefix  string `json:"prefix"`
		Branch  string `json:"branch,omitempty"`
		Files   int    `json:"files"`
		Skipped string `json:"skipped,omitempty"`
		// Superseded: a newer release run took the authority over while this
		// one ran. Its stamps are already on the graph, so Files above is real
		// — this is an ordering statement, not a skip.
		Superseded bool `json:"superseded,omitempty"`
	}
	var per []perRepo
	totalFiles := 0
	// One answer-level generation for the whole run: every target a corpus
	// enrichment admits names generation zero, so it is set from the first
	// admitted output rather than re-assigned per repository (which would make
	// the field read "whatever the last repository named").
	generation := int64(0)
	generationNamed := false
	for _, t := range targets {
		b := branchArg
		if b == "" {
			b = churn.DefaultBranch(t.root)
			// b can stay "" — releases.EnrichGraphForBranch treats
			// that as "every tag", the right fallback when no default
			// branch resolves.
		}
		out, err := s.beginEnrichmentOutput(ctx, EnrichProducerReleases, t.prefix, t.root)
		if err != nil {
			return mcp.NewToolResultError("release enrichment: " + err.Error()), nil
		}
		count, err := releases.EnrichGraphForBranch(out.Store, out.Root, t.prefix, b)
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
		per = append(per, perRepo{Prefix: t.prefix, Branch: b, Files: count, Superseded: superseded})
		totalFiles += count
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"repos":       per,
		"files":       totalFiles,
		"generation":  generation,
		"duration_ms": time.Since(started).Milliseconds(),
	})
}
