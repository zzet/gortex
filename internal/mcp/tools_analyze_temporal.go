package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/resolver"
)

// handleAnalyzeTemporalOrphans is the queryable face of the Temporal
// call-graph integrity check: broken dispatches (a workflow calls an
// activity/child-workflow that resolves to nothing), signals/queries with
// no handler, and registered activities/workflows nobody dispatches or
// starts. Exposed as `analyze kind=temporal_orphans`.
func (s *Server) handleAnalyzeTemporalOrphans(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// DetectTemporalOrphans takes a graph.Store, so the integrity walk itself
	// runs on the base store; the row gates below read through the request
	// reader, which drops entries whose subject the caller's buffers deleted.
	rep := resolver.DetectTemporalOrphans(s.graph)
	if s.scopeFiltersActive(ctx) {
		reader := s.readerFor(ctx)
		// Narrow each integrity-gap list to entries whose subject node is
		// visible to the request (workspace ceiling + optional repo
		// allow-set); the inline response below then recomputes totals
		// from the filtered lengths. TemporalOrphan.From and the
		// OrphanActivity / OrphanWorkflow strings are the node IDs; Name
		// is a Temporal contract name (not a node), so it never leaks.
		// Filtered at the MCP layer only — the resolver stays
		// scope-agnostic. Unbound requests skip this branch — a no-op.
		keepOrphans := func(in []resolver.TemporalOrphan) []resolver.TemporalOrphan {
			out := make([]resolver.TemporalOrphan, 0, len(in))
			for _, o := range in {
				if s.analyzeNodeVisible(ctx, reader.GetNode(o.From)) {
					out = append(out, o)
				}
			}
			return out
		}
		keepIDs := func(in []string) []string {
			out := make([]string, 0, len(in))
			for _, id := range in {
				if s.analyzeNodeVisible(ctx, reader.GetNode(id)) {
					out = append(out, id)
				}
			}
			return out
		}
		rep.BrokenDispatch = keepOrphans(rep.BrokenDispatch)
		rep.SignalNoHandler = keepOrphans(rep.SignalNoHandler)
		rep.QueryNoHandler = keepOrphans(rep.QueryNoHandler)
		rep.OrphanActivity = keepIDs(rep.OrphanActivity)
		rep.OrphanWorkflow = keepIDs(rep.OrphanWorkflow)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"broken_dispatch":   rep.BrokenDispatch,
		"signal_no_handler": rep.SignalNoHandler,
		"query_no_handler":  rep.QueryNoHandler,
		"orphan_activity":   rep.OrphanActivity,
		"orphan_workflow":   rep.OrphanWorkflow,
		"totals": map[string]int{
			"broken_dispatch":   len(rep.BrokenDispatch),
			"signal_no_handler": len(rep.SignalNoHandler),
			"query_no_handler":  len(rep.QueryNoHandler),
			"orphan_activity":   len(rep.OrphanActivity),
			"orphan_workflow":   len(rep.OrphanWorkflow),
		},
	})
}
