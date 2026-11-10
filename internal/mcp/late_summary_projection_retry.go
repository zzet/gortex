package mcp

import (
	"context"
	"errors"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/pathkey"
)

// A late native event is not proof that the selected bytes changed or stayed
// equal. A summary may earn the existing second attempt by checking the exact
// same current version itself, without relying on a concurrent peer refresh.
// This never validates or clears the original request's suspect owner.
func (s *Server) retryAfterLateSummaryProjectionChange(ctx context.Context, req mcplib.CallToolRequest, failure error) bool {
	if !errors.Is(failure, projectionSuperseded) || ctx.Err() != nil || requestReadsCommittedTree(ctx) {
		return false
	}
	legacy, known := s.legacyToolName(&req)
	allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	deadline, timed := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !known || legacy != "get_file_summary" || !ok || allowance.tool != legacy || !timed {
		return false
	}
	available := func() bool {
		if ctx.Err() != nil || allowance.attempts.Load() != 1 || !time.Now().Before(deadline) {
			return false
		}
		state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
		return state == nil || !state.replayUnsafe
	}
	if !available() {
		return false
	}
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return false
	}
	facts.mu.RLock()
	var entry *projectionEntry
	if len(facts.entries) == 1 {
		for _, selected := range facts.entries {
			entry = selected
		}
	}
	facts.mu.RUnlock()
	if entry == nil {
		return false
	}
	pendingFileProjections.mu.Lock()
	eligible := pendingFileProjections.snap.Load().entries[entry.key] == entry &&
		entry.sourceCommitSeq != 0 && (entry.commitSeq != 0 || entry.readVersionChecked) &&
		entry.suspect.Load() && entry.sourceEvents > 0 && !entry.checkedRetired.Load() &&
		!entry.tombstone && entry.layer != nil
	pendingFileProjections.mu.Unlock()
	if !eligible {
		return false
	}
	// The borrowed file identifies the leaf's actual selection through raw and
	// facade dispatch. Resolve it again within this request's selected checkout;
	// an equal graph path from another physical root is never a proof.
	abs, graphPath, err := s.resolveFilePath(ctx, entry.graphPath)
	if err != nil || graphPathKey(graphPath) != entry.graphPath || pathkey.MutationKey(abs) != entry.key {
		return false
	}
	idx, rel := s.indexerForRel(entry.graphPath)
	if idx == nil || !available() {
		return false
	}
	buildCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	checked, err := s.projectCurrentTrackedFileVersion(buildCtx, ctx, abs, entry.graphPath, rel, entry)
	if err != nil || checked == nil || !available() {
		return false
	}
	// Reuse the unchanged structural proof, including exact owner/event revision,
	// disk identity, physical source sequence and immutable layer equality.
	return retryAfterCheckedProjectionChange(ctx)
}
