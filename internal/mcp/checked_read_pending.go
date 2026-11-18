package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/pathkey"
)

// This terminal reply contains no selected graph/source data. Only an externally
// built checked read version and an exact checked ownership transition qualify;
// ordinary physical peers, unchecked drops and events retain their refusal.
// Formatting pending does not claim another attempt or revive the private budget.
func (s *Server) checkedReadTransitionPending(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	deadline, hasDeadline := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	legacy, known := s.legacyToolName(&req)
	if !ok || !hasDeadline || !known || legacy != allowance.tool || !auditedProjectionReadRetry(legacy, req) || allowance.attempts.Load() < 2 && time.Now().Before(deadline) {
		return nil, nil
	}
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return nil, nil
	}
	facts.mu.RLock()
	var file, key string
	eligible := len(facts.entries) == 1
	for _, entry := range facts.entries {
		pendingFileProjections.mu.Lock()
		eligible = eligible && entry != nil && entry.commitSeq == 0 && entry.sourceCommitSeq == 0 && entry.readVersionChecked && !entry.tombstone && entry.layer != nil
		if entry != nil {
			file = entry.graphPath
			key = entry.key
		}
		pendingFileProjections.mu.Unlock()
	}
	facts.mu.RUnlock()
	if !eligible || file == "" || !checkedProjectionChange(ctx) {
		return nil, nil
	}
	// Exact path binding is retained even though all borrowed data is discarded.
	target := req.GetString("path", "")
	if legacy == "get_symbol_source" {
		id := req.GetString("id", "")
		var found bool
		target, _, found = strings.Cut(id, "::")
		if !found {
			return nil, nil
		}
	}
	abs, rel, resolveErr := s.resolveFilePath(ctx, target)
	if resolveErr != nil {
		return nil, ctx.Err()
	}
	if target == "" || graphPathKey(rel) != file || pathkey.MutationKey(abs) != key {
		return nil, nil
	}
	return s.projectionTransitionPendingResult(ctx, req, file, "external_write")
}

// A real source event can reach the exact queued storm after leaf selection.
// Only that typed failed selection may become a data-free pending reply. It
// receives no retry permission and cannot make any old fact valid again.
func (s *Server) lateStormTransitionPending(ctx context.Context, req mcplib.CallToolRequest, failure error) (*mcplib.CallToolResult, error) {
	if !errors.Is(failure, projectionSuperseded) {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	legacy, known := s.legacyToolName(&req)
	_, hasDeadline := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !ok || !known || !hasDeadline || legacy != "get_file_summary" || allowance.tool != legacy || allowance.attempts.Load() < 1 || allowance.attempts.Load() > 2 {
		return nil, nil
	}
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return nil, nil
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
		return nil, nil
	}
	// Existing registry ownership, immutable checked-read provenance and the
	// actual source-event revision distinguish this from an unchecked eviction,
	// another physical execution, or an arbitrary projection refusal.
	eligible := func() bool {
		facts.mu.RLock()
		defer facts.mu.RUnlock()
		pendingFileProjections.mu.Lock()
		defer pendingFileProjections.mu.Unlock()
		return len(facts.entries) == 1 && facts.entries[entry.key] == entry &&
			pendingFileProjections.snap.Load().entries[entry.key] == entry &&
			entry.commitSeq == 0 && entry.sourceCommitSeq == 0 && entry.readVersionChecked &&
			!entry.tombstone && entry.layer != nil && !entry.checkedRetired.Load() &&
			entry.suspect.Load() && entry.sourceEvents > 0
	}
	if !eligible() {
		return nil, nil
	}
	// Use the same fail-closed scoped resolver as the checked-retirement funnel,
	// never graphRelPath's unresolved-string fallback. No source bytes are read.
	target := req.GetString("path", "")
	abs, rel, resolveErr := s.resolveFilePath(ctx, target)
	if resolveErr != nil {
		return nil, ctx.Err()
	}
	if target == "" || graphPathKey(rel) != entry.graphPath || pathkey.MutationKey(abs) != entry.key {
		return nil, nil
	}
	if !s.projectionStormActive(abs) {
		return nil, ctx.Err()
	}
	// Watcher observation may block on its existing locks. A newer physical
	// publication or caller cancellation while observing it retains refusal.
	if !eligible() {
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := s.projectionTransitionPendingResult(ctx, req, entry.graphPath, "storm")
	if cancelled := ctx.Err(); cancelled != nil {
		return nil, cancelled
	}
	return result, err
}

func (s *Server) projectionTransitionPendingResult(ctx context.Context, req mcplib.CallToolRequest, file, reason string) (*mcplib.CallToolResult, error) {
	legacy, _ := s.legacyToolName(&req)
	payload := map[string]any{"path": file, "complete": false}
	if legacy == "get_symbol_source" {
		payload["id"] = req.GetString("id", "")
		payload["source_state"] = "pending"
		payload["source_reason"] = reason
	}
	freshness := map[string]any{}
	relationFreshness{state: "pending", scope: "file", files: []string{file}, fileReason: reason}.apply(freshness)
	payload["freshness"] = freshness
	result, err := s.respondJSONOrTOON(ctx, req, payload)
	if err != nil {
		return nil, err
	}
	result = s.attachViewRider(ctx, result)
	result = s.attachCheckoutControlScope(ctx, result)
	return result, nil
}
