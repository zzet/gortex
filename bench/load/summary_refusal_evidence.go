package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
)

// The selector combines the original plan request hash and a hash of the
// dispatched arguments and checkout path relative to the owned run root.
const (
	firstSummaryRefusalRequestEnv = "GORTEX_LOAD_FIRST_SUMMARY_REFUSAL_REQUEST"
	firstSummaryRefusalField      = "first_summary_projection_refusal"
)

type firstSummaryRefusalEvidenceKey struct{}

type firstSummaryRefusalEvidence struct {
	request string
	route   string
	root    string
	seen    atomic.Bool
}

var (
	summaryRefusalRequestHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
	summaryRefusalLabel       = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	summaryRefusalOwnerID     = regexp.MustCompile(`^(nil|[0-9a-f]{16})$`)
)

func withFirstSummaryRefusalEvidence(ctx context.Context, selection string) (context.Context, error) {
	if selection == "" {
		return ctx, nil
	}
	request, route, ok := strings.Cut(selection, ":")
	if !ok || !summaryRefusalRequestHash.MatchString(request) || !summaryRefusalRequestHash.MatchString(route) {
		return ctx, errors.New("first summary refusal evidence requires lowercase plan and checkout route SHA-256 digests")
	}
	return context.WithValue(ctx, firstSummaryRefusalEvidenceKey{}, &firstSummaryRefusalEvidence{request: request, route: route}), nil
}

func firstSummaryRefusalFrom(ctx context.Context) *firstSummaryRefusalEvidence {
	selected, _ := ctx.Value(firstSummaryRefusalEvidenceKey{}).(*firstSummaryRefusalEvidence)
	return selected
}

func summaryRefusalRouteHash(root, cwd string, args map[string]any) (string, error) {
	relative, err := filepath.Rel(root, cwd)
	if err != nil || !filepath.IsAbs(root) || !filepath.IsLocal(relative) || relative == "." {
		return "", errors.New("summary refusal route requires an owned checkout")
	}
	return hashJSON(map[string]any{"cwd": filepath.ToSlash(relative), "arguments": args})
}

func (s *firstSummaryRefusalEvidence) retain(attempt *Attempt, args map[string]any, raw json.RawMessage) bool {
	if s == nil || attempt.Tool != "read.summary" || args["operation"] != "summary" || attempt.Role != "workload" ||
		attempt.Request != s.request || attempt.Code != -32000 || s.seen.Load() {
		return false
	}
	data := attempt.ErrorData
	if !typedProjectionSupersededRefusal(data) {
		return false
	}
	route, err := summaryRefusalRouteHash(s.root, attempt.CWD, args)
	if err != nil || route != s.route || !s.seen.CompareAndSwap(false, true) {
		return false
	}
	trace := summaryRefusalTrace(raw)
	trace["route_sha256"] = route
	data[firstSummaryRefusalField] = trace
	return true
}

func typedProjectionSupersededRefusal(data map[string]any) bool {
	return data["error_code"] == "projection_superseded" && data["retry_scope"] == "read_only" &&
		data["retriable"] == true && data["delivery_unknown"] == false
}

// The same allowlisted projection, for every immediate edit-probe summary
// readback refused with the typed read-only projection_superseded shape. The
// daemon emits the predicate only under its refusal-evidence opt-in; without
// it the trace says unavailable. Each trace is bounded and holds no source.
func editProbeSummaryRefusalTrace(kind string, data map[string]any, raw json.RawMessage) map[string]any {
	if kind != "summary" || !typedProjectionSupersededRefusal(data) {
		return nil
	}
	return summaryRefusalTrace(raw)
}

func summaryRefusalTrace(raw json.RawMessage) map[string]any {
	state := func(value string) map[string]any { return map[string]any{"state": value} }
	if len(raw) > 4096 {
		return state("oversize")
	}
	var envelope struct {
		IsError bool `json:"isError"`
		Meta    struct {
			Projection json.RawMessage `json:"gortex_projection_predicate"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.IsError {
		return state("invalid")
	}
	if len(envelope.Meta.Projection) == 0 {
		return state("unavailable")
	}
	if len(envelope.Meta.Projection) > 2048 {
		return state("oversize")
	}
	var trace struct {
		First    json.RawMessage `json:"first_refusal"`
		Recovery json.RawMessage `json:"recovery_samples"`
		Overflow *bool           `json:"overflow"`
	}
	if err := json.Unmarshal(envelope.Meta.Projection, &trace); err != nil || trace.Overflow == nil {
		return state("invalid")
	}
	if string(trace.First) == `"UNOBSERVED"` {
		return state("unavailable")
	}
	first, ok := selectedSummaryPredicate(trace.First)
	if !ok {
		return state("invalid")
	}
	result := state("observed")
	result["first_refusal"] = first
	if *trace.Overflow {
		result["overflow"] = true
		if string(trace.Recovery) == `"TRUNCATED"` {
			return result
		}
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(trace.Recovery, &raws); err != nil || raws == nil || len(raws) > 3 {
		return state("invalid")
	}
	if len(raws) != 0 {
		recovery := make([]map[string]any, 0, len(raws))
		for _, raw := range raws {
			row, ok := selectedSummaryPredicate(raw)
			if !ok {
				return state("invalid")
			}
			recovery = append(recovery, row)
		}
		result["recovery_samples"] = recovery
	}
	return result
}

// Only project known scalar predicate operands. The result cannot retain
// source text, request arguments, unrelated metadata, or raw response bytes.
func selectedSummaryPredicate(raw json.RawMessage) (map[string]any, bool) {
	var fields map[string]json.RawMessage
	var site, decision string
	var attempt int
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["site"], &site) != nil ||
		json.Unmarshal(fields["decision"], &decision) != nil || json.Unmarshal(fields["attempt"], &attempt) != nil ||
		!summaryRefusalLabel.MatchString(site) || !summaryRefusalLabel.MatchString(decision) || attempt < 1 || attempt > 2 {
		return nil, false
	}
	result := map[string]any{"site": site, "decision": decision, "attempt": attempt}
	for _, field := range []string{"selected_id", "current_id"} {
		data, present := fields[field]
		if present && string(data) != `"UNOBSERVED"` {
			var id string
			if json.Unmarshal(data, &id) != nil || !summaryRefusalOwnerID.MatchString(id) {
				return nil, false
			}
			result[field] = id
		}
	}
	for _, field := range []string{"selected_source_seq", "current_source_seq", "current_source_events", "original_remaining_ns", "shared_remaining_ns"} {
		data, present := fields[field]
		if present && string(data) != `"UNOBSERVED"` {
			var value *int64
			if json.Unmarshal(data, &value) != nil || value == nil {
				return nil, false
			}
			result[field] = *value
		}
	}
	return result, true
}
