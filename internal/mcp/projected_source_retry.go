package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

var errProjectedSourceMoved = errors.New("selected source bytes changed during the read")
var errProjectedSourceDeferred = errors.New("source is pending after the projection budget expired")

func invalidateProjectedSource(entry *projectionEntry) error {
	pendingFileProjections.invalidateCurrent(entry)
	return errProjectedSourceMoved
}

type projectedSourceDeadlineKey struct{}
type projectedSourceAllowanceKey struct{}
type projectedSourceAllowance struct {
	attempts       atomic.Int32
	tool           string
	diagnostic     *projectionPredicateTrace
	symbolMetadata bool // Bound once to the actual audited full-JSON request.
	// A plain require_fresh caller first waits on the original watcher bound.
	// Its projection window starts only afterward and is shared by rebounds.
	afterFreshness bool
	deadlineMu     sync.Mutex
	deadline       time.Time
	watchedWait    time.Duration
}

// Only these nonpaginated reads have audited replay-safe response work.
// Editing-context compression still uses its legacy physical source reader;
// it keeps the existing refusal behavior rather than gaining another attempt.
func auditedProjectionReadRetry(tool string, req mcplib.CallToolRequest) bool {
	switch tool {
	case "get_symbol_source", "get_file_summary":
		return true
	case "get_callers":
		freshness := takeRequestFreshness(&req)
		// Exact-view requests retain their existing freshness controller. A
		// plain fresh traversal can replay a checked retirement after its wait,
		// never a physical replacement or an unobserved owner transition.
		return freshness.err == nil && !freshness.requireExact && (freshness.requireFresh || !freshness.hasDeadline)
	case "get_symbol":
		// Full JSON assembly has no cursor/compression state. The handler
		// separately marks any active enrichment attempt replay-unsafe.
		return req.GetString("detail", "brief") == "full" && req.GetString("format", "json") == "json"
	case "get_editing_context":
		args := req.GetArguments()
		var options map[string]any
		if value, present := args["options"]; present {
			var valid bool
			options, valid = value.(map[string]any)
			if !valid {
				return false
			}
		}
		for _, values := range []map[string]any{args, options} {
			if value, present := values["compress_bodies"]; present {
				compressed, valid := value.(bool)
				if !valid || compressed {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func (s *Server) withProjectedSourceReadBudget(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		legacy, known := s.legacyToolName(&req)
		// Every symbol shape keeps its early projection-selection allowance.
		// Only full JSON with inert enrichment also qualifies after the leaf;
		// session bookkeeping is staged until the final acceptance check.
		if !known || !auditedProjectionReadRetry(legacy, req) && legacy != "get_symbol" {
			return next(ctx, req)
		}
		if _, exists := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance); !exists {
			freshness := takeRequestFreshness(&req)
			if legacy == "get_callers" && freshness.requireFresh {
				started := time.Now()
				ctx = withToolReceivedAt(ctx, started)
				// Latch the caller's wait ceiling once, rather than renewing it
				// when a complete attempt is rebound after checked retirement.
				originalDeadline := freshness.effectiveDeadline(started, ctx)
				ctx = context.WithValue(ctx, freshSymbolDeadlineKey{}, originalDeadline)
				diagnostic := newProjectionPredicateTrace()
				if diagnostic != nil {
					diagnostic.originalDeadline = originalDeadline
				}
				ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, &projectedSourceAllowance{tool: legacy, afterFreshness: true, diagnostic: diagnostic})
				return next(ctx, req)
			}
			deadline := time.Now().Add(projectedReadBudget)
			if prior, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && prior.Before(deadline) {
				deadline = prior
			}
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, &projectedSourceAllowance{tool: legacy, symbolMetadata: legacy == "get_symbol" && auditedProjectionReadRetry(legacy, req)})
		}
		return next(ctx, req)
	}
}

// The freshness wait is not charged against a projection window that did not
// exist yet. Once established, this same absolute ceiling covers both attempts;
// inherited projection/caller deadlines may shorten it, never extend it.
func projectedReadBudgetAfterFreshness(ctx context.Context) context.Context {
	allowance, _ := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	if allowance == nil || !allowance.afterFreshness {
		return ctx
	}
	allowance.deadlineMu.Lock()
	defer allowance.deadlineMu.Unlock()
	if allowance.deadline.IsZero() {
		deadline := time.Now().Add(projectedReadBudget)
		for _, key := range []any{projectedSourceDeadlineKey{}, freshSymbolDeadlineKey{}} {
			if prior, ok := ctx.Value(key).(time.Time); ok && prior.Before(deadline) {
				deadline = prior
			}
		}
		if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
			deadline = caller
		}
		allowance.deadline = deadline
	}
	return context.WithValue(ctx, projectedSourceDeadlineKey{}, allowance.deadline)
}

// Only observed time inside the watcher barrier is cumulative. In particular
// projection/handler work between attempts never becomes reported wait time.
func recordProjectedWatchedWait(ctx context.Context, waited time.Duration) time.Duration {
	allowance, _ := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	if allowance == nil || !allowance.afterFreshness {
		return waited
	}
	allowance.deadlineMu.Lock()
	defer allowance.deadlineMu.Unlock()
	allowance.watchedWait += waited
	return allowance.watchedWait
}

func (a *projectedSourceAllowance) claim() bool {
	for {
		before := a.attempts.Load()
		if before >= 2 {
			if a.diagnostic != nil {
				r := predicateSample("attempt.limit", nil, nil)
				r.decision = "exhausted"
				recordPredicateTrace(a.diagnostic, r, false)
			}
			return false
		}
		if a.attempts.CompareAndSwap(before, before+1) {
			if a.diagnostic != nil {
				a.diagnostic.attempt.Store(before + 1)
			}
			return true
		}
	}
}

// Permission for a fresh bounded attempt, never freshness of the old context.
// Checked canonical retirement and a checked same-version read-COW are distinct
// from another physical execution, content change or an unobserved source event.
func retryAfterCheckedProjectionChange(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	if !ok || allowance.tool == "get_symbol" && !allowance.symbolMetadata {
		return false
	}
	if state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt); state != nil && state.replayUnsafe {
		return false
	}
	deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !ok || !predicateDeadlineBefore(ctx, deadline, time.Now(), "recovery.entry_budget") {
		return false
	}
	return checkedProjectionChange(ctx) && ctx.Err() == nil && predicateDeadlineBefore(ctx, deadline, time.Now(), "recovery.final_budget")
}

// A file can retire during edge carry, before it enters the retained facts
// used by checkedProjectionChange. Give explicit-fresh callers the same whole
// rebound for that exact checked retirement without borrowing the old layer.
func retryAfterCheckedProjectionBindRetirement(ctx context.Context, entry *projectionEntry, guard projectionLivenessGuard) bool {
	if guard != projectionOwnerAbsentBefore && guard != projectionOwnerAbsentAfter {
		return false
	}
	allowance, _ := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	if allowance == nil || !allowance.afterFreshness || allowance.tool != "get_callers" || ctx.Err() != nil {
		return false
	}
	if state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt); state != nil && state.replayUnsafe {
		return false
	}
	deadline, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
	if !ok || !predicateDeadlineBefore(ctx, deadline, time.Now(), "recovery.bind_entry_budget") {
		return false
	}
	pendingFileProjections.mu.Lock()
	retired := pendingFileProjections.snap.Load().entries[entry.key] == nil && entry.checkedRetired.Load() && !entry.suspect.Load()
	pendingFileProjections.mu.Unlock()
	return retired && validateRequestProjections(ctx) == nil && ctx.Err() == nil &&
		predicateDeadlineBefore(ctx, deadline, time.Now(), "recovery.bind_final_budget")
}

// Structural ownership proof, independent of permission or time for another attempt.
func checkedProjectionChange(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	if !ok {
		return false
	}
	facts.mu.RLock()
	defer facts.mu.RUnlock()
	allowance, _ := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
	retirementOnly := allowance != nil && allowance.afterFreshness
	changed := false
	for _, entry := range facts.entries {
		if entry == nil || ctx.Err() != nil {
			return false
		}
		candidate := pendingFileProjections.snap.Load().entries[entry.key]
		// Stat remains outside the registry mutex. Under the mutex, bind the
		// observed disk check to the exact current pointer/event revision.
		live := candidate != nil && liveProjectionForAcknowledgement(candidate)
		sameDisk := candidate != nil && candidate.disk.matches(entry.disk)
		pendingFileProjections.mu.Lock()
		current := pendingFileProjections.snap.Load().entries[entry.key]
		unchanged := current == entry && current == candidate && live
		sample := predicateSample("recovery.checked_change", entry, current)
		sample.currentSeen = true
		retired := current == nil && predicateBool(&sample.retired, entry.checkedRetired.Load()) && !predicateBool(&sample.suspect, entry.suspect.Load())
		// A zero sequence never becomes a physical receipt. An externally
		// built version may authorize a new attempt only when BOTH owners
		// earned checked-read publication and share this immutable version.
		versionChecked := current != nil && current.sourceCommitSeq == entry.sourceCommitSeq &&
			(current.sourceCommitSeq != 0 || entry.commitSeq == 0 && current.readVersionChecked && entry.readVersionChecked)
		readCOW := current != nil && current != entry && current == candidate && live &&
			current.commitSeq == 0 && versionChecked &&
			!predicateBool(&sample.currentSuspect, current.suspect.Load()) && predicateEventZero(sample, current.sourceEvents) && !predicateBool(&sample.currentRetired, current.checkedRetired.Load()) &&
			!current.tombstone && !entry.tombstone && current.layer != nil && current.layer == entry.layer &&
			current.key == entry.key && current.graphPath == entry.graphPath && current.repoPrefix == entry.repoPrefix &&
			current.hash == entry.hash && current.rawHash == entry.rawHash && current.contentSHA256 == entry.contentSHA256 &&
			sameDisk
		pendingFileProjections.mu.Unlock()
		if !unchanged {
			sample.decision = "refused"
			if retired {
				sample.decision = "checked_retirement"
			} else if !retirementOnly && readCOW {
				sample.decision = "checked_read_cow"
			}
			recordPredicate(ctx, sample, false)
		}
		if unchanged {
			continue
		}
		if !retired && (retirementOnly || !readCOW) {
			return false
		}
		changed = true
	}
	return changed
}

// Audited reads retry once after a checked version transition; source retains
// its typed byte-mismatch retry. Every attempt has fresh facts/layers/leases
// under the original shared deadline and two-attempt allowance.
func (s *Server) retryProjectedSymbolSource(attempt mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (result *mcplib.CallToolResult, err error) {
		defer func() { attachPredicateDiagnostic(ctx, result) }()
		allowance, ok := ctx.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
		if !ok {
			return attempt(ctx, req)
		}
		for {
			if !allowance.claim() {
				return mcplib.NewToolResultError(errProjectedSourceMoved.Error()), nil
			}
			copy := req
			copy.Params.Arguments = cloneFreshSymbolArguments(req.GetArguments())
			attemptCtx, cancel := ctx, func() {}
			if allowance.afterFreshness {
				allowance.deadlineMu.Lock()
				deadline := allowance.deadline
				allowance.deadlineMu.Unlock()
				if !deadline.IsZero() {
					if !predicateDeadlineBefore(ctx, deadline, time.Now(), "attempt.rebound_budget") {
						return mcplib.NewToolResultError(errProjectedSourceMoved.Error()), nil
					}
					// A rebound's watcher barrier and assembly share the already
					// started ceiling too. The original caller remains unchanged.
					attemptCtx, cancel = context.WithDeadline(ctx, deadline)
				}
			}
			result, err := runProjectedSourceAttempt(attemptCtx, copy, attempt)
			cancel()
			if !errors.Is(err, errProjectedSourceMoved) {
				return result, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A caller-owned selection cannot be rebound by this nested invocation.
			if ctx.Value(requestProjectionFactsKey{}) != nil {
				return mcplib.NewToolResultError(errProjectedSourceMoved.Error()), nil
			}
		}
	}
}

func runProjectedSourceAttempt(ctx context.Context, req mcplib.CallToolRequest, attempt mcpserver.ToolHandlerFunc) (*mcplib.CallToolResult, error) {
	state, _ := ctx.Value(freshSymbolAttemptKey{}).(*freshSymbolAttempt)
	if state == nil {
		return runFreshSymbolAttempt(ctx, req, &freshSymbolAttempt{}, attempt)
	}
	accepted, finish := len(state.accepted), len(state.finish)
	result, err := attempt(ctx, req)
	if errors.Is(err, errProjectedSourceMoved) {
		state.accepted = state.accepted[:accepted]
		for _, release := range state.finish[finish:] {
			release()
		}
		state.finish = state.finish[:finish]
	}
	return result, err
}
