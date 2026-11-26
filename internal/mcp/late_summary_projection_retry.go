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
// It may also earn it when another editor's recorded tool commit replaced the
// selected owner and the disk still holds that commit's bytes; the second
// attempt selects the newer owner afresh and stays pending. Neither path
// validates or clears the original request's owner.
func (s *Server) retryAfterLateSummaryProjectionChange(ctx context.Context, req mcplib.CallToolRequest, failure error) bool {
	if !errors.Is(failure, projectionSuperseded) || ctx.Err() != nil || requestReadsCommittedTree(ctx) {
		return false
	}
	legacy, known := s.legacyToolName(&req)
	if !known || legacy != "get_file_summary" {
		return false
	}
	deadline, available, ok := summaryRetryWindow(ctx)
	if !ok {
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
	current := pendingFileProjections.snap.Load().entries[entry.key]
	eligible := current == entry &&
		entry.sourceCommitSeq != 0 && (entry.commitSeq != 0 || entry.readVersionChecked) &&
		entry.suspect.Load() && entry.sourceEvents > 0 && !entry.checkedRetired.Load() &&
		!entry.tombstone && entry.layer != nil
	newer := !eligible && entry.sourceCommitSeq != 0 && toolCommittedSummaryOwner(current) &&
		current.commitSeq > entry.sourceCommitSeq &&
		current.key == entry.key && current.graphPath == entry.graphPath && current.repoPrefix == entry.repoPrefix
	pendingFileProjections.mu.Unlock()
	if !eligible && !newer {
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
	if newer {
		return s.checkedNewerSummaryOwner(buildCtx, ctx, abs, entry.graphPath, rel, entry, current, available)
	}
	checked, err := s.projectCurrentTrackedFileVersion(buildCtx, ctx, abs, entry.graphPath, rel, entry)
	if err != nil || checked == nil || !available() {
		return false
	}
	// Reuse the unchanged structural proof, including exact owner/event revision,
	// disk identity, physical source sequence and immutable layer equality.
	return retryAfterCheckedProjectionChange(ctx)
}

// A summary that selected checked canonical bytes, with no owner, can lose
// them to another editor's tool commit while its store read times out. Those
// bytes never answer after the commit. When the commit is recorded and the
// disk still holds its bytes, the summary earns the existing second attempt,
// which selects the commit's owner and stays pending.
func (s *Server) retryAfterSummaryCanonicalToolCommit(ctx context.Context, proof *canonicalSummarySource) bool {
	if proof == nil || proof.registry == nil || proof.indexer == nil || ctx.Err() != nil || requestReadsCommittedTree(ctx) {
		return false
	}
	deadline, available, ok := summaryRetryWindow(ctx)
	if !ok || proof.registry.entries[proof.key] != nil {
		return false
	}
	pendingFileProjections.mu.Lock()
	current := pendingFileProjections.snap.Load().entries[proof.key]
	newer := toolCommittedSummaryOwner(current) &&
		current.key == proof.key && current.graphPath == proof.graphPath && current.absPath == proof.abs
	pendingFileProjections.mu.Unlock()
	if !newer {
		return false
	}
	idx, rel := s.indexerForRel(proof.graphPath)
	if idx != proof.indexer || !available() {
		return false
	}
	buildCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return s.checkedNewerSummaryOwner(buildCtx, ctx, proof.abs, proof.graphPath, rel, nil, current, available)
}

// summaryRetryWindow returns the shared deadline and a check that this first
// summary attempt may still earn the second one before that deadline.
func summaryRetryWindow(ctx context.Context) (time.Time, func() bool, bool) {
	allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	deadline, timed := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !ok || allowance.tool != "get_file_summary" || !timed {
		return time.Time{}, nil, false
	}
	available := func() bool {
		if ctx.Err() != nil || allowance.attempts.Load() != 1 || !time.Now().Before(deadline) {
			return false
		}
		state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
		return state == nil || !state.replayUnsafe
	}
	return deadline, available, available()
}

// toolCommittedSummaryOwner reports whether entry is a live owner published by
// a physical commit at its own sequence. A read-built copy, a checked
// retirement or a tombstone is not one. The caller holds the registry mutex.
func toolCommittedSummaryOwner(entry *projectionEntry) bool {
	return entry != nil && entry.commitSeq != 0 && entry.sourceCommitSeq == entry.commitSeq &&
		!entry.checkedRetired.Load() && !entry.tombstone && entry.layer != nil
}

// checkedNewerSummaryOwner accepts current only when a recorded tool commit
// published exactly it and the disk still holds its bytes. A watcher event
// on the owner is answered by the same bounded byte check, which publishes a
// checked copy of the same version. The checked owner must still be live.
func (s *Server) checkedNewerSummaryOwner(buildCtx, ctx context.Context, abs, graphPath, rel string, selected, current *projectionEntry, available func() bool) bool {
	sample := predicateSample("recovery.newer_tool_commit", selected, current)
	sample.decision = "refused"
	defer func() { recordPredicate(ctx, sample, false) }()
	if !s.mutationCommits.publishedProjection(current) {
		sample.decision = "unrecorded_owner"
		return false
	}
	checked, err := s.projectCurrentTrackedFileVersion(buildCtx, ctx, abs, graphPath, rel, current)
	if err != nil || checked == nil || !available() {
		return false
	}
	pendingFileProjections.mu.Lock()
	sameVersion := checked == current ||
		checked.commitSeq == 0 && checked.sourceCommitSeq == current.commitSeq && checked.layer == current.layer &&
			checked.rawHash == current.rawHash && checked.readVersionChecked
	owned := pendingFileProjections.snap.Load().entries[current.key] == checked && sameVersion
	pendingFileProjections.mu.Unlock()
	if !owned || !liveProjectionForAcknowledgement(checked) || !available() {
		return false
	}
	sample.decision = "newer_tool_commit"
	return true
}
