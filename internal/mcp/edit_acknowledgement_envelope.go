package mcp

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// The first actual middleware invocation owns these two outer boundaries.
// They do not include the leaf, whose parse/disk/admission/wait laps can repeat.
// Deferred view/repository/activity cleanup ends at the existing actual-handler
// endpoint, not at a newly registered defer which would run before that cleanup.
type editAcknowledgementEnvelope struct {
	observed                  bool
	leafStarted, leafFinished time.Time
}

func (s *Server) noteAcknowledgementMiddleware(ctx context.Context) {
	if a := editAcknowledgementFrom(ctx, s); a != nil {
		a.mu.Lock()
		a.envelope.observed = true
		a.mu.Unlock()
	}
}

func (s *Server) invokeAcknowledgedLeaf(ctx context.Context, req mcp.CallToolRequest, h mcpserver.ToolHandlerFunc) (*mcp.CallToolResult, error) {
	a := editAcknowledgementFrom(ctx, s)
	if a == nil || !a.beginLeaf() {
		return h(ctx, req) // reads/native/nested calls add no envelope clock or defer
	}
	return runAcknowledgedLeaf(a, ctx, req, h)
}

func runAcknowledgedLeaf(a *editAcknowledgement, ctx context.Context, req mcp.CallToolRequest, h mcpserver.ToolHandlerFunc) (*mcp.CallToolResult, error) {
	defer func() {
		a.mu.Lock()
		a.envelope.leafFinished = time.Now()
		a.closeEditControlLocked(a.envelope.leafFinished)
		a.mu.Unlock()
	}()
	return h(ctx, req)
}

func (a *editAcknowledgement) beginLeaf() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.envelope.observed || !a.envelope.leafStarted.IsZero() {
		return false
	}
	a.envelope.leafStarted = time.Now()
	a.laps.controlObserved = true
	a.openEditControlLocked(a.envelope.leafStarted)
	return true
}

func (e editAcknowledgementEnvelope) snapshot(entered, reply, actual time.Time) (editAcknowledgementLapSnapshot, editAcknowledgementLapSnapshot) {
	if !e.observed {
		return editAcknowledgementLapSnapshot{State: "unobserved"}, editAcknowledgementLapSnapshot{State: "unobserved"}
	}
	if e.leafStarted.IsZero() {
		// A refusal before the leaf is actual middleware work, not a fabricated
		// zero-duration leaf/reply phase. Pending work remains pending.
		middleware := acknowledgementEnvelopeInterval(entered, actual, entered, reply, actual)
		cleanup := editAcknowledgementLapSnapshot{State: "pending", Started: 1}
		if !actual.IsZero() {
			cleanup.State, cleanup.Completed, cleanup.NotApplicable = "not_applicable", 1, 1
		}
		return middleware, cleanup
	}
	middleware := acknowledgementEnvelopeInterval(entered, e.leafStarted, entered, reply, actual)
	cleanup := acknowledgementEnvelopeInterval(e.leafFinished, actual, entered, reply, actual)
	if e.leafStarted.Before(entered) || (!e.leafFinished.IsZero() && e.leafFinished.Before(e.leafStarted)) {
		middleware = editAcknowledgementLapSnapshot{State: "invalid_interval", Started: 1, Completed: 1}
		cleanup = middleware
	}
	return middleware, cleanup
}

func acknowledgementEnvelopeInterval(start, end, entered, reply, actual time.Time) editAcknowledgementLapSnapshot {
	out := editAcknowledgementLapSnapshot{Started: 1, State: "incomplete"}
	if start.IsZero() || end.IsZero() {
		return out
	}
	out.Completed = 1
	if entered.IsZero() || start.Before(entered) || end.Before(start) {
		out.State = "invalid_interval"
		return out
	}
	if !actual.IsZero() && end.After(actual) {
		out.State = "outside_execution_window"
		return out
	}
	ms := float64(end.Sub(start)) / float64(time.Millisecond)
	out.ObservedMS = &ms
	if reply.IsZero() {
		out.State = "pending_reply"
		return out
	}
	if reply.Before(entered) || end.After(reply) {
		out.State = "outside_reply_window"
		return out
	}
	ackMS := ms
	out.AckMS, out.State = &ackMS, "measured"
	out.ackNS, out.hasAckNS = end.Sub(start).Nanoseconds(), true
	return out
}
