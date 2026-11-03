package mcp

import (
	"context"
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
	payload := map[string]any{"path": file, "complete": false}
	if legacy == "get_symbol_source" {
		payload["id"] = req.GetString("id", "")
		payload["source_state"] = "pending"
		payload["source_reason"] = "external_write"
	}
	freshness := map[string]any{}
	relationFreshness{state: "pending", scope: "file", files: []string{file}, fileReason: "external_write"}.apply(freshness)
	payload["freshness"] = freshness
	result, err := s.respondJSONOrTOON(ctx, req, payload)
	if err != nil {
		return nil, err
	}
	result = s.attachViewRider(ctx, result)
	result = s.attachCheckoutControlScope(ctx, result)
	return result, nil
}
