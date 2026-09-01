package mcp

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleMutationStatus answers "what actually happened to my edit?" after a
// response was lost — the recovery path for a tool call the deadline wrapper
// abandoned (see mutation_commit.go).
//
// It is read-only. Its whole job is to turn the two unknowns a timed-out client
// is left holding into two independent facts: whether the bytes reached disk,
// and whether the graph has caught up with them.
func (s *Server) handleMutationStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	receipt := strings.TrimSpace(req.GetString("receipt", ""))
	mutationID := strings.TrimSpace(req.GetString("mutation_id", ""))
	rawPath := strings.TrimSpace(req.GetString("path", ""))

	switch {
	case receipt != "":
		record, ok := s.mutationCommits.byReceipt(receipt)
		if !ok {
			return mcp.NewToolResultError(
				"no mutation receipt " + receipt + " — receipts are kept for " + mutationCommitRetention.String() +
					"; query by path instead, or read the file to see its current state"), nil
		}
		return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))

	case mutationID != "":
		record, ok := s.mutationCommits.byMutationID(mutationID)
		if !ok {
			return mcp.NewToolResultError("no mutation recorded for mutation_id " + mutationID), nil
		}
		return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))

	case rawPath != "":
		// Path lookup is the last resort a client has when it lost the whole
		// response and therefore never saw a receipt id. Resolution failure is
		// not fatal here: the raw spelling is still matched against the ledger.
		lookup := rawPath
		if absPath, relPath, err := s.resolveFilePath(rawPath); err == nil {
			if record, ok := s.mutationCommits.recentForPath(relPath); ok {
				return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))
			}
			lookup = absPath
		}
		record, ok := s.mutationCommits.recentForPath(lookup)
		if !ok {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"path":        rawPath,
				"found":       false,
				"disk_status": "unrecorded",
				"note": "this daemon has no record of a mutation to that path — it was never attempted, " +
					"it was applied by another process, or the receipt has aged out",
			})
		}
		return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))
	}

	records := s.mutationCommits.recent(maxMutationCommitListing)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"mutations": mutationCommitListing(records),
		"count":     len(records),
		"note":      "most recent first; pass receipt, mutation_id, or path to select one",
	})
}

// mutationStatusPayload renders one record, refreshing the graph half if the
// reindex it was waiting on has since finished. Without the refresh every
// pending receipt would read as pending forever, which is precisely the stale
// answer this tool exists to avoid.
func (s *Server) mutationStatusPayload(record *mutationCommitRecord) map[string]any {
	if pending := record.pendingReindexReceipt(); pending != "" {
		if outcome, ok := s.mutationReceiptState(pending); ok {
			record.recordGraph(outcome)
		}
	}
	snap := record.snapshot()
	payload := map[string]any{
		"found":        true,
		"receipt":      snap.Receipt,
		"tool":         snap.Tool,
		"path":         snap.Path,
		"disk_status":  snap.DiskStatus,
		"graph_status": snap.GraphStatus,
	}
	if snap.MutationID != "" {
		payload["mutation_id"] = snap.MutationID
	}
	if snap.NewSHA != "" {
		payload["new_sha"] = snap.NewSHA
	}
	if snap.BytesWritten > 0 {
		payload["bytes_written"] = snap.BytesWritten
	}
	if snap.Error != "" {
		payload["error"] = snap.Error
	}
	if snap.StartedAt != "" {
		payload["started_at"] = snap.StartedAt
	}
	if snap.CommittedAt != "" {
		payload["committed_at"] = snap.CommittedAt
	}
	payload["retry_safe"] = snap.DiskStatus == mutationDiskNotApplied || snap.DiskStatus == mutationDiskFailed
	payload["guidance"] = mutationStatusGuidance(snap.DiskStatus)
	return payload
}

func mutationStatusGuidance(disk string) string {
	switch disk {
	case mutationDiskCommitted:
		return "the bytes are on disk — do not re-apply this edit; if graph_status is not \"fresh\" the graph is still catching up"
	case mutationDiskNotApplied:
		return "nothing was written — the file is unchanged and retrying is safe"
	case mutationDiskFailed:
		return "the write was attempted and failed — the file still holds its previous content, so retrying is safe"
	case mutationDiskInFlight:
		return "the write has not reported back yet — poll this receipt rather than retrying"
	default:
		return ""
	}
}
