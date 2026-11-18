package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
)

// This is a call-scoped private observation, not retained receipt history. In
// particular it must not be stored in the unbounded batchTransactions map.
// Its origin precedes boundHandler, not the dispatcher/transport or the legacy
// publication origin at middleware entry. No graph work is inferred from it.
type editAcknowledgement struct {
	owner       *Server
	id          uint64
	entered     time.Time
	mu          sync.Mutex
	actual      time.Time
	reply       time.Time
	outcome     string
	laps        editAcknowledgementLaps
	envelope    editAcknowledgementEnvelope
	dispatch    daemon.MCPDispatchTiming
	hasDispatch bool
	parserScope string
}

type editAcknowledgementKey struct{}

var editAcknowledgementSequence atomic.Uint64

// A status call may borrow this pointer long enough to copy a snapshot. It
// never installs one: eviction clears the record's ownership even when other
// pending-source evidence still retains the physical commit record.
func retainedMutationAcknowledgement(record *mutationCommitRecord) *editAcknowledgement {
	if !record.isPhysicalCommit() {
		return nil
	}
	record.mu.RLock()
	defer record.mu.RUnlock()
	return record.acknowledgement
}

// The two walls deliberately overlap: they are diagnostic alternatives, never
// additive laps. A detached handler can finish after the reply wall froze.
// Snapshot values own their scalar pointers; no encoder samples a live clock.
type editAcknowledgementSnapshot struct {
	RPCID                uint64
	Scope                string
	ReplyWallMS          *float64
	ReplyState           string
	ReplyOutcome         string
	ActualWallMS         *float64
	ActualState          string
	ActualFinishedLate   bool
	DispatcherQueueMS    *float64
	DispatcherQueueState string
	DispatcherPreambleMS *float64
	DispatcherState      string
	DispatcherReplyMS    *float64
	DispatcherReplyState string
	ParseGate            editAcknowledgementLapSnapshot
	DiskWrite            editAcknowledgementLapSnapshot
	TicketAdmission      editAcknowledgementLapSnapshot
	ReindexWait          editAcknowledgementLapSnapshot
	Middleware           editAcknowledgementLapSnapshot
	ReplyPrepare         editAcknowledgementLapSnapshot
	EditControl          editAcknowledgementLapSnapshot
	CallerResult         editAcknowledgementLapSnapshot
	ParserScope          string
	LapsDisjoint         bool
	replyNS              int64
	hasReplyNS           bool
}

func editAcknowledgementFrom(ctx context.Context, owner *Server) *editAcknowledgement {
	if ctx == nil {
		return nil
	}
	ack, _ := ctx.Value(editAcknowledgementKey{}).(*editAcknowledgement)
	if ack != nil && ack.owner == owner {
		return ack
	}
	return nil
}

// withEditAcknowledgement is called only after the existing measured-source
// classifier succeeded. A same-Server nested raw/facade call borrows the RPC
// identity; another Server cannot complete or freeze that owner's record.
func (s *Server) withEditAcknowledgement(ctx context.Context) (context.Context, *editAcknowledgement, bool) {
	if existing := editAcknowledgementFrom(ctx, s); existing != nil {
		return ctx, existing, false
	}
	dispatch, hasDispatch := daemon.MCPDispatchTimingForContext(ctx)
	ack := &editAcknowledgement{owner: s, id: editAcknowledgementSequence.Add(1), entered: time.Now(), dispatch: dispatch, hasDispatch: hasDispatch}
	return context.WithValue(ctx, editAcknowledgementKey{}, ack), ack, true
}

func (a *editAcknowledgement) finishActual() {
	a.finishActualAt(time.Now())
}

func (a *editAcknowledgement) finishActualAt(at time.Time) {
	a.mu.Lock()
	if a.actual.IsZero() {
		a.actual = at
	}
	a.mu.Unlock()
}

func (a *editAcknowledgement) finishReply(result *mcp.CallToolResult, err error) {
	outcome := "result"
	switch {
	case err != nil:
		outcome = "go_error"
	case result == nil:
		outcome = "no_result"
	case result.IsError:
		outcome = "tool_error"
	}
	a.finishReplyAt(time.Now(), outcome)
}

func (a *editAcknowledgement) finishReplyAt(at time.Time, outcome string) {
	a.mu.Lock()
	if a.reply.IsZero() {
		a.reply, a.outcome = at, outcome
	}
	a.mu.Unlock()
}

func (a *editAcknowledgement) snapshot() editAcknowledgementSnapshot {
	a.mu.Lock()
	actual, reply, outcome, laps, envelope, parserScope := a.actual, a.reply, a.outcome, a.laps, a.envelope, a.parserScope
	a.mu.Unlock()
	result := editAcknowledgementSnapshot{
		RPCID: a.id, Scope: "mcp_bound_call", ReplyOutcome: outcome,
		DispatcherQueueState: "unavailable_dispatcher_interface",
		DispatcherState:      "unavailable_dispatcher_interface",
		DispatcherReplyState: "unavailable_dispatcher_interface",
	}
	result.ReplyWallMS, result.ReplyState = editAcknowledgementWall(a.entered, reply)
	if result.ReplyState == "measured" {
		result.replyNS, result.hasReplyNS = reply.Sub(a.entered).Nanoseconds(), true
	}
	result.ActualWallMS, result.ActualState = editAcknowledgementWall(a.entered, actual)
	result.ActualFinishedLate = result.ReplyState == "measured" && result.ActualState == "measured" && actual.After(reply)
	result.ParseGate = laps.snapshot(editAckParseGate, a.entered, reply, actual)
	result.ParserScope = parserScope
	if result.ParseGate.Started == 0 && (parserScope == "not_applicable_text_splice" || parserScope == "unsupported_non_go_move") {
		result.ParseGate.State = "not_applicable"
	}
	result.DiskWrite = laps.snapshot(editAckDiskWrite, a.entered, reply, actual)
	result.TicketAdmission = laps.snapshot(editAckTicketAdmission, a.entered, reply, actual)
	result.ReindexWait = laps.snapshot(editAckReindexWait, a.entered, reply, actual)
	result.EditControl = laps.snapshot(editAckLeafControl, a.entered, reply, actual)
	result.CallerResult = acknowledgementCallerResult(a.entered, actual, reply)
	result.LapsDisjoint = !laps.overlap
	if a.hasDispatch {
		if a.dispatch.Granted.Before(a.dispatch.Entered) || a.entered.Before(a.dispatch.Granted) {
			result.DispatcherQueueState, result.DispatcherState = "invalid_interval", "invalid_interval"
			result.DispatcherReplyState = "invalid_interval"
		} else {
			result.DispatcherQueueMS, result.DispatcherQueueState = editAcknowledgementWall(a.dispatch.Entered, a.dispatch.Granted)
			result.DispatcherPreambleMS, result.DispatcherState = editAcknowledgementWall(a.dispatch.Granted, a.entered)
			result.DispatcherReplyMS, result.DispatcherReplyState = editAcknowledgementWall(a.dispatch.Entered, reply)
		}
	}
	result.Middleware, result.ReplyPrepare = envelope.snapshot(a.entered, reply, actual)
	// These outer intervals surround, rather than contain, the leaf's fixed
	// operation laps. A misplaced operation must not produce a plausible sum.
	if envelope.observed {
		for _, lap := range laps.data {
			if lap.started > 0 && (envelope.leafStarted.IsZero() || lap.first.Before(envelope.leafStarted) ||
				(!envelope.leafFinished.IsZero() && lap.last.After(envelope.leafFinished))) {
				result.LapsDisjoint = false
			}
		}
	}
	return result
}

func editAcknowledgementWall(start, end time.Time) (*float64, string) {
	if start.IsZero() {
		return nil, "unavailable_origin"
	}
	if end.IsZero() {
		return nil, "pending"
	}
	if end.Before(start) {
		return nil, "invalid_order"
	}
	ms := float64(end.Sub(start)) / float64(time.Millisecond)
	return &ms, "measured"
}
