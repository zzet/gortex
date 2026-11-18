package mcp

// PRIVATE diagnostic snapshot. No recovery authority is created here.
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"os"
	"reflect"
	"sync/atomic"
	"time"
)

// 0=not evaluated, 1=false, 2=true. Do not replace short-circuit predicates
// with eager observation: an unevaluated load is deliberately unavailable.
type projectionPredicateSample struct {
	site                                                               string
	selected, current                                                  uintptr
	selectedSeen, currentSeen                                          bool
	selectedSeq, selectedSourceSeq, currentSeq, currentSourceSeq       uint64
	retired, suspect, currentRetired, currentSuspect, candidateSuspect int8
	eventSeen, currentEventSeen                                        bool
	expectedEvent, currentEvent                                        uint64
	decision                                                           string
	budgetSite                                                         string
	attempt                                                            int32
	originalRemaining, sharedRemaining                                 *int64
}
type projectionPredicateLog struct {
	rows     [3]*projectionPredicateSample
	count    int
	overflow bool
}
type projectionPredicateTrace struct {
	first            atomic.Pointer[projectionPredicateSample]
	decisions        atomic.Pointer[projectionPredicateLog]
	attempt          atomic.Int32
	budget           atomic.Pointer[projectionPredicateSample]
	originalDeadline time.Time // copied when the actual freshness bound is latched
}

var projectionDiagnosticSalt = new(byte)

func projectionIdentity(p *projectionEntry) uintptr {
	if p == nil {
		return 0
	}
	return reflect.ValueOf(p).Pointer()
}
func predicateSample(site string, selected, current *projectionEntry) *projectionPredicateSample {
	r := &projectionPredicateSample{site: site, selected: projectionIdentity(selected), current: projectionIdentity(current), selectedSeen: selected != nil, currentSeen: current != nil}
	if selected != nil {
		r.selectedSeq, r.selectedSourceSeq = selected.commitSeq, selected.sourceCommitSeq
	}
	if current != nil {
		r.currentSeq, r.currentSourceSeq = current.commitSeq, current.sourceCommitSeq
	}
	return r
}
func predicateBool(dst *int8, value bool) bool {
	*dst = 1
	if value {
		*dst = 2
	}
	return value
}
func predicateEvents(r *projectionPredicateSample, expected, current uint64) bool {
	r.eventSeen = true
	r.expectedEvent, r.currentEvent = expected, current
	return predicateReason(r, "source_event_changed", current != expected)
}
func predicateReason(r *projectionPredicateSample, reason string, value bool) bool {
	if value {
		r.decision = reason
	}
	return value
}
func newProjectionPredicateTrace() *projectionPredicateTrace {
	if os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") != "1" {
		return nil
	}
	return &projectionPredicateTrace{}
}
func predicateTrace(ctx context.Context) *projectionPredicateTrace {
	a, _ := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	if a == nil {
		return nil
	}
	return a.diagnostic
}
func recordPredicate(ctx context.Context, r *projectionPredicateSample, refusal bool) {
	recordPredicateTrace(predicateTrace(ctx), r, refusal)
}
func recordPredicateTrace(t *projectionPredicateTrace, r *projectionPredicateSample, refusal bool) {
	if t == nil || r == nil {
		return
	}
	copy := *r
	r = &copy
	r.attempt = t.attempt.Load()
	if !refusal {
		if b := t.budget.Load(); b != nil {
			r.originalRemaining, r.sharedRemaining, r.budgetSite = b.originalRemaining, b.sharedRemaining, b.site
		}
	}
	if refusal {
		t.first.CompareAndSwap(nil, r)
		return
	}
	for {
		old := t.decisions.Load()
		next := &projectionPredicateLog{}
		if old != nil {
			*next = *old
		}
		if next.count == 3 {
			next.overflow = true
			next.rows[0], next.rows[1], next.rows[2] = next.rows[1], next.rows[2], r
		} else {
			next.rows[next.count] = r
			next.count++
		}
		if t.decisions.CompareAndSwap(old, next) {
			return
		}
	}
}
func predicateDeadlineBefore(ctx context.Context, deadline, now time.Time, site string) bool {
	before := now.Before(deadline)
	recordPredicateBudget(ctx, deadline, now, site, before)
	return before
}
func recordPredicateBudget(ctx context.Context, deadline, now time.Time, site string, before bool) {
	t := predicateTrace(ctx)
	if t == nil {
		return
	}
	shared := deadline.Sub(now).Nanoseconds()
	r := predicateSample(site, nil, nil)
	r.decision = "expired"
	if before {
		r.decision = "within_budget"
	}
	r.sharedRemaining = &shared
	if !t.originalDeadline.IsZero() {
		original := t.originalDeadline.Sub(now).Nanoseconds()
		r.originalRemaining = &original
	}
	t.budget.Store(r)
	if !before {
		recordPredicate(ctx, r, false)
	}
}
func predicateRefusal(ctx context.Context, stage projectionReadStage, entry *projectionEntry) (projectionLivenessGuard, *acknowledgementProjectionFailure) {
	guard, d := acknowledgementProjectionLiveness(entry)
	if guard != projectionOwnerLive && d != nil {
		r := predicateSample(stage.label(), nil, nil)
		r.selected, r.current = d.selectedIdentity, d.ownerIdentity
		r.selectedSeq, r.selectedSourceSeq = d.selectedSequence, d.selectedSourceSequence
		r.currentSeq, r.currentSourceSeq = d.ownerSequence, d.ownerSourceSequence
		r.selectedSeen = entry != nil
		r.currentSeen = d.OwnerCheck != "not_observed"
		r.suspect = d.observedSuspect
		r.decision = guard.label()
		recordPredicate(ctx, r, true)
	}
	return guard, d
}
func predicateError(site string, selected, current *projectionEntry, fill func(*projectionPredicateSample)) error {
	return predicateErrorEnabled(os.Getenv("GORTEX_SEARCH_REFUSAL_EVIDENCE") == "1", site, selected, current, fill)
}
func predicateErrorEnabled(enabled bool, site string, selected, current *projectionEntry, fill func(*projectionPredicateSample)) error {
	if !enabled {
		return projectionSuperseded
	}
	r := predicateSample(site, selected, current)
	if fill != nil {
		fill(r)
	}
	return &projectionReadDiagnostic{cause: projectionSuperseded, stage: projectionReadValidation, guard: projectionOwnerLive, predicate: r}
}
func retainPredicateError(ctx context.Context, err error) {
	if d, ok := err.(*projectionReadDiagnostic); ok && d.predicate != nil {
		recordPredicate(ctx, d.predicate, true)
	}
}
func predicateEventZero(r *projectionPredicateSample, value uint64) bool {
	r.currentEventSeen = true
	r.currentEvent = value
	return value == 0
}
func predicateID(id uintptr) string {
	if id == 0 {
		return "nil"
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%p:%x", projectionDiagnosticSalt, id)))
	return fmt.Sprintf("%x", sum[:8])
}
func predicateBoolean(v int8) any {
	if v == 0 {
		return "UNOBSERVED"
	}
	return v == 2
}
func renderPredicate(r *projectionPredicateSample) map[string]any {
	out := map[string]any{"site": r.site, "selected_id": "UNOBSERVED", "current_id": "UNOBSERVED", "checked_retired": predicateBoolean(r.retired), "suspect": predicateBoolean(r.suspect), "current_checked_retired": predicateBoolean(r.currentRetired), "current_suspect": predicateBoolean(r.currentSuspect), "candidate_suspect": predicateBoolean(r.candidateSuspect), "attempt": r.attempt, "decision": r.decision, "original_remaining_ns": "UNOBSERVED", "shared_remaining_ns": "UNOBSERVED", "events": "UNOBSERVED", "budget_sample_site": r.budgetSite}
	if r.selectedSeen {
		out["selected_id"], out["selected_seq"], out["selected_source_seq"] = predicateID(r.selected), r.selectedSeq, r.selectedSourceSeq
	}
	if r.currentSeen {
		out["current_id"], out["current_seq"], out["current_source_seq"] = predicateID(r.current), r.currentSeq, r.currentSourceSeq
	}
	if r.currentEventSeen {
		out["current_source_events"] = r.currentEvent
	}
	if r.originalRemaining != nil {
		out["original_remaining_ns"] = *r.originalRemaining
	}
	if r.sharedRemaining != nil {
		out["shared_remaining_ns"] = *r.sharedRemaining
	}
	if r.eventSeen {
		out["events"] = []uint64{r.expectedEvent, r.currentEvent}
	}
	return out
}
func attachPredicateDiagnostic(ctx context.Context, result *mcplib.CallToolResult) {
	if result == nil {
		return
	}
	t := predicateTrace(ctx)
	if t == nil {
		return
	}
	first := t.first.Load()
	log := t.decisions.Load()
	if first == nil && log == nil {
		return
	}
	var firstRendered any = "UNOBSERVED"
	if first != nil {
		firstRendered = renderPredicate(first)
	}
	out := map[string]any{"first_refusal": firstRendered, "recovery_samples": []any{}, "overflow": false, "sampling": "original checks only; recovery is a separate later sample"}
	if log != nil {
		rows := []any{}
		for i := 0; i < log.count; i++ {
			rows = append(rows, renderPredicate(log.rows[i]))
		}
		out["recovery_samples"] = rows
		out["overflow"] = log.overflow
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return
	}
	if len(encoded) > 2048 {
		out = map[string]any{"first_refusal": firstRendered, "recovery_samples": "TRUNCATED", "overflow": true}
	}
	encoded, err = json.Marshal(out)
	if err != nil || len(encoded) > 2048 {
		return
	}
	if result.Meta == nil {
		result.Meta = &mcplib.Meta{AdditionalFields: map[string]any{}}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = map[string]any{}
	}
	result.Meta.AdditionalFields["gortex_projection_predicate"] = out
}
