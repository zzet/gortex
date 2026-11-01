package mcp

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/indexer"
)

var graphFreshnessReceiptID = regexp.MustCompile(`^mutation-\d+$`)

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
			if payload, handled := s.moveFileObservationPayload(ctx, receipt); handled {
				return s.respondJSONOrTOON(ctx, req, payload)
			}
			if payload, handled := s.batchAcknowledgementReceiptPayload(ctx, receipt); handled {
				return s.respondJSONOrTOON(ctx, req, payload)
			}
			if payload, handled := s.batchFileObservationPayload(ctx, receipt); handled {
				return s.respondJSONOrTOON(ctx, req, payload)
			}
			if payload, found := s.graphRefreshReceiptPayload(ctx, receipt); found {
				return s.respondJSONOrTOON(ctx, req, payload)
			}
		}
		if !ok || !mutationCommitInScope(ctx, record) {
			return mcp.NewToolResultError(
				"no mutation receipt " + receipt + " — receipts are kept for " + mutationCommitRetention.String() +
					"; query by path instead, or read the file to see its current state"), nil
		}
		return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))

	case mutationID != "":
		record, ok := s.mutationCommits.byMutationID(mutationID)
		if !ok && graphFreshnessReceiptID.MatchString(mutationID) {
			if payload, found := s.graphRefreshReceiptPayload(ctx, mutationID); found {
				payload["note"] = "this is a graph freshness receipt id, not a caller mutation_id"
				return s.respondJSONOrTOON(ctx, req, payload)
			}
			return mcp.NewToolResultError("no mutation recorded for mutation_id " + mutationID +
				" — use change.receipt options.receipt=" + mutationID + " to query a graph freshness receipt"), nil
		}
		if !ok || !mutationCommitInScope(ctx, record) {
			return mcp.NewToolResultError("no mutation recorded for mutation_id " + mutationID), nil
		}
		return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))

	case rawPath != "":
		// Path lookup is the last resort a client has when it lost the whole
		// response and therefore never saw a receipt id. Resolution failure is
		// not fatal here: the raw spelling is still matched against the ledger.
		lookup := rawPath
		if absPath, relPath, err := s.resolveFilePath(ctx, rawPath); err == nil {
			if record, ok := s.recentMutationCommitInScope(ctx, relPath); ok {
				return s.respondJSONOrTOON(ctx, req, s.mutationStatusPayload(record))
			}
			lookup = absPath
		}
		record, ok := s.recentMutationCommitInScope(ctx, lookup)
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

	var records []*mutationCommitRecord
	for _, record := range s.mutationCommits.recent(maxMutationCommits) {
		if mutationCommitInScope(ctx, record) {
			s.refreshPendingSourceRecord(record)
			records = append(records, record)
			if len(records) == maxMutationCommitListing {
				break
			}
		}
	}
	// Rendered through the same flag as the single-record path. A listing that
	// showed graph_status without graph_status_terminal would hand an agent
	// exactly the reading this tool is trying to stop, just by a different
	// entry point.
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"mutations": mutationCommitListingPayload(records),
		"count":     len(records),
		"note":      "most recent first; pass receipt, mutation_id, or path to select one. Read graph_status_terminal before waiting on graph_status",
	})
}

// Recovery reindexes have a publication ticket but no disk commit. Accept that
// same ticket ID through change.receipt without inventing a write verdict.
func (s *Server) graphRefreshReceiptPayload(ctx context.Context, id string) (map[string]any, bool) {
	value, ok := s.mutationReceipts.Load(id)
	if !ok {
		return nil, false
	}
	receipt, ok := value.(*mutationReceipt)
	if !ok {
		return nil, false
	}
	checkoutID, incarnation := mutationCheckoutScope(ctx)
	if !mutationCheckoutScopeMatches(checkoutID, incarnation, receipt.checkoutID, receipt.checkoutIncarnation) {
		return nil, false
	}
	outcome := receipt.outcome(true)
	status := graphStatusFor(outcome)
	payload := map[string]any{
		"found":                 true,
		"receipt":               receipt.id,
		"receipt_kind":          "graph_refresh",
		"path":                  receipt.path,
		"disk_status":           "unrecorded",
		"graph_status_terminal": !outcome.Pending,
		"graph_note":            mutationGraphStatusNote(status, true, receipt.checkoutID),
		"guidance":              "this receipt tracks graph publication only; use the mutation_receipt for disk commit evidence",
	}
	// This endpoint has no source-file syntax-health target, including for a
	// canonical watcher receipt. Only render the publication evidence.
	outcome.checkoutScoped = true
	s.attachMutationFreshness(payload, "", "", outcome)
	if receipt.checkoutID != "" {
		payload["checkout_id"] = receipt.checkoutID
		payload["checkout_incarnation"] = receipt.checkoutIncarnation
	}
	if outcome.Err != nil {
		payload["error"] = outcome.Err.Error()
	}
	var retained *indexer.PublicationPhaseRecord
	var acknowledgement *editAcknowledgementSnapshot
	acknowledgementState := "not_applicable"
	if commit, found := s.mutationCommits.byFreshnessReceipt(receipt.id); found && mutationCommitInScope(ctx, commit) {
		acknowledgementState = "unavailable_recording_or_evicted"
		retained, _ = retainedMutationPublication(commit)
		if ack := retainedMutationAcknowledgement(commit); ack != nil {
			snapshot := ack.snapshot()
			acknowledgement = &snapshot
		}
		if retained != nil && retained.Key() != receipt.id {
			retained = nil
		}
	}
	attachMutationPhaseTimings(payload, receipt.id, time.Time{}, indexer.PublicationSourceFreshRequest, retained)
	attachAcknowledgementPayload(payload, acknowledgement, acknowledgementState)
	return payload, true
}

func mutationCommitInScope(ctx context.Context, record *mutationCommitRecord) bool {
	if !record.isPhysicalCommit() {
		return false
	}
	checkoutID, incarnation := mutationCheckoutScope(ctx)
	record.mu.RLock()
	defer record.mu.RUnlock()
	return mutationCheckoutScopeMatches(checkoutID, incarnation, record.checkoutID, record.checkoutIncarnation)
}

func (s *Server) recentMutationCommitInScope(ctx context.Context, path string) (*mutationCommitRecord, bool) {
	for _, record := range s.mutationCommits.recent(maxMutationCommits) {
		if !mutationCommitInScope(ctx, record) {
			continue
		}
		record.mu.RLock()
		match := record.relPath == path || record.absPath == path
		record.mu.RUnlock()
		if match {
			return record, true
		}
	}
	return nil, false
}

// mutationStatusPayload renders one record, refreshing the graph half if the
// reindex it was waiting on has since finished. Without the refresh every
// pending receipt would read as pending forever, which is precisely the stale
// answer this tool exists to avoid.
func (s *Server) mutationStatusPayload(record *mutationCommitRecord) map[string]any {
	if !record.isPhysicalCommit() {
		return map[string]any{"found": false, "reason": "not_physical_commit"}
	}
	s.refreshPendingSourceRecord(record)
	snap := record.snapshot()
	payload := map[string]any{
		"found":        true,
		"receipt":      snap.Receipt,
		"tool":         snap.Tool,
		"path":         snap.Path,
		"disk_status":  snap.DiskStatus,
		"graph_status": snap.GraphStatus,
	}
	attachMutationRefreshSnapshot(payload, snap)
	if snap.readRepair != nil {
		payload["read_assisted_publication"] = snap.readRepair.payload()
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
	payload["graph_status_terminal"] = graphStatusTerminal(snap.GraphStatus, snap.GraphRecorded)
	s.attachRecordDerivedFanout(payload, record, snap)
	if snap.Reason == "receipt_lost" {
		payload["graph_note"] = "publication outcome is unavailable because its freshness receipt was lost; this does not establish graph freshness"
	} else if note := mutationGraphStatusNote(snap.GraphStatus, snap.GraphRecorded, snap.CheckoutID); note != "" {
		payload["graph_note"] = note
	}
	payload["guidance"] = mutationStatusGuidance(snap.DiskStatus)
	retained, committedAt := retainedMutationPublication(record)
	attachMutationPhaseTimings(payload, snap.ReindexReceipt, committedAt, indexer.PublicationSourceMCPEdit, retained)
	var acknowledgement *editAcknowledgementSnapshot
	if ack := retainedMutationAcknowledgement(record); ack != nil {
		snapshot := ack.snapshot()
		acknowledgement = &snapshot
	}
	attachAcknowledgementPayload(payload, acknowledgement, "unavailable_recording_or_evicted")
	return payload
}

// attachRecordDerivedFanout publishes the bounded-derived-pass completeness
// fact beside graph_status_terminal.
//
// It is the answer to the question graph_status cannot answer. A caller whose
// edit response was lost comes here to learn two things — did the bytes land,
// and has the graph caught up — and "caught up" has always silently meant "the
// file was re-parsed", never "its dependants were rebound". When the
// affected-by bound fires, graph_status reads "fresh" over a graph that is
// knowingly incoherent for the files the pass dropped, and until now nothing
// on this payload said so.
//
// The fact is read from the freshness receipt rather than from the ledger
// record: the record keeps the generation the reindex ran under, which is the
// handle that finds it (see derivedFanoutForMutation). A record whose receipt
// has aged out, or one from a path the watcher never measured, renders
// nothing — an absent verdict, never an invented one.
func (s *Server) attachRecordDerivedFanout(
	payload map[string]any,
	record *mutationCommitRecord,
	snap mutationCommitSnapshot,
) {
	if payload == nil || record == nil || !snap.GraphRecorded {
		return
	}
	record.mu.RLock()
	absPath := record.absPath
	record.mu.RUnlock()
	fanout, ok := s.derivedFanoutForMutation(absPath, snap.ReindexGeneration)
	if !ok {
		return
	}
	attachDerivedFanout(payload, fanout)
}

func mutationGraphStatusNote(graph string, recorded bool, checkoutID string) string {
	if recorded && checkoutID != "" && graph == mutationGraphFailed {
		return "publication of the original checkout content failed terminally; inspect the error and the current exact checkout view. A different branch or later edit cannot certify this receipt; request a new scoped refresh if needed"
	}
	return graphStatusNote(graph, recorded)
}

func attachMutationRefreshSnapshot(payload map[string]any, snap mutationCommitSnapshot) {
	if snap.Reason != "" {
		payload["reason"] = snap.Reason
	}
	if snap.ReindexReceipt != "" {
		payload["reindex_receipt"] = snap.ReindexReceipt
	}
	if snap.ReindexGeneration != 0 {
		payload["reindex_generation"] = snap.ReindexGeneration
	}
	if snap.AppliedGeneration != 0 {
		payload["applied_generation"] = snap.AppliedGeneration
	}
	if snap.CheckoutID != "" {
		payload["checkout_id"] = snap.CheckoutID
	}
	if snap.CheckoutIncarnation != "" {
		payload["checkout_incarnation"] = snap.CheckoutIncarnation
	}
}

// graphStatusTerminal answers the only question a caller actually has about
// the graph half: is it still worth waiting? Once an outcome is recorded only
// "pending" can resolve on its own. A lost pending receipt settles as failed;
// an explicitly recorded terminal stale outcome remains historical evidence.
// Rendering the four values without this flag makes a terminal one look like a
// stage in a progression, and a caller that waits on it waits forever.
//
// recorded is not a refinement, it is the other half of the answer.
// beginMutationCommit seeds graph as "stale" and nothing reports back until
// the mutation completes, so an edit still in flight renders exactly like one
// whose reindex finished without confirming a write. Deriving terminality from
// the string alone would tell the caller this tool exists to serve — one whose
// edit was abandoned at its deadline and may still be completing — to stop
// waiting on a value that is about to become "fresh". That is the failure this
// flag is meant to prevent, pointing the other way.
func graphStatusTerminal(graph string, recorded bool) bool {
	return recorded && graph != mutationGraphPending
}

// graphStatusNote says what the value means and what would change it. The
// wording matters most for "stale" and "failed", which are the two a caller
// is most likely to sit on.
func graphStatusNote(graph string, recorded bool) string {
	if !recorded {
		return "the reindex for this mutation has not reported back yet, so this value is a seed rather than an outcome — poll this receipt; it is not settled"
	}
	switch graph {
	case mutationGraphPending:
		return "the reindex for this mutation is still running — poll this receipt; this is the one graph_status that resolves on its own"
	case mutationGraphFresh:
		return "the graph has read these bytes"
	case mutationGraphStale:
		return "the reindex reported back without confirming an index write. This will NOT become \"fresh\" on its own. It also does not gate change.detect — that barrier reads the freshness receipts, not this ledger. The bytes are on disk: verify them and proceed"
	case mutationGraphFailed:
		return "the graph ingest failed terminally. Waiting will not change it. A later successful mutation of this path, or a scoped reindex of it, resolves the freshness receipt that does gate change.detect"
	default:
		return ""
	}
}

func mutationStatusGuidance(disk string) string {
	switch disk {
	case mutationDiskCommitted:
		return "the bytes are on disk — do not re-apply this edit; read graph_status_terminal before waiting on graph_status, because only \"pending\" resolves on its own"
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

// mutationCommitListingPayload renders the listing with the same graph
// terminality the single-record payload carries. The listing handler refreshes
// selected records first; this renderer only formats their current snapshots.
func mutationCommitListingPayload(records []*mutationCommitRecord) []map[string]any {
	snaps := mutationCommitListing(records)
	out := make([]map[string]any, 0, len(snaps))
	for _, snap := range snaps {
		entry := map[string]any{
			"receipt":               snap.Receipt,
			"disk_status":           snap.DiskStatus,
			"graph_status":          snap.GraphStatus,
			"graph_status_terminal": graphStatusTerminal(snap.GraphStatus, snap.GraphRecorded),
		}
		attachMutationRefreshSnapshot(entry, snap)
		// Every remaining field mirrors mutationCommitSnapshot's omitempty
		// exactly. Rendering by hand is what dropped new_sha and
		// bytes_written the first time, so the parity is asserted against the
		// struct itself rather than trusted here — see
		// TestMutationCommitListingKeepsEverySnapshotField.
		if snap.Tool != "" {
			entry["tool"] = snap.Tool
		}
		if snap.Path != "" {
			entry["path"] = snap.Path
		}
		if snap.MutationID != "" {
			entry["mutation_id"] = snap.MutationID
		}
		if snap.NewSHA != "" {
			entry["new_sha"] = snap.NewSHA
		}
		if snap.BytesWritten != 0 {
			entry["bytes_written"] = snap.BytesWritten
		}
		if snap.Error != "" {
			entry["error"] = snap.Error
		}
		if snap.StartedAt != "" {
			entry["started_at"] = snap.StartedAt
		}
		if snap.CommittedAt != "" {
			entry["committed_at"] = snap.CommittedAt
		}
		out = append(out, entry)
	}
	return out
}
